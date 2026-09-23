package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// materializingAgent is a fake agent whose PinImage commits the rootfs into
// the shared cache root, mirroring the real agent's pull-commit behavior.
// noCommit simulates an agent journal replay: PinImage succeeds without
// materializing any artifact.
type materializingAgent struct {
	*fakeAgentClient
	cacheRoot string
	mu        sync.Mutex
	pinErr    error
	pinCount  int
	noCommit  bool
}

func (m *materializingAgent) PinImage(ctx context.Context, requestID, image string) (string, error) {
	m.mu.Lock()
	err := m.pinErr
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	digest, err := m.fakeAgentClient.PinImage(ctx, requestID, image)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.pinCount++
	noCommit := m.noCommit
	m.mu.Unlock()
	if noCommit {
		return digest, nil
	}
	dir := filepath.Join(m.cacheRoot, imageCacheDir, imageKey(image))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	// The real agent commits the complete restore set; a partial commit
	// (rootfs only) must NOT count as delivered.
	for name, payload := range map[string]string{
		rootfsImageName:     "rootfs-image-data",
		vmstateSnapshotName: "vmstate-snapshot-data",
		memorySnapshotName:  "memory-snapshot-data",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(payload), 0o640); err != nil {
			return "", err
		}
	}
	return digest, nil
}

// PinCheckpoint materializes the checkpoint set through the same fake
// commit path; the cache is keyed by the checkpoint reference.
func (m *materializingAgent) PinCheckpoint(ctx context.Context, requestID, reference, _, _ string) (string, error) {
	return m.PinImage(ctx, requestID, reference)
}

func (m *materializingAgent) setPinErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pinErr = err
}

func (m *materializingAgent) deliveredPins() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pinCount
}

// installMaterializingAgent wires a materializing agent into a fixture.
func (f *driverFixture) installMaterializingAgent(agent *materializingAgent) {
	f.driver.newAgentClient = func(string) (AgentClient, error) { return agent, nil }
	f.driver.agentSocket = "/run/fast-sandbox/firecracker/runtime.sock"
	f.driver.podUID = "pod-1"
}

func TestDeliverImageReportsDeliveredWhenCached(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)

	status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivered, status)
}

func TestDeliverImageLocalModeMissingIsImpossible(t *testing.T) {
	fixture := newDriverFixture(t)

	status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Equal(t, runtimecontract.ImageDeliveryStatus(""), status)

	status, err = fixture.driver.DeliverImage(context.Background(), "")
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Equal(t, runtimecontract.ImageDeliveryStatus(""), status)
}

func TestDeliverImageKicksBackgroundAttemptUntilCommitted(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &materializingAgent{fakeAgentClient: &fakeAgentClient{}, cacheRoot: fixture.stateRoot}
	fixture.installMaterializingAgent(agent)

	started := time.Now()
	status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivering, status)

	require.Eventually(t, func() bool {
		_, resolveErr := resolveRootfsImage(fixture.stateRoot, fixture.sandboxSpec.Spec.Image)
		return resolveErr == nil
	}, 5*time.Second, 20*time.Millisecond, "background delivery must commit the image")

	require.GreaterOrEqual(t, agent.deliveredPins(), 1)
	require.Less(t, time.Since(started), 5*time.Second, "DeliverImage must never block on the transfer")

	status, err = fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivered, status)
}

