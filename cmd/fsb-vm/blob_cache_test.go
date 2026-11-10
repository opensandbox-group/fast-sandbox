package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/ocitemplate"
)

func setupBlobCache(t *testing.T) {
	t.Helper()
	setupManifestCache(t)
	global.blobCacheDir = filepath.Join(t.TempDir(), "blobs")
	global.blobCacheMaxBytes = 1 << 20
}

func retainTestBlob(t *testing.T, value string) (oci.Descriptor, os.FileInfo) {
	t.Helper()
	desc := oci.Descriptor{Digest: digest.FromString(value), Size: int64(len(value))}
	artifacts := t.TempDir()
	path := filepath.Join(artifacts, "blobs/sha256", desc.Digest.Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(value), 0600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, retainCheckpointLayers(context.Background(), []oci.Descriptor{desc}, artifacts))
	require.NoFileExists(t, path, "retention moves, rather than copies, the source")
	cached, err := os.Stat(blobCachePath(desc.Digest))
	require.NoError(t, err)
	require.True(t, os.SameFile(info, cached), "same inode survives staging cleanup")
	return desc, info
}

func blobTestImage(t *testing.T, desc oci.Descriptor) resolvedImage {
	t.Helper()
	// Decode through the actual manifest path instead of duplicating its type.
	root := cacheTestManifest(t, "placeholder")
	var manifest oci.Manifest
	require.NoError(t, json.Unmarshal(root, &manifest))
	manifest.Layers = []oci.Descriptor{desc}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	registry := newManifestCacheRegistry(t, raw)
	image, err := resolveImage(context.Background(), registry.reference(digest.FromBytes(raw).String()), dockerConfig{})
	require.NoError(t, err)
	return image
}

func TestLocalBlobPinsSurviveEvictionUntilDeviceCleanup(t *testing.T) {
	setupBlobCache(t)
	desc, source := retainTestBlob(t, "checkpoint")
	image := blobTestImage(t, desc)
	home := t.TempDir()
	lowers := cachedLowers(home, image)
	require.Len(t, lowers, 1)
	require.NotEmpty(t, lowers[0].File)
	pin, err := os.Stat(lowers[0].File)
	require.NoError(t, err)
	require.True(t, os.SameFile(source, pin))
	require.Equal(t, desc.Digest.String(), lowers[0].Digest)
	require.ErrorContains(t, trimBlobCache(0, 0, 0), "live device files retained")
	require.FileExists(t, blobCachePath(desc.Digest))
	require.Equal(t, lowers, cachedLowers(home, image), "pin creation is idempotent")
	// Existing teardown removes the owning directory after device release.
	require.NoError(t, os.RemoveAll(home))
	require.NoError(t, trimBlobCache(0, 0, 0))
	require.Empty(t, cachedLowers(t.TempDir(), image)[0].File, "evicted blobs use remote digest/size")
}

func TestLocalBlobCacheEvictsOldestAndProtectsPinnedBudget(t *testing.T) {
	setupBlobCache(t)
	first, _ := retainTestBlob(t, "old")
	second, _ := retainTestBlob(t, "new")
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(blobCachePath(first.Digest), old, old))
	require.NoError(t, trimBlobCache(3, 1, 0))
	require.NoFileExists(t, blobCachePath(first.Digest))
	require.FileExists(t, blobCachePath(second.Digest))
	image := blobTestImage(t, second)
	require.NotEmpty(t, cachedLowers(t.TempDir(), image)[0].File)
	global.blobCacheMaxBytes = 3
	path := filepath.Join(t.TempDir(), "incoming")
	require.NoError(t, os.WriteFile(path, []byte("add"), 0600))
	require.Error(t, retainBlob(path, digest.FromString("add"), 3))
	require.FileExists(t, path, "rejected admission leaves staging for normal cleanup")
	require.FileExists(t, blobCachePath(second.Digest))
	require.Error(t, trimBlobCache(100, 100, ^uint64(0)), "free-space protection preserves live files")
}

func TestLocalBlobCacheBusyDisabledAndInvalidSource(t *testing.T) {
	setupBlobCache(t)
	desc, _ := retainTestBlob(t, "data")
	image := blobTestImage(t, desc)
	unlock, err := tryLock(blobCacheRoot(), "cache")
	require.NoError(t, err)
	require.Empty(t, cachedLowers(t.TempDir(), image)[0].File)
	require.ErrorIs(t, retainCheckpointLayers(context.Background(), nil, t.TempDir()), errBusy)
	unlock()
	global.blobCacheMaxBytes = 0
	require.Empty(t, cachedLowers(t.TempDir(), image)[0].File)
	require.NoError(t, retainCheckpointLayers(context.Background(), nil, t.TempDir()))
	global.blobCacheMaxBytes = 1024
	artifacts := t.TempDir()
	path := filepath.Join(artifacts, "blobs/sha256", desc.Digest.Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.Symlink(blobCachePath(desc.Digest), path))
	require.ErrorContains(t, retainCheckpointLayers(context.Background(), []oci.Descriptor{desc}, artifacts), "type mismatch")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, retainCheckpointLayers(ctx, []oci.Descriptor{desc}, artifacts), context.Canceled)
}

