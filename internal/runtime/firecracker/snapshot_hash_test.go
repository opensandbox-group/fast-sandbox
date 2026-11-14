package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestCopyFileSparseHashedMatchesContentAndDigest(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Join([][]byte{
		make([]byte, 5<<20),               // spans the 4 MiB read buffer
		bytes.Repeat([]byte{0xab}, 70000), // straddles a 64 KiB block
		make([]byte, 64<<10+3),            // trailing hole with odd length
	}, nil)
	source := filepath.Join(dir, "memory.dump")
	require.NoError(t, os.WriteFile(source, payload, 0o640))

	target := filepath.Join(dir, "memory.snap")
	sum, err := copyFileSparseHashed(source, target)
	require.NoError(t, err)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got))
	want, err := artifacts.SHA256File(source)
	require.NoError(t, err)
	require.Equal(t, want, sum)
}

func TestHashStagedFilesUsesKnownDigests(t *testing.T) {
	dir := t.TempDir()
	for name, payload := range map[string]string{"a": "alpha", "b": "beta"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(payload), 0o640))
	}
	known := map[string]string{filepath.Join(dir, "a"): "precomputed"}
	cache, err := hashStagedFiles(dir, []string{"a", "b"}, known)
	require.NoError(t, err)
	require.Equal(t, "precomputed", cache[filepath.Join(dir, "a")])
	want, err := artifacts.SHA256File(filepath.Join(dir, "b"))
	require.NoError(t, err)
	require.Equal(t, want, cache[filepath.Join(dir, "b")])

	_, err = hashStagedFiles(dir, []string{"missing"}, nil)
	require.Error(t, err)
}

// chunkedSnapshotAgent is a snapshot agent fake that reports the chunked
// transfer mode, like a runtime-agent configured with transfer.format:
// chunked.
type chunkedSnapshotAgent struct {
	*snapshotAgentFake
}

func (a chunkedSnapshotAgent) TransferFormat() string { return agentTransferFormatChunked }

func (a chunkedSnapshotAgent) PublishImage(ctx context.Context, requestID, kind, key, dir string) (PublishOutcome, error) {
	outcome, err := a.snapshotAgentFake.PublishImage(ctx, requestID, kind, key, dir)
	outcome.TransferFormat = agentTransferFormatChunked
	return outcome, err
}

func TestAgentTransferChunkedFollowsTheAgent(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	require.False(t, fixture.driver.agentTransferChunked(), "an agent that reports no mode means legacy")

	fixture.driver.agentClient = chunkedSnapshotAgent{agent}
	require.True(t, fixture.driver.agentTransferChunked())
}

func TestCreateTemplateSnapshotCommitsCacheInChunkedMode(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	fixture.driver.agentClient = chunkedSnapshotAgent{agent}
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	// The set is addressable under the template name, so a create from the
	// snapshot on this node restores without a store pull (issue #57).
	require.NoError(t, verifyRestorableImage(fixture.stateRoot, "app-v2"))
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1"))
	require.True(t, os.IsNotExist(statErr), "staging must be consumed by the cache commit")
}

func TestCreateTemplateSnapshotDiscardsStagingInLegacyMode(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.ErrorIs(t, verifyRestorableImage(fixture.stateRoot, "app-v2"), ErrImageNotReady)
}

// withClonedRootfs stands in for a reflink-capable filesystem: the snapshot
// rootfs "clone" succeeds (as a plain copy), enabling the background hash.
func withClonedRootfs(t *testing.T) {
	t.Helper()
	original := cloneRootfs
	cloneRootfs = copyFile
	t.Cleanup(func() { cloneRootfs = original })
}

// observeRootfsHash wraps the background rootfs hash, failing it when fail is
// set, and returns the paths it was called with.
func observeRootfsHash(t *testing.T, fail bool) *[]string {
	t.Helper()
	var calls []string
	original := hashStagedRootfs
	hashStagedRootfs = func(path string) (string, error) {
		calls = append(calls, path)
		if fail {
			return "", os.ErrPermission
		}
		return original(path)
	}
	t.Cleanup(func() { hashStagedRootfs = original })
	return &calls
}

func manifestRootfsDigest(t *testing.T, agent *snapshotAgentFake) string {
	t.Helper()
	files, ok := agent.manifest["files"].(map[string]any)
	require.True(t, ok, "manifest files section")
	entry, ok := files[publishedRootfsName].(map[string]any)
	require.True(t, ok, "manifest rootfs entry")
	return entry["sha256"].(string)
}

func TestCreateSnapshotHashesClonedRootfsInBackground(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	fixture.driver.agentClient = chunkedSnapshotAgent{agent}
	withClonedRootfs(t)
	calls := observeRootfsHash(t, false)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	want, err := artifacts.SHA256File(filepath.Join(fixture.stateRoot, sandboxStateDir, sandboxID, instanceRootfsName))
	require.NoError(t, err)

	_, err = fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1", publishedRootfsName)}, *calls,
		"the cloned rootfs is hashed once, by the background hash")
	require.Equal(t, want, manifestRootfsDigest(t, agent))
}

func TestCreateSnapshotFallsBackWhenBackgroundRootfsHashFails(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	fixture.driver.agentClient = chunkedSnapshotAgent{agent}
	withClonedRootfs(t)
	calls := observeRootfsHash(t, true)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	want, err := artifacts.SHA256File(filepath.Join(fixture.stateRoot, sandboxStateDir, sandboxID, instanceRootfsName))
	require.NoError(t, err)

	_, err = fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Len(t, *calls, 1)
	require.Equal(t, want, manifestRootfsDigest(t, agent), "the manifest assembly hashes the rootfs itself")
}

