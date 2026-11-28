package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
)

func writeTestPins(t *testing.T, root, owner string, updatedAt time.Time, keys ...string) {
	t.Helper()
	require.NoError(t, writeImagePinFile(root, owner, imagePinFile{UpdatedAt: updatedAt, Images: keys}))
}

func cacheEntryExists(root, key string) bool {
	_, err := os.Stat(filepath.Join(root, imageCacheDir, key))
	return err == nil
}

// A template warmed by another Pool's Fastlet on the same node has no Sandbox
// referencing it; its fresh pin file must still keep it out of this
// Fastlet's GC.
func TestGarbageCollectKeepsImagesPinnedByAnotherFastlet(t *testing.T) {
	root := t.TempDir()
	warmedElsewhere := seedCommittedImage(t, root, "tpl-other-pool")
	unpinned := seedCommittedImage(t, root, "tpl-unused")
	writeTestPins(t, root, "pod-other", time.Now(), warmedElsewhere)

	pinned, err := loadNodeImagePins(root, time.Now(), imagePinTTL(10*time.Minute))
	require.NoError(t, err)
	removed, err := garbageCollectImages(root, 0, map[string]int64{}, pinned)
	require.NoError(t, err)
	require.Equal(t, []string{unpinned}, removed)
	require.True(t, cacheEntryExists(root, warmedElsewhere))
}

// The pin file of a Fastlet that stopped refreshing it (gone without Close)
// stops protecting images after the TTL.
func TestStaleImagePinsAreIgnored(t *testing.T) {
	root := t.TempDir()
	image := seedCommittedImage(t, root, "tpl-abandoned")
	writeTestPins(t, root, "pod-gone", time.Now().Add(-2*time.Hour), image)
	require.NoError(t, os.WriteFile(filepath.Join(root, imagePinDir, "garbage.json"), []byte("{"), 0o640))

	pinned, err := loadNodeImagePins(root, time.Now(), imagePinTTL(10*time.Minute))
	require.NoError(t, err)
	require.Empty(t, pinned)
	removed, err := garbageCollectImages(root, 0, map[string]int64{}, pinned)
	require.NoError(t, err)
	require.Equal(t, []string{image}, removed)
}

// A directory without a committed manifest is a pull in progress (the agent
// writes manifest.json last) until it has been idle for the grace window.
func TestGarbageCollectSkipsPullsInProgress(t *testing.T) {
	root := t.TempDir()
	path, err := imageCachePath(root, "tpl-pulling")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("partial"), 0o640))
	pulling := imageKey("tpl-pulling")

	removed, err := garbageCollectImages(root, 0, map[string]int64{}, nil)
	require.NoError(t, err)
	require.Empty(t, removed)
	require.True(t, cacheEntryExists(root, pulling))

	stale := time.Now().Add(-2 * imagePullGraceWindow)
	require.NoError(t, os.Chtimes(path, stale, stale))
	removed, err = garbageCollectImages(root, 0, map[string]int64{}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{pulling}, removed, "an abandoned partial pull is collectable")
}

// Sparse rootfs and memory files count the blocks they occupy, not their
// logical size: a 4 GiB sparse file must not push the cache over its limit.
func TestImageDirUsageCountsAllocatedBytes(t *testing.T) {
	root := t.TempDir()
	key := seedCommittedImage(t, root, "tpl-sparse")
	sparse, err := os.OpenFile(filepath.Join(root, imageCacheDir, key, memorySnapshotName), os.O_CREATE|os.O_WRONLY, 0o640)
	require.NoError(t, err)
	require.NoError(t, sparse.Truncate(4<<30))
	require.NoError(t, sparse.Close())

	usage, err := imageDirUsage(filepath.Join(root, imageCacheDir, key))
	require.NoError(t, err)
	require.True(t, usage.committed)
	require.Equal(t, 3, usage.files)
	require.Less(t, usage.allocated, int64(16<<20))
}

