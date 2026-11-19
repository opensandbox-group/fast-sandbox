package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	require.ErrorContains(t, trimBlobCache(blobCacheLimits{}), "live device files retained")
	require.FileExists(t, blobCachePath(desc.Digest))
	require.Equal(t, lowers, cachedLowers(home, image), "pin creation is idempotent")
	// Existing teardown removes the owning directory after device release.
	require.NoError(t, os.RemoveAll(home))
	require.NoError(t, trimBlobCache(blobCacheLimits{}))
	require.Empty(t, cachedLowers(t.TempDir(), image)[0].File, "evicted blobs use remote digest/size")
}

func TestLocalBlobCacheEvictsOldestAndProtectsPinnedBudget(t *testing.T) {
	setupBlobCache(t)
	first, _ := retainTestBlob(t, "old")
	second, _ := retainTestBlob(t, "new")
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(blobCachePath(first.Digest), old, old))
	require.NoError(t, trimBlobCache(blobCacheLimits{maxBytes: 3, maxFiles: 1}))
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
	require.Error(t, trimBlobCache(blobCacheLimits{maxBytes: 100, maxFiles: 100, minFree: ^uint64(0)}), "free-space protection preserves live files")
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

// seedBlobCacheEntry writes a cache entry directly so watermark tests control
// resident bytes and age without going through admission.
func seedBlobCacheEntry(t *testing.T, value string, size int, age time.Duration) digest.Digest {
	t.Helper()
	desc := digest.FromString(value)
	path := blobCachePath(desc)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", size)), 0600))
	stamp := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
	return desc
}

func cacheDiskBytes(t *testing.T, desc digest.Digest) int64 {
	t.Helper()
	info, err := os.Stat(blobCachePath(desc))
	require.NoError(t, err)
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

func TestLocalBlobCacheWatermarksDriveToTarget(t *testing.T) {
	setupBlobCache(t)
	global.blobCacheMaxBytes = 1000
	var descs []digest.Digest
	for i := 0; i < 10; i++ {
		descs = append(descs, seedBlobCacheEntry(t, fmt.Sprintf("entry-%d", i), 100, time.Duration(10-i)*time.Minute))
	}
	require.NoError(t, trimBlobCache(blobCacheLimits{maxBytes: 1000, maxFiles: blobCacheMaxFiles}))
	// Ledger usage crossed the 80% trigger, so the pass drove it down to the
	// 60% target instead of stopping at the boundary.
	for i, desc := range descs {
		if i < 4 {
			require.NoFileExists(t, blobCachePath(desc))
		} else {
			require.FileExists(t, blobCachePath(desc))
		}
	}
	stats := loadBlobCacheStats()
	require.Equal(t, int64(1000), stats.BudgetBytes)
	require.Equal(t, blobCacheStatWatermark{TriggerBytes: 800, TargetBytes: 600, WarnBytes: 750}, stats.Watermarks)
	require.Equal(t, 6, stats.Cache.Entries)
	require.Equal(t, int64(600), stats.Cache.Bytes)
	require.Equal(t, 60, stats.Cache.UsagePercent)
	require.Equal(t, stats.Cache.Bytes-stats.Cache.DiskBytes, stats.Cache.LedgerDiskDeltaBytes)
	expectedState := blobCacheStateOK
	if stats.Cache.DiskBytes > 750 {
		expectedState = blobCacheStateWarn
	}
	require.Equal(t, expectedState, stats.State)
	require.Zero(t, stats.Cache.PinnedCount)
	require.NotNil(t, stats.LastTrim)
	require.Equal(t, 4, stats.LastTrim.EvictedEntries)
	require.Equal(t, int64(400), stats.LastTrim.EvictedBytes)
	require.True(t, stats.LastTrim.ReachedTarget)
	require.Equal(t, int64(4), stats.Counters.Evictions)
	require.Equal(t, int64(400), stats.Counters.EvictedBytes)
	require.Equal(t, int64(1), stats.Counters.EvictionPasses)
	require.Zero(t, stats.Counters.EvictionFailures)
}

func TestLocalBlobCacheReservationTriggersAheadOfAdmission(t *testing.T) {
	setupBlobCache(t)
	global.blobCacheMaxBytes = 1000
	var descs []digest.Digest
	for i := 0; i < 7; i++ {
		descs = append(descs, seedBlobCacheEntry(t, fmt.Sprintf("reserved-%d", i), 100, time.Duration(7-i)*time.Minute))
	}
	// 700 resident bytes are below the 800 trigger, but the reserved admission
	// quota pushes the fill across it, so eviction runs before the rename.
	require.NoError(t, trimBlobCache(blobCacheLimits{maxBytes: 1000, maxFiles: blobCacheMaxFiles, reserve: 150}))
	require.NoFileExists(t, blobCachePath(descs[0]))
	require.FileExists(t, blobCachePath(descs[1]))
	stats := loadBlobCacheStats()
	require.Equal(t, 6, stats.Cache.Entries)
	require.Equal(t, int64(600), stats.Cache.Bytes)
	require.Equal(t, 1, stats.LastTrim.EvictedEntries)
	require.True(t, stats.LastTrim.ReachedTarget)
	// Without the reservation the same state does not trigger: the sweep only
	// runs when the incoming quota would cross the watermark.
	require.NoError(t, trimBlobCache(blobCacheLimits{maxBytes: 1000, maxFiles: blobCacheMaxFiles}))
	stats = loadBlobCacheStats()
	require.Equal(t, 6, stats.Cache.Entries)
	require.Zero(t, stats.LastTrim.EvictedEntries)
	require.True(t, stats.LastTrim.ReachedTarget)
	require.Equal(t, int64(2), stats.Counters.EvictionPasses)
	require.Equal(t, int64(1), stats.Counters.Evictions)
}

func TestLocalBlobPinsHoldWatermarkAndReportExhaustion(t *testing.T) {
	setupBlobCache(t)
	global.blobCacheMaxBytes = 2000
	desc := seedBlobCacheEntry(t, "pinned-live", 1600, time.Minute)
	home := t.TempDir()
	pin := filepath.Join(home, ".local-blobs", desc.Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(pin), 0700))
	require.NoError(t, os.Link(blobCachePath(desc), pin))
	disk := cacheDiskBytes(t, desc)
	require.GreaterOrEqual(t, disk, int64(1600))
	// 1600 resident pin-only bytes plus a 100-byte reservation cross the 1600
	// trigger, but the only entry is a live device file: the pass keeps it and
	// the alarm gauge reads allocated disk bytes above the 1500 watermark.
	require.NoError(t, trimBlobCache(blobCacheLimits{maxBytes: 2000, maxFiles: blobCacheMaxFiles, reserve: 100}))
	require.FileExists(t, blobCachePath(desc))
	stats := loadBlobCacheStats()
	require.Equal(t, blobCacheStateWarn, stats.State)
	require.Equal(t, 1, stats.Cache.PinnedCount)
	require.Equal(t, int64(1600), stats.Cache.PinnedBytes)
	require.Equal(t, disk, stats.Cache.DiskBytes)
	require.Equal(t, int(disk*100/2000), stats.Cache.DiskUsagePercent)
	require.False(t, stats.LastTrim.ReachedTarget)
	require.Zero(t, stats.Counters.Evictions)
	// An impossible free-space request cannot unlink the live file and is
	// published as the exhausted state.
	require.ErrorContains(t, trimBlobCache(blobCacheLimits{maxBytes: 2000, maxFiles: blobCacheMaxFiles, minFree: ^uint64(0)}), "live device files retained")
	stats = loadBlobCacheStats()
	require.Equal(t, blobCacheStateExhausted, stats.State)
	require.Equal(t, int64(1), stats.Counters.EvictionFailures)
	require.FileExists(t, blobCachePath(desc))
}