func TestCreateSnapshotLegacyModeSkipsBackgroundRootfsHash(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	withClonedRootfs(t)
	calls := observeRootfsHash(t, false)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Empty(t, *calls, "legacy mode keeps the sequential assembly")
}

// The pipelined copy must produce exactly the content and digest of a serial
// copy for dense, zero-heavy and unaligned files that span many in-flight
// blocks, and leave zero granules as holes.
func TestCopyFileSparseHashedPipelineShapes(t *testing.T) {
	dense := make([]byte, 9*sparseCopyBlockSize+12345)
	for i := range dense {
		dense[i] = byte(i*7 + i>>13)
	}
	zeroHeavy := make([]byte, 6*sparseCopyBlockSize+sparseZeroGranule/2)
	for offset := 0; offset < len(zeroHeavy); offset += 3 * sparseZeroGranule {
		copy(zeroHeavy[offset:], bytes.Repeat([]byte{0x5a}, 100))
	}
	shapes := map[string][]byte{
		"empty":       {},
		"one byte":    {1},
		"dense":       dense,
		"zero heavy":  zeroHeavy,
		"all zero":    make([]byte, 2*sparseCopyBlockSize+1),
		"block exact": bytes.Repeat([]byte{0xff}, sparseCopySlots*sparseCopyBlockSize),
	}
	for name, payload := range shapes {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			require.NoError(t, os.WriteFile(source, payload, 0o640))
			target := filepath.Join(dir, "target")
			sum, err := copyFileSparseHashed(source, target)
			require.NoError(t, err)
			want, err := artifacts.SHA256File(source)
			require.NoError(t, err)
			require.Equal(t, want, sum)
			got, err := os.ReadFile(target)
			require.NoError(t, err)
			require.True(t, bytes.Equal(payload, got))
		})
	}

	if runtime.GOOS != "linux" {
		t.Skip("hole layout is checked on linux filesystems")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "zero-heavy")
	require.NoError(t, os.WriteFile(source, zeroHeavy, 0o640))
	target := filepath.Join(dir, "zero-heavy.copy")
	_, err := copyFileSparseHashed(source, target)
	require.NoError(t, err)
	info, err := os.Stat(target)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Less(t, stat.Blocks*512, int64(len(zeroHeavy))/2, "zero granules stay holes")
}

// A read failure stops the pipeline and is returned.
func TestCopyFileSparseHashedReportsReadError(t *testing.T) {
	dir := t.TempDir()
	_, err := copyFileSparseHashed(dir, filepath.Join(t.TempDir(), "target"))
	require.Error(t, err)
}

// chunkListSnapshotAgent is a chunked snapshot agent fake that also publishes
// native artifacts under chunk-list digests.
type chunkListSnapshotAgent struct {
	chunkedSnapshotAgent
}

func (a chunkListSnapshotAgent) ChunkListDigest() bool { return true }

func TestCreateSnapshotSkipsWholeFileHashesWithChunkListAgent(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	fixture.driver.agentClient = chunkListSnapshotAgent{chunkedSnapshotAgent{agent}}
	require.True(t, fixture.driver.agentChunkListDigest())
	withClonedRootfs(t)
	calls := observeRootfsHash(t, false)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Empty(t, *calls, "no background rootfs hash when the agent identifies chunks")
	files, ok := agent.manifest["files"].(map[string]any)
	require.True(t, ok, "manifest files section")
	for name, entry := range files {
		fields := entry.(map[string]any)
		require.NotContains(t, fields, "sha256", "%s is recorded by size only", name)
		require.Positive(t, fields["sizeBytes"], "%s keeps its size", name)
	}
}

func TestCollectSnapshotManifestEntriesChunkListHashesOnlyStateFiles(t *testing.T) {
	staging := t.TempDir()
	for name, payload := range map[string]string{
		publishedRootfsName: "rootfs", memorySnapshotName: "memory", vmstateSnapshotName: "vmstate",
		vaultBundleName: "vault", policyBundleName: "policy",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte(payload), 0o640))
	}
	entries, err := collectSnapshotManifestEntries(staging, false, nil, true, true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{vaultBundleName, policyBundleName}, entries.HashedNames)
	for name, file := range entries.Files {
		require.Empty(t, file.SHA256, "%s", name)
		require.Positive(t, file.SizeBytes, "%s", name)
	}
	for name, file := range entries.StateFiles {
		require.NotEmpty(t, file.SHA256, "state file %s keeps its sha256", name)
	}

	entries, err = collectSnapshotManifestEntries(staging, false, nil, true, false)
	require.NoError(t, err)
	require.Len(t, entries.HashedNames, len(entries.Names), "without chunk-list digests every file is hashed")
}

func TestRelocateSpilledFileWithChunkListRecordsNoDigest(t *testing.T) {
	dir := t.TempDir()
	payload := append(make([]byte, 1<<20), bytes.Repeat([]byte{7}, 4096)...)
	source := filepath.Join(dir, "memory.dump")
	require.NoError(t, os.WriteFile(source, payload, 0o640))
	target := filepath.Join(dir, "memory.snap")
	plan := &dumpPlan{chunked: true, chunkList: true}
	require.NoError(t, relocateSpilledFile(plan, source, target))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got))
	require.Empty(t, plan.knownSums, "no whole-file digest is computed")
}
