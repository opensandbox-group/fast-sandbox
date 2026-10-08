package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
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
		path, err := imageCachePath(root, image)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("rootfs"), 0o640))
		return imageKey(image)
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
		Config: fastletapi.RuntimeSandboxConfig{Spec: fastletapi.SandboxSpec{Image: "example.com/used:v1"}, Identity: fastletapi.SandboxIdentity{SandboxUID: "sbx-1"}},
		Phase:  PhaseRunning,
	}))

	// Unknown content in the cache root is never collected.
	junk := filepath.Join(root, imageCacheDir, "not-a-digest")
	require.NoError(t, os.MkdirAll(junk, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(junk, "rootfs.img"), []byte("junk"), 0o640))

	// The limit fits exactly the referenced image (6 bytes): everything
	// unreferenced is evicted, hot last because it is the most frequently
	// used.
	removed, err := garbageCollectImages(root, 6, useCount)
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
	removed, err = garbageCollectImages(root, 12, useCount)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{unknown, cold}, removed)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "high-frequency image must survive eviction")

	// Unreadable Sandbox state aborts the collection entirely.
	second, err := ensureSandboxDir(root, "sbx-2")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath(second), []byte("{corrupt"), 0o600))
	_, err = garbageCollectImages(root, 12, useCount)
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "collection must not remove anything when a state cannot be read")

	// After the referencing Sandbox is deleted the image becomes evictable;
	// the lower-frequency one goes first.
	require.NoError(t, removeSandboxDir(directory))
	require.NoError(t, os.Remove(metaPath(second)))
	removed, err = garbageCollectImages(root, 6, useCount)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{used}, removed)
	_, err = os.Stat(filepath.Join(root, imageCacheDir, hot))
	require.NoError(t, err, "high-frequency image must survive eviction")
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
	driver.imageCacheLimitBytes = 6
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
