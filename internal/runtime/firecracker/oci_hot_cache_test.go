package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func hostCPUKnown() bool {
	id := artifacts.HostCPUIdentity()
	return id.Vendor != "" && id.Vendor != "unknown"
}

func hotCheckpointTestMetadata(t *testing.T) map[string]any {
	t.Helper()
	doc := preparedMetadata(t)
	if !hostCPUKnown() {
		compat, _ := doc["compatibility"].(map[string]any)
		if compat == nil {
			compat = map[string]any{}
			doc["compatibility"] = compat
		}
		compat["vendor"] = "GenuineIntel"
		compat["cpuFamily"] = 6
		compat["cpuModel"] = 85
		compat["cpuStepping"] = 4
	}
	return doc
}

func writeHotCheckpointStaging(t *testing.T, staging string, rawManifest []byte, memPayload string, withStateBundles bool) {
	t.Helper()
	require.NoError(t, os.MkdirAll(staging, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, snapshotManifestName), rawManifest, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(staging, vmstateSnapshotName), []byte("vmstate-bytes"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(staging, memorySnapshotName), []byte(memPayload), 0o600))
	if withStateBundles {
		require.NoError(t, os.WriteFile(filepath.Join(staging, vaultBundleName), []byte(`{"vault":true}`), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(staging, policyBundleName), []byte(`{"policy":true}`), 0o600))
	}
}

func seedHotCheckpointEntry(t *testing.T, stateRoot, digestHex string, rawManifest []byte) os.FileInfo {
	t.Helper()
	staging := filepath.Join(stateRoot, "seed-staging-"+digestHex[:8])
	writeHotCheckpointStaging(t, staging, rawManifest, "hot-memfile", false)
	require.NoError(t, retainOCIHotCheckpoint(stateRoot, digestHex, staging))
	info, err := os.Stat(filepath.Join(ociHotCheckpointPath(stateRoot, digestHex), ociMemfileName))
	require.NoError(t, err)
	return info
}

func TestOCIHotCheckpointRetainPinAndInodeIdentity(t *testing.T) {
	stateRoot := t.TempDir()
	staging := filepath.Join(stateRoot, "staging-1")
	digestHex := strings.Repeat("a", 64)

	rawManifest, err := json.Marshal(hotCheckpointTestMetadata(t))
	require.NoError(t, err)
	writeHotCheckpointStaging(t, staging, rawManifest, "memory-snapshot-payload", true)
	origMem, err := os.Stat(filepath.Join(staging, memorySnapshotName))
	require.NoError(t, err)
	origVMState, err := os.Stat(filepath.Join(staging, vmstateSnapshotName))
	require.NoError(t, err)

	require.NoError(t, retainOCIHotCheckpoint(stateRoot, digestHex, staging))
	require.NoFileExists(t, filepath.Join(staging, memorySnapshotName), "retention moves rather than copies staging files")
	require.NoFileExists(t, filepath.Join(staging, vmstateSnapshotName))

	cachedDir := ociHotCheckpointPath(stateRoot, digestHex)
	cachedMem, err := os.Stat(filepath.Join(cachedDir, ociMemfileName))
	require.NoError(t, err)
	require.True(t, os.SameFile(origMem, cachedMem), "cached memfile must retain the original inode")
	cachedVMState, err := os.Stat(filepath.Join(cachedDir, ociVMStateFileName))
	require.NoError(t, err)
	require.True(t, os.SameFile(origVMState, cachedVMState), "cached vmstate must retain the original inode")

	pinDir := filepath.Join(stateRoot, "pins", "sandbox-1")
	hit, err := pinOCIHotCheckpoint(stateRoot, digestHex, pinDir)
	require.NoError(t, err)
	require.True(t, hit)

	pinnedMem, err := os.Stat(filepath.Join(pinDir, ociMemfileName))
	require.NoError(t, err)
	require.True(t, os.SameFile(origMem, pinnedMem), "pinned memfile must hard-link the cached inode")
	require.FileExists(t, filepath.Join(pinDir, vaultBundleName))
	require.FileExists(t, filepath.Join(pinDir, policyBundleName))

	// While pinned, eviction cannot delete the live entry even under zero budget.
	t.Setenv(ociHotCheckpointCacheSizeEnv, "0")
	require.ErrorContains(t, reclaimOCIHotCheckpoints(stateRoot, 0), "pinned entries retained")
	require.DirExists(t, cachedDir)

	// Releasing the pin allows capacity recovery to reclaim the entry even when admission is disabled.
	require.NoError(t, os.RemoveAll(pinDir))
	require.NoError(t, reclaimOCIHotCheckpoints(stateRoot, 0))
	require.NoDirExists(t, cachedDir)
}

