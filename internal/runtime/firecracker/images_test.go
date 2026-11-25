package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestImageCachePathIsContentAddressed(t *testing.T) {
	root := t.TempDir()
	first, err := imageCachePath(root, "example.com/app:v1")
	require.NoError(t, err)
	second, err := imageCachePath(root, "example.com/app@sha256:deadbeef")
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.Equal(t, filepath.Join(root, imageCacheDir, imageKey("example.com/app:v1"), rootfsImageName), first)

	_, err = imageCachePath(root, "")
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestResolveRootfsImage(t *testing.T) {
	root := t.TempDir()
	image := "example.com/app:v1"

	_, err := resolveRootfsImage(root, image)
	require.True(t, errors.Is(err, ErrImageNotReady))

	path, err := imageCachePath(root, image)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("rootfs"), 0o640))

	resolved, err := resolveRootfsImage(root, image)
	require.NoError(t, err)
	require.Equal(t, path, resolved)
}

func TestLocalCacheRejectsIncompleteManifest(t *testing.T) {
	root, image := t.TempDir(), "example.com/legacy-overlaybd:v1"
	path, err := imageCachePath(root, image)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0750))
	for _, name := range []string{rootfsImageName, vmstateSnapshotName, memorySnapshotName} {
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte(name), 0640))
	}
	manifest := cachedManifestPath(root, image)
	require.NoError(t, os.WriteFile(manifest, []byte(`{"format":"overlaybd"}`), 0640))
	require.ErrorIs(t, verifyRestorableImage(root, image), ErrInvalidConfig)
	driver := &Driver{config: firecrackerConfigForTest(t, root)}
	require.ErrorIs(t, driver.PullImage(context.Background(), image), ErrInvalidConfig)

	// The supported native cache remains usable without a runtime-agent.
	raw, err := json.Marshal(testManifestDocument(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, raw, 0640))
	require.NoError(t, verifyRestorableImage(root, image))
	require.NoError(t, driver.PullImage(context.Background(), image))
}

func TestListCachedImagesAndDriverSurfaces(t *testing.T) {
	root := t.TempDir()
	driver := &Driver{config: firecrackerConfigForTest(t, root)}

	images, err := driver.ListImages(context.Background())
	require.NoError(t, err)
	require.Empty(t, images)

	require.True(t, errors.Is(driver.PullImage(context.Background(), "example.com/missing:v1"), ErrImageNotReady))

	path, err := imageCachePath(root, "example.com/app:v1")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("rootfs"), 0o640))
	require.NoError(t, driver.PullImage(context.Background(), "example.com/app:v1"))

	images, err = driver.ListImages(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{imageKey("example.com/app:v1")}, images)
}

func TestGarbageCollectImages(t *testing.T) {
	root := t.TempDir()
	useCount := make(map[string]int64)
	seedImage := func(image string) string {
		return seedCommittedImage(t, root, image)
	}
	// hot and cold are both unreferenced; the LFU order evicts cold first.
	hot := seedImage("example.com/hot:v1")
	cold := seedImage("example.com/cold:v1")
	useCount[hot] = 100
	useCount[cold] = 1
	// unknown has no recorded use: it counts as zero and is the first victim.
	unknown := seedImage("example.com/unknown:v1")

	// A managed Sandbox pins its image through its durable state.
	used := seedImage("example.com/used:v1")
	useCount[used] = 5
	directory, err := ensureSandboxDir(root, "sbx-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Config: fastletapi.RuntimeSandboxConfig{Desired: fastletapi.SandboxDesiredSpec{Image: "example.com/used:v1"}, Identity: fastletapi.SandboxIdentity{SandboxUID: "sbx-1"}},
		Phase:  PhaseRunning,
	}))

	// Unknown content in the cache root is never collected.
	junk := filepath.Join(root, imageCacheDir, "not-a-digest")
	require.NoError(t, os.MkdirAll(junk, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(junk, "rootfs.img"), []byte("junk"), 0o640))

	// The limit fits exactly the referenced image: everything unreferenced
	// is evicted, hot last because it is the most frequently used.
	unit := seededImageBytes(t, root, used)
	removed, err := garbageCollectImages(root, unit, useCount, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{unknown, cold, hot}, removed)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, used))
	require.NoError(t, err, "referenced image must survive GC")
	_, err = os.Stat(junk)
	require.NoError(t, err, "non-digest entries must be left alone")

	// With a limit of two images the least frequently used are evicted first
	// and the hot image survives.
	cold = seedImage("example.com/cold:v1")
	unknown = seedImage("example.com/unknown:v1")
	hot = seedImage("example.com/hot:v1")
	removed, err = garbageCollectImages(root, 2*unit, useCount, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{unknown, cold}, removed)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "high-frequency image must survive eviction")

	// Unreadable Sandbox state aborts the collection entirely.
	second, err := ensureSandboxDir(root, "sbx-2")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath(second), []byte("{corrupt"), 0o600))
	_, err = garbageCollectImages(root, 2*unit, useCount, nil)
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "collection must not remove anything when a state cannot be read")

	// After the referencing Sandbox is deleted the image becomes evictable;
	// the lower-frequency one goes first.
	require.NoError(t, removeSandboxDir(directory))
	require.NoError(t, os.Remove(metaPath(second)))
	removed, err = garbageCollectImages(root, unit, useCount, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{used}, removed)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "high-frequency image must survive eviction")
}