func TestLocalBlobCacheStatsCountAdmissionsLookupsAndPins(t *testing.T) {
	setupBlobCache(t)
	desc, _ := retainTestBlob(t, "checkpoint")
	require.NotEmpty(t, cachedLowers(t.TempDir(), blobTestImage(t, desc))[0].File)
	missing := oci.Descriptor{Digest: digest.FromString("missing"), Size: 7}
	require.Empty(t, cachedLowers(t.TempDir(), blobTestImage(t, missing))[0].File)
	stats := loadBlobCacheStats()
	require.Equal(t, int64(1), stats.Counters.Admissions)
	require.Equal(t, int64(len("checkpoint")), stats.Counters.AdmissionBytes)
	require.Equal(t, int64(1), stats.Counters.LookupHits)
	require.Equal(t, int64(1), stats.Counters.LookupMisses)
	require.Equal(t, 1, stats.Cache.Entries)
	require.Equal(t, int64(len("checkpoint")), stats.Cache.Bytes)
	require.Zero(t, stats.Cache.PinnedBytes, "lookup pins live beside device state, not in the cache directory")
}

func TestLocalBlobCacheStatsRestartOnCorruptSnapshot(t *testing.T) {
	setupBlobCache(t)
	require.NoError(t, os.MkdirAll(blobCacheRoot(), 0700))
	require.NoError(t, os.WriteFile(blobCacheStatsPath(), []byte("{broken"), 0600))
	updateBlobCacheStats(func(stats *blobCacheStats) { stats.Counters.LookupHits++ })
	stats := loadBlobCacheStats()
	require.Equal(t, int64(1), stats.Counters.LookupHits)
	require.Equal(t, blobCacheStatsVersion, stats.Version)
}

func TestBlobCacheWatermarkValidation(t *testing.T) {
	require.NoError(t, validateBlobCacheWatermarks(80, 60, 75))
	require.NoError(t, validateBlobCacheWatermarks(100, 100, 100))
	require.NoError(t, validateBlobCacheWatermarks(0, 0, 0))
	require.Error(t, validateBlobCacheWatermarks(70, 60, 75), "the alarm must not exceed the trigger")
	require.Error(t, validateBlobCacheWatermarks(80, 75, 60), "the target must not exceed the alarm")
	require.Error(t, validateBlobCacheWatermarks(101, 60, 75))
	require.Error(t, validateBlobCacheWatermarks(80, -1, 75))
}