func TestOCIHotCheckpointBudgetWatermarksTTLAndLocks(t *testing.T) {
	stateRoot := t.TempDir()
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	digestC := strings.Repeat("c", 64)

	rawManifest, err := json.Marshal(hotCheckpointTestMetadata(t))
	require.NoError(t, err)
	entrySize := int64(len(rawManifest) + len("vmstate-bytes") + 4096)

	t.Setenv(ociHotCheckpointCacheSizeEnv, strconv.FormatInt(entrySize*3, 10))
	stagingA := filepath.Join(stateRoot, "staging-a")
	writeHotCheckpointStaging(t, stagingA, rawManifest, strings.Repeat("x", 4096), false)
	require.NoError(t, retainOCIHotCheckpoint(stateRoot, digestA, stagingA))
	oldTime := time.Now().Add(-10 * time.Minute)
	require.NoError(t, os.Chtimes(ociHotCheckpointPath(stateRoot, digestA), oldTime, oldTime))

	stagingB := filepath.Join(stateRoot, "staging-b")
	writeHotCheckpointStaging(t, stagingB, rawManifest, strings.Repeat("y", 4096), false)
	require.NoError(t, retainOCIHotCheckpoint(stateRoot, digestB, stagingB))

	staleStaging := filepath.Join(ociHotCheckpointRoot(stateRoot), ociHotCacheStagingPrefix+"orphan")
	require.NoError(t, os.MkdirAll(staleStaging, 0o700))

	// Tighten budget to 2*entrySize so admitting C evicts oldest entry A and reaps orphaned staging.
	t.Setenv(ociHotCheckpointCacheSizeEnv, strconv.FormatInt(entrySize*2, 10))
	stagingC := filepath.Join(stateRoot, "staging-c")
	writeHotCheckpointStaging(t, stagingC, rawManifest, strings.Repeat("z", 4096), false)
	require.NoError(t, retainOCIHotCheckpoint(stateRoot, digestC, stagingC))
	require.NoDirExists(t, ociHotCheckpointPath(stateRoot, digestA), "oldest unpinned entry should be evicted")
	require.NoDirExists(t, staleStaging, "orphaned staging dir should be cleaned under exclusive lock")
	require.DirExists(t, ociHotCheckpointPath(stateRoot, digestC))

	// TTL expiration causes lookup miss.
	t.Setenv(ociHotCheckpointCacheTTLEnv, "1m")
	expired := time.Now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(ociHotCheckpointPath(stateRoot, digestC), expired, expired))
	hit, err := pinOCIHotCheckpoint(stateRoot, digestC, filepath.Join(stateRoot, "pin-expired"))
	require.NoError(t, err)
	require.False(t, hit, "expired entry must miss on lookup")

	touchPath(ociHotCheckpointPath(stateRoot, digestC))

	unlockShared, err := lockOCIHotCache(ociHotCheckpointRoot(stateRoot), unix.LOCK_SH|unix.LOCK_NB)
	require.NoError(t, err)
	hit, err = pinOCIHotCheckpoint(stateRoot, digestC, filepath.Join(stateRoot, "pin-shared"))
	require.NoError(t, err)
	require.True(t, hit, "concurrent shared readers must hit hot cache")
	unlockShared()

	unlockExcl, err := lockOCIHotCache(ociHotCheckpointRoot(stateRoot), unix.LOCK_EX|unix.LOCK_NB)
	require.NoError(t, err)
	hit, err = pinOCIHotCheckpoint(stateRoot, digestC, filepath.Join(stateRoot, "pin-busy"))
	require.NoError(t, err)
	require.False(t, hit, "exclusive lock contention must fall back cleanly")
	unlockExcl()
}