// Warm images pinned through PullImage and recently used images are
// published under the Fastlet's owner name; Close removes the file.
func TestDriverPublishesAndRemovesImagePins(t *testing.T) {
	root := t.TempDir()
	driver := &Driver{}
	driver.config.StateRoot = root
	driver.SetFastletPodUID("pod-a")

	driver.pinImage("tpl-warm")
	driver.noteImageUse("tpl-recent")
	driver.publishImagePins(true)

	payload, err := os.ReadFile(imagePinPath(root, "pod-a"))
	require.NoError(t, err)
	var pins imagePinFile
	require.NoError(t, json.Unmarshal(payload, &pins))
	require.ElementsMatch(t, []string{imageKey("tpl-warm"), imageKey("tpl-recent")}, pins.Images)
	require.WithinDuration(t, time.Now(), pins.UpdatedAt, time.Minute)

	driver.mu.Lock()
	driver.recentImages[imageKey("tpl-recent")] = time.Now().Add(-2 * imageRecentUseWindow)
	driver.mu.Unlock()
	_, _, keys := driver.currentImagePins(time.Now())
	require.Equal(t, []string{imageKey("tpl-warm")}, keys, "recent uses expire, warm pins stay")

	driver.mu.Lock()
	driver.removeImagePinsLocked()
	driver.mu.Unlock()
	_, err = os.Stat(imagePinPath(root, "pod-a"))
	require.True(t, os.IsNotExist(err))
	driver.pinImage("tpl-after-close")
	_, err = os.Stat(imagePinPath(root, "pod-a"))
	require.True(t, os.IsNotExist(err), "nothing is published after Close")
}

// The agent journal replays a committed request id without pulling. When
// that leaves the cache entry missing, pinVerified retries once under a fresh
// request id, which makes the agent pull again.
func TestPinVerifiedRepinsWhenCacheEntryWasEvicted(t *testing.T) {
	root := t.TempDir()
	driver := &Driver{}
	driver.config.StateRoot = root
	driver.SetFastletPodUID("pod-a")
	const image = "tpl-evicted"

	var requestIDs []string
	err := driver.pinVerified(context.Background(), image, func(_ context.Context, requestID string) error {
		requestIDs = append(requestIDs, requestID)
		if len(requestIDs) == 1 {
			return nil // journal replay: no pull happens
		}
		seedRestoreSet(t, root, image)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, requestIDs, 2)
	require.Equal(t, driver.warmPullRequestID(image), requestIDs[0])
	require.NotEqual(t, requestIDs[0], requestIDs[1])
	require.NoError(t, verifyRestorableImage(root, image))

	// A present cache entry needs no retry.
	requestIDs = nil
	require.NoError(t, driver.pinVerified(context.Background(), image, func(_ context.Context, requestID string) error {
		requestIDs = append(requestIDs, requestID)
		return nil
	}))
	require.Len(t, requestIDs, 1)

	// With a runtime-agent the manifest is the commit point: an entry whose
	// files survived but whose manifest is missing is not delivered either.
	driver.agentSocket = filepath.Join(root, "agent.sock")
	manifest := cachedManifestPath(root, image)
	committed, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.NoError(t, os.Remove(manifest))
	requestIDs = nil
	require.NoError(t, driver.pinVerified(context.Background(), image, func(_ context.Context, requestID string) error {
		requestIDs = append(requestIDs, requestID)
		if len(requestIDs) == 2 {
			return os.WriteFile(manifest, committed, 0o640)
		}
		return nil
	}))
	require.Len(t, requestIDs, 2)
	require.NoError(t, verifyDeliveredImage(root, image, true))
}

// seedRestoreSet writes a complete committed restore set (rootfs, vmstate,
// memory and their manifest), as a finished runtime-agent pull leaves it.
func seedRestoreSet(t *testing.T, root, image string) {
	t.Helper()
	directory := filepath.Join(root, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(directory, 0o750))
	contents := map[string][]byte{rootfsImageName: []byte("rootfs"), vmstateSnapshotName: []byte("vmstate"), memorySnapshotName: []byte("memory")}
	document := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: 2, MemoryMiB: 4096, RootfsSizeMiB: 4096, RootfsDriveID: "rootfs", RootfsDrivePath: "rootfs.img"}, "6.1.177", "/missing/firecracker", artifacts.CPUTemplateT2)
	document.Compatibility.FirecrackerVersion = "1.16.1"
	document.Files = map[string]artifacts.ManifestFile{}
	for name, content := range contents {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), content, 0o640))
		published := name
		if name == rootfsImageName {
			published = "rootfs.ext4"
		}
		document.Files[published] = artifacts.ManifestFile{SHA256: artifacts.SHA256Of(content), SizeBytes: int64(len(content))}
	}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "manifest.json"), raw, 0o640))
}