// TestGarbageCollectImagesPinsRestoreCheckpoint covers a Sandbox resumed from
// a pause checkpoint: its later snapshots read the checkpoint's cached
// manifest as the source image, so the GC must keep that entry as well as the
// template image.
func TestGarbageCollectImagesPinsRestoreCheckpoint(t *testing.T) {
	root := t.TempDir()
	seed := func(reference string) string {
		return seedCommittedImage(t, root, reference)
	}
	digest := strings.Repeat("ab", 32)
	template := seed("tpl-resumed")
	checkpoint := seed(artifacts.CheckpointReference(digest))
	other := seed("example.com/other:v1")

	directory, err := ensureSandboxDir(root, "sbx-resumed")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Config: fastletapi.RuntimeSandboxConfig{
			Desired: fastletapi.SandboxDesiredSpec{Image: "tpl-resumed", Restore: &fastletapi.RestoreSpec{
				ManifestRef: "s3://bucket/publish/abababababababab/manifest.json", ArtifactDigest: digest,
			}},
			Identity: fastletapi.SandboxIdentity{SandboxUID: "sbx-resumed"},
		},
		Phase: PhaseRunning,
	}))

	// The limit fits only one entry: every unpinned entry goes, the pinned
	// ones stay even though the cache remains over the limit.
	removed, err := garbageCollectImages(root, seededImageBytes(t, root, other), map[string]int64{}, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{other}, removed)
	for _, pinned := range []string{template, checkpoint} {
		_, err = os.Stat(filepath.Join(root, imageCacheDir, pinned))
		require.NoError(t, err, "the template and the restore checkpoint of a live Sandbox must survive GC")
	}
}

// TestImageGCLoopRunsIndependently verifies the cache GC is driven by its own
// periodic loop, not by Sandbox lifecycle events, and stops on Close.
func TestImageGCLoopRunsIndependently(t *testing.T) {
	root := t.TempDir()
	config := firecrackerConfigForTest(t, root)
	driver, err := New(runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeFirecracker, Firecracker: &config,
		Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady},
	})
	require.NoError(t, err)
	driver.imageGCInterval = 30 * time.Millisecond
	driver.checkStorage = func(context.Context, string) error { return nil }
	require.NoError(t, driver.Initialize(context.Background(), ""))
	t.Cleanup(func() { _ = driver.Close() })

	seedImage := func(image string) string {
		path, err := imageCachePath(root, image)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("rootfs"), 0o640))
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "manifest.json"), []byte("{}"), 0o640))
		digest := imageKey(image)
		// The GC loop reads the map concurrently; mutate under the lock.
		driver.mu.Lock()
		driver.imageUseCount[digest] = 1
		driver.mu.Unlock()
		return digest
	}
	waitFor := func(image string, gone bool) {
		t.Helper()
		path := filepath.Join(root, imageCacheDir, image)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			_, err := os.Stat(path)
			if (err != nil && gone) || (err == nil && !gone) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		require.Failf(t, "timeout waiting for image %s (gone=%v)", image, gone)
	}

	// The cache is over its limit, so the loop evicts the unreferenced
	// low-frequency image without any Sandbox event.
	driver.mu.Lock()
	driver.imageCacheLimitBytes = 1
	driver.mu.Unlock()
	unused := seedImage("example.com/unused:v1")
	waitFor(unused, true)

	// Under its limit again, the loop leaves images alone even when
	// unreferenced.
	driver.mu.Lock()
	driver.imageCacheLimitBytes = 1 << 30
	driver.mu.Unlock()
	hot := seedImage("example.com/hot:v1")
	driver.mu.Lock()
	driver.imageUseCount[hot] = 100
	driver.mu.Unlock()
	time.Sleep(3 * driver.imageGCInterval)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "image under the cache limit must survive the loop")

	// Close stops the loop: an image added afterwards is left alone.
	require.NoError(t, driver.Close())
	t.Cleanup(func() {}) // Close already ran
	after := seedImage("example.com/after:v1")
	time.Sleep(3 * driver.imageGCInterval)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, after))
	require.NoError(t, err, "loop must stop when the driver is closed")
}

