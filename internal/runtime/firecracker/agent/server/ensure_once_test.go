package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
)

// gatedPuller blocks every pull until released (or its context ends), so a
// test can hold one pull in flight while other ensures arrive.
type gatedPuller struct {
	pulls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func newGatedPuller() *gatedPuller {
	return &gatedPuller{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (g *gatedPuller) pull(ctx context.Context, stateRoot, reference string) error {
	g.pulls.Add(1)
	g.started <- struct{}{}
	select {
	case <-g.release:
		return seedCache(stateRoot, reference)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedPuller) PullImage(ctx context.Context, stateRoot, image string) error {
	return g.pull(ctx, stateRoot, image)
}

func (g *gatedPuller) PullCheckpoint(ctx context.Context, stateRoot, reference, _, _ string) error {
	return g.pull(ctx, stateRoot, reference)
}

func newGatedService(t *testing.T) (*Service, *gatedPuller) {
	t.Helper()
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	puller := newGatedPuller()
	return NewService(puller, state, stateRoot), puller
}

func TestConcurrentEnsuresOfOneImageRunOnePull(t *testing.T) {
	service, puller := newGatedService(t)
	const callers = 8

	errs := make(chan error, callers)
	go func() { errs <- service.ensureImage(context.Background(), testImage) }()
	<-puller.started
	var joined sync.WaitGroup
	for range callers - 1 {
		joined.Add(1)
		go func() {
			joined.Done()
			errs <- service.ensureImage(context.Background(), testImage)
		}()
	}
	joined.Wait()
	// Give the waiters time to reach the in-flight ensure; any that arrive
	// after the release find the committed cache and pull nothing either.
	time.Sleep(50 * time.Millisecond)
	close(puller.release)
	for range callers {
		require.NoError(t, <-errs)
	}
	require.Equal(t, int64(1), puller.pulls.Load(), "concurrent ensures of one image share one pull")

	// A different image is a different key and pulls on its own.
	require.NoError(t, service.ensureImage(context.Background(), testImage+"-other"))
	require.Equal(t, int64(2), puller.pulls.Load())
}

func TestEnsureWaiterRetriesWhenTheSharedPullIsCancelled(t *testing.T) {
	service, puller := newGatedService(t)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { leader <- service.ensureImage(leaderCtx, testImage) }()
	<-puller.started

	waiter := make(chan error, 1)
	go func() { waiter <- service.ensureImage(context.Background(), testImage) }()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	require.ErrorIs(t, <-leader, context.Canceled, "the cancelled caller sees its own cancellation")

	// The waiter's context is live: it starts its own pull instead of
	// failing with the leader's cancellation.
	<-puller.started
	close(puller.release)
	require.NoError(t, <-waiter)
	require.Equal(t, int64(2), puller.pulls.Load())
}
