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
