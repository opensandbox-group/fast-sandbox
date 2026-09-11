package firecracker

import (
	"context"
	"sync"
	"testing"

	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

// fakeAgentClient is a scriptable AgentClient for wiring tests.
type fakeAgentClient struct {
	mu         sync.Mutex
	pins       []string
	pinReqs    []string
	unpins     []string
	unpinReqs  []string
	releases   []string
	healthErr  error
	digest     string
	publishes  []string
	publishOut PublishOutcome
	publishErr error
}

func (f *fakeAgentClient) PinImage(_ context.Context, requestID, image string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins = append(f.pins, image)
	f.pinReqs = append(f.pinReqs, requestID)
	if f.digest == "" {
		return "sha256:" + image, nil
	}
	return f.digest, nil
}

func (f *fakeAgentClient) UnpinImage(_ context.Context, requestID, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpins = append(f.unpins, image)
	f.unpinReqs = append(f.unpinReqs, requestID)
	return nil
}

func (f *fakeAgentClient) PublishImage(_ context.Context, _, key, _ string) (PublishOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, key)
	if f.publishErr != nil {
		return PublishOutcome{}, f.publishErr
	}
	return f.publishOut, nil
}

func (f *fakeAgentClient) LeaseDevices(context.Context, string, *fastletapi.RuntimeSandboxConfig) (Lease, error) {
	return Lease{}, nil
}

func (f *fakeAgentClient) ReleaseDevices(_ context.Context, _, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, leaseID)
	return nil
}

func (f *fakeAgentClient) ListLeases(context.Context) ([]Lease, error) { return nil, nil }
func (f *fakeAgentClient) Compatibility(context.Context) (string, error) {
	return "native-stage-1", nil
}
func (f *fakeAgentClient) Health(context.Context) error {
	return f.healthErr
}

func (f *fakeAgentClient) snapshot() (pins, unpins, releases []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pins...), append([]string(nil), f.unpins...), append([]string(nil), f.releases...)
}

// installAgent wires a fake agent into a driver fixture.
func (f *driverFixture) installAgent(agent *fakeAgentClient) {
	f.driver.newAgentClient = func(string) (AgentClient, error) { return agent, nil }
	f.driver.agentSocket = "/run/fast-sandbox/firecracker/runtime.sock"
	f.driver.podUID = "pod-1"
}

func TestPullImageProxiesToPinImage(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &fakeAgentClient{}
	fixture.installAgent(agent)

	require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Spec.Image))

	pins, _, _ := agent.snapshot()
	require.Equal(t, []string{fixture.sandboxSpec.Spec.Image}, pins)
	// The request id is the pod-scoped warm-pull key of the image, so
	// retries of THIS fastlet replay the first pin instead of double-
	// counting, while another fastlet pod on the same node keeps its own
	// key (the agent journal rejects a request id committed by a different
	// pod UID).
	require.Equal(t, "warm-pull-pod-1-"+imageKey(fixture.sandboxSpec.Spec.Image), agent.pinReqs[0])
}

func TestPullImageIdempotentAcrossCalls(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &fakeAgentClient{}
	fixture.installAgent(agent)

	require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Spec.Image))
	require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Spec.Image))

	pins, _, _ := agent.snapshot()
	require.Len(t, pins, 2)
	require.Equal(t, agent.pinReqs[0], agent.pinReqs[1])
}

func TestPullImageLocalModeWithoutAgent(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Spec.Image))

	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	missing := fixture.sandboxSpec.Spec.Image + "-missing"
	require.ErrorIs(t, fixture.driver.PullImage(context.Background(), missing), ErrImageNotReady)
}

func TestPullImageAgentUnreachableFallsBackToLocal(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	// The agent socket exists but nothing listens: dial fails with
	// errAgentUnreachable and the driver falls back to the local cache.
	fixture.driver.newAgentClient = func(socket string) (AgentClient, error) {
		return NewAgentClient(testAgentSocketPath(t), "tenant-a", "pod-1")
	}
	fixture.driver.agentSocket = "/unused"

	require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Spec.Image))
}

func TestProbeCapabilitiesAgentHealthFailureDegrades(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &fakeAgentClient{healthErr: errAgentUnreachable}
	fixture.installAgent(agent)

	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, "Degraded", string(report.State))
	require.Equal(t, "AgentUnavailable", report.Reason)
}

func TestProbeCapabilitiesAgentReadyKeepsHostChecks(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &fakeAgentClient{}
	fixture.installAgent(agent)

	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, "Ready", string(report.State))
	require.Equal(t, "RuntimeDriverReady", report.Reason)
}

func TestProbeCapabilitiesLocalModeUnchanged(t *testing.T) {
	fixture := newDriverFixture(t)
	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, "Ready", string(report.State))
	require.Equal(t, "RuntimeDriverReady", report.Reason)
}

func TestDeleteSandboxReleasesAndUnpins(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	agent := &fakeAgentClient{}
	fixture.installAgent(agent)
	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	fixture.driver.rememberLease(fixture.sandboxSpec.Identity.SandboxUID, "lease-1")

	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), fixture.sandboxSpec.Identity.SandboxUID))

	pins, unpins, releases := agent.snapshot()
	require.Empty(t, pins)
	require.Equal(t, []string{fixture.sandboxSpec.Spec.Image}, unpins)
	require.Equal(t, []string{"lease-1"}, releases)
	_, ok := fixture.driver.leaseForSandbox(fixture.sandboxSpec.Identity.SandboxUID)
	require.False(t, ok)
}

func TestDeleteSandboxLocalModeSkipsAgent(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), fixture.sandboxSpec.Identity.SandboxUID))
}

func TestSetAgentSocketEmptySwitchesToLocalMode(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.installAgent(&fakeAgentClient{})
	fixture.driver.SetAgentSocket("")
	client, err := fixture.driver.agentClientOrNil()
	require.NoError(t, err)
	require.Nil(t, client)
}

// TestEnsureSandboxColdCreateNeverDeliversSynchronously: a cold image with a
// configured agent must NOT block the create on the node-side pull. The
// driver reports ErrImageNotReady without touching the agent — asynchronous
// delivery belongs to DeliverImage (image_delivery_test.go), which Fastlet
// drives before the Sandbox can boot.
func TestEnsureSandboxColdCreateNeverDeliversSynchronously(t *testing.T) {
	fixture := newDriverFixture(t)
	agent := &fakeAgentClient{}
	fixture.installAgent(agent)

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorIs(t, err, ErrImageNotReady)
	pins, _, _ := agent.snapshot()
	require.Empty(t, pins, "EnsureSandbox must not pull a missing image inline")
}

func TestEnsureSandboxCachedImageNeverPins(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	agent := &fakeAgentClient{}
	fixture.installAgent(agent)

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	pins, _, _ := agent.snapshot()
	require.Empty(t, pins, "a cached image must not pin on create (warm pull pins once, delete unpins once)")
}