func TestPrepareOCIRestoreUsesHotCacheAndFallsBack(t *testing.T) {
	digestHex := strings.Repeat("b", 64)
	image := "oci://r.example/templates/base@sha256:" + digestHex

	setupResumeFixture := func(t *testing.T, ownerUID string) (*driverFixture, []byte, string) {
		t.Helper()
		fixture := newDriverFixture(t)
		require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
		metadata := hotCheckpointTestMetadata(t)
		metadata["platform"] = map[string]any{"checkpoint": map[string]string{"sandboxUID": ownerUID, "namespace": "tenant-a"}}
		rawMeta, err := json.Marshal(metadata)
		require.NoError(t, err)
		hotArgFile := filepath.Join(fixture.stateRoot, "observed-hot-arg")
		t.Setenv("TEST_HOT_ARG_FILE", hotArgFile)
		installPreparedOCI(t, fixture, string(rawMeta))
		fixture.sandboxSpec.Desired.Image = image
		fixture.sandboxSpec.Desired.Restore = &fastletapi.RestoreSpec{ManifestRef: image, ArtifactDigest: digestHex}
		return fixture, rawMeta, hotArgFile
	}

	t.Run("hit passes hot-snapfiles-dir and preserves inode", func(t *testing.T) {
		if !hostCPUKnown() {
			t.Skip("host CPU identity compatibility check requires Linux /proc/cpuinfo")
		}
		fixture, rawMeta, hotArgFile := setupResumeFixture(t, "sandbox-1")
		cachedMem := seedHotCheckpointEntry(t, fixture.stateRoot, digestHex, rawMeta)

		_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
		require.NoError(t, err)
		argRaw, err := os.ReadFile(hotArgFile)
		require.NoError(t, err)
		require.NotEmpty(t, string(argRaw), "hot cache hit must pass --hot-snapfiles-dir to fsb-vm")
		require.NoDirExists(t, string(argRaw), "temporary pin directory must be cleaned up after attach")

		mountedMem, err := os.Stat(filepath.Join(fixture.driver.ociRoot(), "sandboxes", "sandbox-1", "mnt", ociMemfileName))
		require.NoError(t, err)
		require.True(t, os.SameFile(cachedMem, mountedMem), "sandbox mnt/memfile must share inode with hot cache")
	})

	t.Run("miss falls back transparently without hot-snapfiles-dir", func(t *testing.T) {
		if !hostCPUKnown() {
			t.Skip("host CPU identity compatibility check requires Linux /proc/cpuinfo")
		}
		fixture, _, hotArgFile := setupResumeFixture(t, "sandbox-1")

		_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
		require.NoError(t, err)
		argRaw, err := os.ReadFile(hotArgFile)
		require.NoError(t, err)
		require.Empty(t, string(argRaw), "hot cache miss must fall back to normal OCI snapfiles path")
	})

	t.Run("attach passes hot-snapfiles-dir and validates owner across platforms", func(t *testing.T) {
		fixture, rawMeta, hotArgFile := setupResumeFixture(t, "sandbox-1")
		cachedMem := seedHotCheckpointEntry(t, fixture.stateRoot, digestHex, rawMeta)

		pinDir := filepath.Join(fixture.driver.ociRoot(), "hot-pins", "sandbox-1")
		hit, err := pinOCIHotCheckpoint(fixture.stateRoot, digestHex, pinDir)
		require.NoError(t, err)
		require.True(t, hit)

		rootfs, vmstate, memfile, err := fixture.driver.attachOCIWithHotSnapfiles(context.Background(), "sandbox-1", image, pinDir)
		require.NoError(t, err)
		require.Equal(t, "/dev/ublkb42", rootfs)
		require.Equal(t, filepath.Join(fixture.driver.ociRoot(), "sandboxes", "sandbox-1", "mnt", ociVMStateFileName), vmstate)
		require.Equal(t, filepath.Join(fixture.driver.ociRoot(), "sandboxes", "sandbox-1", "mnt", ociMemfileName), memfile)
		require.NoError(t, os.RemoveAll(pinDir))

		argRaw, err := os.ReadFile(hotArgFile)
		require.NoError(t, err)
		require.Equal(t, pinDir, string(argRaw))

		mountedMem, err := os.Stat(memfile)
		require.NoError(t, err)
		require.True(t, os.SameFile(cachedMem, mountedMem))

		parsedMeta, err := readPreparedOCIMetadata(fixture.driver.ociRoot(), "sandbox-1")
		require.NoError(t, err)
		require.NoError(t, validateOCIResumeOwner(parsedMeta, &fixture.sandboxSpec))
	})

	t.Run("owner mismatch still rejected on hot cache hit", func(t *testing.T) {
		fixture, rawMeta, _ := setupResumeFixture(t, "other-sandbox")
		seedHotCheckpointEntry(t, fixture.stateRoot, digestHex, rawMeta)

		_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
		require.ErrorIs(t, err, ErrInvalidConfig)
		require.NoDirExists(t, filepath.Join(fixture.driver.ociRoot(), "sandboxes", "sandbox-1"))
	})
}