// TestDeliverImageReportsReplayWithoutLocalCommitAsFailure: the agent journal
// replays a committed PinImage without re-pulling, so a cache purged under a
// committed pin must surface as a terminal attempt error (never as a success
// that loops forever).
func TestDeliverImageReportsReplayWithoutLocalCommitAsFailure(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &materializingAgent{fakeAgentClient: &fakeAgentClient{}, cacheRoot: fixture.stateRoot, noCommit: true}
	fixture.installMaterializingAgent(agent)

	status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivering, status)

	var reported error
	require.Eventually(t, func() bool {
		_, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
		if err != nil {
			reported = err
			return true
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "a replay without a local commit point must fail the attempt")
	require.ErrorIs(t, reported, ErrImageNotReady)
	require.Contains(t, reported.Error(), "no committed restore set")
	require.NotZero(t, agent.deliveredPins())
}

func TestDeliverImageIsSingleFlight(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &materializingAgent{fakeAgentClient: &fakeAgentClient{}, cacheRoot: fixture.stateRoot}
	fixture.installMaterializingAgent(agent)

	var statuses = make(chan runtimecontract.ImageDeliveryStatus, 2)
	for range 2 {
		go func() {
			status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
			require.NoError(t, err)
			statuses <- status
		}()
	}
	for range 2 {
		status := <-statuses
		// The fake delivery is fast: a caller either coalesces into the
		// single in-flight attempt (Delivering) or polls after the commit
		// (Delivered). Both are correct single-flight outcomes.
		require.Contains(t,
			[]runtimecontract.ImageDeliveryStatus{runtimecontract.ImageDelivering, runtimecontract.ImageDelivered},
			status)
	}
	require.Eventually(t, func() bool {
		_, resolveErr := resolveRootfsImage(fixture.stateRoot, fixture.sandboxSpec.Spec.Image)
		return resolveErr == nil
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, 1, agent.deliveredPins(), "concurrent deliveries must coalesce into one attempt")
}

func TestDeliverImageReportsFailureOnceThenRecoversAfterWindow(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &materializingAgent{fakeAgentClient: &fakeAgentClient{}, cacheRoot: fixture.stateRoot}
	fixture.installMaterializingAgent(agent)
	// Shorten the sticky window so the test observes failure AND the
	// window-expiry retry without waiting real minutes.
	fixture.driver.imageDeliveryFailureWindow = 300 * time.Millisecond
	boom := errors.New("remote image index 404")

	agent.setPinErr(boom)
	status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivering, status)

	// The finished attempt failure is reported at most once per window.
	var reported error
	require.Eventually(t, func() bool {
		_, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
		if err != nil {
			reported = err
			return true
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the failed attempt must surface to polling callers")
	require.ErrorIs(t, reported, boom)

	// Within the window the failure stays sticky (no doomed new attempt).
	_, err = fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.ErrorIs(t, err, boom)

	// After the window a fresh attempt runs; once the remote recovers the
	// image is delivered.
	time.Sleep(400 * time.Millisecond)
	agent.setPinErr(nil)
	status, err = fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivering, status)
	require.Eventually(t, func() bool {
		_, resolveErr := resolveRootfsImage(fixture.stateRoot, fixture.sandboxSpec.Spec.Image)
		return resolveErr == nil
	}, 5*time.Second, 20*time.Millisecond)
}

// TestDeliverImagePartialCacheIsNotDelivered: a cache with only the rootfs
// committed (snapshots still transferring) must keep reporting Delivering and
// re-attempt delivery, never Delivered — the readiness criterion is the
// complete restore set, the same one EnsureSandbox's restore path enforces.
// This is the regression test for the cold-create slot burn: a partial-cache
// Delivered let the boot worker poll EnsureSandbox, whose post-acquire
// snapshot check then release-destroyed a network slot per attempt.
func TestDeliverImagePartialCacheIsNotDelivered(t *testing.T) {
	fixture := newDriverFixture(t)
	image := fixture.sandboxSpec.Spec.Image
	dir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))

	// Local mode (no agent): a partial cache is NOT ready, so delivery is
	// impossible and the call reports ErrImageNotReady — never Delivered.
	status, err := fixture.driver.DeliverImage(context.Background(), image)
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Equal(t, runtimecontract.ImageDeliveryStatus(""), status)

	// The same criterion gates the synchronous create path: a partial cache
	// parks (or fails in local mode) before any network slot is acquired.
	require.ErrorIs(t, verifyRestorableImage(fixture.stateRoot, image), ErrImageNotReady)
}

func TestDeliverCheckpointMaterializesAndReportsDelivered(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &materializingAgent{fakeAgentClient: &fakeAgentClient{}, cacheRoot: fixture.stateRoot}
	fixture.installMaterializingAgent(agent)

	restore := &fastletapi.RestoreSpec{
		ManifestRef: "s3://bucket/publish/0123456789abcdef/manifest.json", ArtifactDigest: "checkpoint-digest",
	}
	reference := artifacts.CheckpointReference(restore.ArtifactDigest)

	status, err := fixture.driver.DeliverCheckpoint(context.Background(), restore)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivering, status, "a cold checkpoint is delivered in the background")
	require.Eventually(t, func() bool {
		return verifyRestorableImage(fixture.stateRoot, reference) == nil
	}, 2*time.Second, 10*time.Millisecond)

	status, err = fixture.driver.DeliverCheckpoint(context.Background(), restore)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivered, status)
	require.Equal(t, 1, agent.deliveredPins(), "a committed checkpoint cache is not pinned again")
}

func TestDeliverCheckpointRejectsIncompleteAddress(t *testing.T) {
	fixture := newDriverFixture(t)
	_, err := fixture.driver.DeliverCheckpoint(context.Background(), &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/m.json"})
	require.ErrorContains(t, err, "checkpoint manifestRef and artifactDigest are required")
}

func TestRestoreReferenceUsesCheckpointDigest(t *testing.T) {
	reference, err := restoreReference(fastletapi.SandboxSpec{
		Image:   "app:v1",
		Restore: &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/m.json", ArtifactDigest: "digest-cp"},
	})
	require.NoError(t, err)
	require.Equal(t, artifacts.CheckpointReference("digest-cp"), reference)

	reference, err = restoreReference(fastletapi.SandboxSpec{Image: "app:v1"})
	require.NoError(t, err)
	require.Equal(t, "app:v1", reference)

	_, err = restoreReference(fastletapi.SandboxSpec{Image: "app:v1", Restore: &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/m.json"}})
	require.ErrorContains(t, err, "restore requires manifestRef and artifactDigest")
}
