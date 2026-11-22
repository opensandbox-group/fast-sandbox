package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

func testJobs(n int) []chunkJob {
	jobs := make([]chunkJob, n)
	for i := range jobs {
		jobs[i] = chunkJob{file: "f", offset: int64(i) * testChunkSize, length: testChunkSize}
	}
	return jobs
}

// A parent cancelled between jobs must not read as success: the workers stop
// at their context check with no job error, and a nil return would let the
// pull commit (and mark verified) files whose remaining chunks are holes.
func TestRunChunkJobsCancelledBetweenJobs(t *testing.T) {
	for _, workers := range []int{1, 4} {
		ctx, cancel := context.WithCancel(context.Background())
		var ran atomic.Int64
		err := runChunkJobs(ctx, workers, testJobs(16), func(context.Context, chunkJob) error {
			ran.Add(1)
			cancel()
			return nil
		})
		require.ErrorIs(t, err, context.Canceled, "workers=%d", workers)
		require.Less(t, ran.Load(), int64(16), "workers=%d: cancellation must stop the remaining jobs", workers)
	}
}

func TestRunChunkJobsCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runChunkJobs(ctx, 4, testJobs(8), func(context.Context, chunkJob) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunChunkJobsJobErrorWinsOverCancellation(t *testing.T) {
	boom := errors.New("boom")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := runChunkJobs(ctx, 1, testJobs(4), func(context.Context, chunkJob) error {
		cancel()
		return boom
	})
	require.ErrorIs(t, err, boom)
}

func TestRunChunkJobsAllSucceed(t *testing.T) {
	var ran atomic.Int64
	require.NoError(t, runChunkJobs(context.Background(), 4, testJobs(9), func(context.Context, chunkJob) error {
		ran.Add(1)
		return nil
	}))
	require.Equal(t, int64(9), ran.Load())
	require.NoError(t, runChunkJobs(context.Background(), 4, nil, func(context.Context, chunkJob) error { return nil }))
}

// A pull whose context is cancelled while chunks are being fetched must fail
// and leave no committed cache entry, whichever side of a fetch the
// cancellation lands on; a retry then restores the set.
func TestChunkedPullCancelledMidwayCommitsNothing(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	result, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, stagedFiles(), nil))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var chunkGets atomic.Int64
	cancelling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.handle(w, r)
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, artifacts.ChunkStorePrefix) && chunkGets.Add(1) == 1 {
			cancel()
		}
	}))
	defer cancelling.Close()

	stateRoot := t.TempDir()
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	transfer := chunkedTransfer(TransferCompressionNone)
	transfer.Concurrency = 1
	err = newTestClient(t, cancelling, transfer).PullCheckpoint(ctx, stateRoot, reference, result.ManifestRef, result.ArtifactDigest)
	require.Error(t, err, "a cancelled pull must not report success")
	ready, readyErr := ImageReady(stateRoot, reference)
	require.NoError(t, readyErr)
	require.False(t, ready, "a cancelled pull must not leave a committed cache entry")

	require.NoError(t, newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
	ready, readyErr = ImageReady(stateRoot, reference)
	require.NoError(t, readyErr)
	require.True(t, ready)
}