func TestLocalCheckpointMetadataAndBlobsSurviveStagingCleanup(t *testing.T) {
	setupBlobCache(t)
	root, snap := cacheTestManifest(t, "inherited"), cacheTestManifest(t, "checkpoint")
	registry := newManifestCacheRegistry(t, root)
	registry.byDigest[digest.FromBytes(snap).String()] = snap
	rootRef, snapRef := registry.reference(digest.FromBytes(root).String()), registry.reference(digest.FromBytes(snap).String())
	metadata, err := json.Marshal(map[string]any{"oci": ocitemplate.Pair{Rootfs: rootRef, Snapfiles: snapRef}})
	require.NoError(t, err)
	result := &ocitemplate.PublishedCheckpoint{Reference: ocitemplate.Prefix + snapRef, Metadata: metadata, Manifests: map[string]json.RawMessage{rootRef: root, snapRef: snap}}
	artifacts := t.TempDir()
	path := filepath.Join(artifacts, "blobs/sha256", digest.FromString("checkpoint").Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("checkpoint"), 0600))
	require.NoError(t, cacheCheckpoint(context.Background(), result, dockerConfig{}, artifacts))
	require.NoError(t, os.RemoveAll(artifacts))
	registry.mu.Lock()
	registry.deny = true
	registry.mu.Unlock()
	var template templateFile
	template.Template.Reference = result.Reference
	require.NoError(t, resolveTemplate(context.Background(), &template, dockerConfig{}))
	image, err := resolveImage(context.Background(), template.Template.Snapfiles, dockerConfig{})
	require.NoError(t, err)
	lowers := cachedLowers(t.TempDir(), image)
	content, err := os.ReadFile(lowers[0].File)
	require.NoError(t, err)
	require.Equal(t, "checkpoint", string(content))
	require.Zero(t, registry.requestCount(), "publication seeds metadata without fetching it")
	// Invalid publication metadata cannot be admitted even with valid files.
	result.Manifests[rootRef] = snap
	require.ErrorContains(t, cacheCheckpoint(context.Background(), result, dockerConfig{}, artifacts), "digest mismatch")
}

func TestLocalBlobAdmissionFailureKeepsManifestSeed(t *testing.T) {
	setupBlobCache(t)
	raw := cacheTestManifest(t, "large")
	registry := newManifestCacheRegistry(t, raw)
	ref := registry.reference(digest.FromBytes(raw).String())
	metadata, err := json.Marshal(map[string]any{"oci": ocitemplate.Pair{Rootfs: ref, Snapfiles: ref}})
	require.NoError(t, err)
	result := &ocitemplate.PublishedCheckpoint{Reference: ocitemplate.Prefix + ref, Metadata: metadata, Manifests: map[string]json.RawMessage{ref: raw}}
	artifacts := t.TempDir()
	path := filepath.Join(artifacts, "blobs/sha256", digest.FromString("large").Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("large"), 0600))
	global.blobCacheMaxBytes = 1
	require.ErrorContains(t, cacheCheckpoint(context.Background(), result, dockerConfig{}, artifacts), "exceeds")
	_, err = resolveImage(context.Background(), ref, dockerConfig{})
	require.NoError(t, err)
	require.Zero(t, registry.requestCount())
	require.FileExists(t, path)
}

func TestLocalBlobCrossFilesystemFallsBackWithoutCopy(t *testing.T) {
	setupBlobCache(t)
	other, err := os.MkdirTemp("/dev/shm", "fsb-blob-test-")
	if err != nil {
		t.Skip("separate tmpfs unavailable")
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })
	desc, cached := retainTestBlob(t, "cross-device")
	remoteDir, err := os.Stat(other)
	require.NoError(t, err)
	if cached.Sys().(*syscall.Stat_t).Dev == remoteDir.Sys().(*syscall.Stat_t).Dev {
		t.Skip("test requires separate filesystems")
	}
	require.Empty(t, cachedLowers(other, blobTestImage(t, desc))[0].File)
	path := filepath.Join(other, "source")
	require.NoError(t, os.WriteFile(path, []byte("new"), 0600))
	err = retainBlob(path, digest.FromString("new"), 3)
	require.ErrorIs(t, err, syscall.EXDEV)
	require.FileExists(t, path)
	require.NoFileExists(t, blobCachePath(digest.FromString("new")))
}

func TestTrimBlobCacheForSnapshotCapacity(t *testing.T) {
	setupBlobCache(t)
	unpinned, _ := retainTestBlob(t, "reclaimable")
	pinned, _ := retainTestBlob(t, "live")
	home := t.TempDir()
	lower := cachedLowers(home, blobTestImage(t, pinned))[0]
	require.NotEmpty(t, lower.File)
	run := func(minFree uint64) error {
		command := newTrimBlobCacheCommand()
		command.SetArgs([]string{"--min-free-bytes", strconv.FormatUint(minFree, 10)})
		return command.Execute()
	}
	// Even with entries below the byte/file budget, a staging-space request
	// reclaims unpinned data. An impossible target must not unlink live files.
	require.ErrorContains(t, run(^uint64(0)), "live device files retained")
	require.NoFileExists(t, blobCachePath(unpinned.Digest))
	require.FileExists(t, blobCachePath(pinned.Digest))
	data, err := os.ReadFile(lower.File)
	require.NoError(t, err)
	require.Equal(t, "live", string(data))

	// Disabling admission must not strand old cache contents. Reclamation
	// uses the same lock as admission and device pin creation.
	require.NoError(t, os.RemoveAll(home))
	global.blobCacheMaxBytes = 0
	unlock, err := tryLock(blobCacheRoot(), "cache")
	require.NoError(t, err)
	require.ErrorIs(t, run(0), errBusy)
	require.FileExists(t, blobCachePath(pinned.Digest))
	unlock()
	require.NoError(t, run(0))
	require.NoFileExists(t, blobCachePath(pinned.Digest))
}
