package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