// Native cold boots need only rootfs and declared platform files, but the
// manifest remains the commit point and every declared digest must match.
func TestNativeRootfsDeliveryRequiresCommittedCompleteCache(t *testing.T) {
	root, image := t.TempDir(), "native-rootfs"
	dir := filepath.Dir(cachedManifestPath(root, image))
	require.NoError(t, os.MkdirAll(dir, 0700))
	disk, policy := []byte("native-disk"), []byte(`{"version":1}`)
	manifest := newMetadataTestVMManifest()
	manifest.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly", StateFiles: map[string]artifacts.ManifestFile{
		"policy.json": {SHA256: artifacts.SHA256Of(policy), SizeBytes: int64(len(policy))},
	}}
	manifest.Kernel.Cmdline = "root=/dev/vda rw"
	manifest.Files = map[string]artifacts.ManifestFile{
		"rootfs.ext4": {SHA256: artifacts.SHA256Of(disk), SizeBytes: int64(len(disk))},
	}
	raw, err := artifacts.MarshalVMManifest(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), disk, 0600))
	require.ErrorIs(t, verifyDeliveredImage(root, image, true), ErrImageNotReady)

	// A committed manifest must not hide a missing declared platform file.
	require.NoError(t, os.WriteFile(cachedManifestPath(root, image), raw, 0600))
	require.ErrorIs(t, verifyDeliveredImage(root, image, true), ErrImageNotReady)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "policy.json"), policy, 0600))
	require.NoError(t, verifyDeliveredImage(root, image, true))
	require.NoFileExists(t, filepath.Join(dir, vmstateSnapshotName))
	require.NoFileExists(t, filepath.Join(dir, memorySnapshotName))

	// Size-only checks cannot detect a corrupt native disk or policy bundle.
	for name, content := range map[string][]byte{rootfsImageName: disk, "policy.json": policy} {
		t.Run(name, func(t *testing.T) {
			corrupt := append([]byte(nil), content...)
			corrupt[0] ^= 1
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), corrupt, 0600))
			require.ErrorIs(t, verifyDeliveredImage(root, image, true), ErrImageNotReady)
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), content, 0600))
			require.NoError(t, verifyDeliveredImage(root, image, true))
		})
	}
}

func TestCopyReflinkOrCopyClonesAndIsolates(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	dst := filepath.Join(dir, "dst.img")
	original := []byte("firecracker-rootfs-clone-test-payload")
	require.NoError(t, os.WriteFile(src, original, 0o600))

	require.NoError(t, copyReflinkOrCopy(src, dst))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, original, got)

	// Mutating the clone must never mutate the source.
	mutated := append([]byte(nil), original...)
	mutated[0] ^= 0xff
	require.NoError(t, os.WriteFile(dst, mutated, 0o600))
	stillOriginal, err := os.ReadFile(src)
	require.NoError(t, err)
	require.Equal(t, original, stillOriginal)
}

// seedCommittedImage writes a cache entry with its commit-point manifest, as
// the runtime-agent leaves a completed pull.
func seedCommittedImage(t *testing.T, root, reference string) string {
	t.Helper()
	path, err := imageCachePath(root, reference)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("rootfs"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "manifest.json"), []byte("{}"), 0o640))
	return imageKey(reference)
}

// seededImageBytes is the allocated size the GC accounts for one entry.
func seededImageBytes(t *testing.T, root, key string) int64 {
	t.Helper()
	usage, err := imageDirUsage(filepath.Join(root, imageCacheDir, key))
	require.NoError(t, err)
	require.Positive(t, usage.allocated)
	return usage.allocated
}
