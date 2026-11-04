package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func newExitedRuntimePhaseFixture(t *testing.T, runtime *terminalFailureBarrierRuntime, phase string) (*SandboxManager, *fastletapi.CreateSandboxRequest, *SandboxMetadata) {
	t.Helper()
	manager, profile := newResourceAwareAdmissionManager(t, runtime, 1, "4", "20Gi")
	request := resourceAwareRequest("terminal-created-phase", profile)
	_, err := manager.CreateSandbox(t.Context(), request)
	require.NoError(t, err)
	manager.mu.Lock()
	metadata := manager.sandboxes[request.Identity.SandboxUID]
	metadata.Phase = phase
	manager.mu.Unlock()
	// The driver's durable metadata is independent of Fastlet's local Infra,
	// Route and Action phase, just as it is for a real Firecracker instance.
	runtime.mu.Lock()
	exited := *runtime.sandboxes[request.Identity.SandboxUID]
	exited.Phase = "Stopped"
	runtime.sandboxes[request.Identity.SandboxUID] = &exited
	runtime.mu.Unlock()
	return manager, request, metadata
}

func terminalPhaseBarrierRelease(t *testing.T, barrier chan struct{}) func() {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(barrier) }) }
	t.Cleanup(release)
	return release
}

func awaitTerminalPhaseSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func requireTerminalPhaseViews(t *testing.T, manager *SandboxManager, request *fastletapi.CreateSandboxRequest) {
	t.Helper()
	heartbeat := manager.GetSandboxStatuses(t.Context())
	require.Len(t, heartbeat, 1)
	inspected, err := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	replayed, err := manager.CreateSandbox(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionExisting, replayed.Disposition)
	for _, status := range []*fastletapi.SandboxStatus{&heartbeat[0], inspected.Sandbox, replayed.Sandbox} {
		require.NotNil(t, status)
		require.Equal(t, request.Identity.SandboxUID, status.SandboxID)
		require.Equal(t, request.Identity.InstanceGeneration, status.InstanceGeneration)
		require.Equal(t, request.Identity.RuntimeInstanceID, status.RuntimeInstanceID)
		require.Equal(t, request.Identity.AssignmentAttempt, status.AssignmentAttempt)
		require.Equal(t, request.Identity.RouteGeneration, status.RouteGeneration)
		require.Equal(t, fastletapi.RuntimeStateFailed, status.Runtime.State)
		require.Equal(t, inspected.Sandbox.Runtime.Message, status.Runtime.Message)
		require.Equal(t, fastletapi.DataPlaneStateFailed, status.DataPlane.State)
	}
}

func requireTerminalPhaseCapacityHeld(t *testing.T, manager *SandboxManager, committed fastletapi.SandboxResourceVector) {
	t.Helper()
	admission, _, _ := manager.State()
	require.Equal(t, 1, admission.Used, "capacity remains committed until physical cleanup completes")
	require.NotNil(t, admission.Resources)
	require.Equal(t, committed, admission.Resources.Committed)
	cpu, err := resource.ParseQuantity(admission.Resources.Committed.CPU)
	require.NoError(t, err)
	memory, err := resource.ParseQuantity(admission.Resources.Committed.Memory)
	require.NoError(t, err)
	require.Positive(t, cpu.Sign())
	require.Positive(t, memory.Sign())
}

func awaitTerminalPhaseCapacityReleased(t *testing.T, manager *SandboxManager) {
	t.Helper()
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, time.Millisecond)
	admission, _, _ := manager.State()
	require.NotNil(t, admission.Resources)
	cpu, err := resource.ParseQuantity(admission.Resources.Committed.CPU)
	require.NoError(t, err)
	memory, err := resource.ParseQuantity(admission.Resources.Committed.Memory)
	require.NoError(t, err)
	require.Zero(t, cpu.Sign())
	require.Zero(t, memory.Sign())
}

func TestTerminalFailureCoversEveryCreatedRuntimePhase(t *testing.T) {
	for _, phase := range []string{
		sandboxStateInfraPending, sandboxStateInitializingInfra, sandboxStateInfraUnavailable,
		sandboxStateRoutePending, sandboxStatePublishingRoute, sandboxStateRouteUnavailable,
		sandboxStateActionPending, sandboxStateActionUnavailable, sandboxStateRunning,
	} {
		t.Run(phase, func(t *testing.T) {
			runtime := &terminalFailureBarrierRuntime{
				terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()},
				cleanupEntered:             make(chan struct{}), cleanupRelease: make(chan struct{}),
			}
			releaseCleanup := terminalPhaseBarrierRelease(t, runtime.cleanupRelease)
			manager, request, _ := newExitedRuntimePhaseFixture(t, runtime, phase)
			initial, _, _ := manager.State()
			require.NotNil(t, initial.Resources)
			committed := initial.Resources.Committed
			first := manager.GetSandboxStatuses(t.Context())
			require.Len(t, first, 1)
			require.Equal(t, fastletapi.RuntimeStateFailed, first[0].Runtime.State)
			awaitTerminalPhaseSignal(t, runtime.cleanupEntered, "terminal failure did not reach physical cleanup")
			for range 3 {
				requireTerminalPhaseViews(t, manager, request)
				requireTerminalPhaseCapacityHeld(t, manager, committed)
			}
			runtime.mu.Lock()
			recordCalls, cleanupCalls := runtime.recordCalls, runtime.cleanupCalls
			durable := *runtime.sandboxes[request.Identity.SandboxUID]
			runtime.mu.Unlock()
			require.Equal(t, 1, recordCalls, "repeated observations must not record the same failure again")
			require.Zero(t, cleanupCalls)
			require.Equal(t, sandboxStateRuntimeFailed, durable.Phase)
			require.Equal(t, request.Identity, durable.Config.Identity)

			releaseCleanup()
			awaitTerminalPhaseCapacityReleased(t, manager)
			for range 3 {
				requireTerminalPhaseViews(t, manager, request)
			}
			runtime.mu.Lock()
			recordCalls, cleanupCalls = runtime.recordCalls, runtime.cleanupCalls
			_, retained := runtime.sandboxes[request.Identity.SandboxUID]
			runtime.mu.Unlock()
			require.Equal(t, 1, recordCalls)
			require.Equal(t, 1, cleanupCalls)
			require.True(t, retained, "the complete terminal identity survives physical cleanup")
			require.False(t, runtime.GetDeleteCalled(), "observations cannot acknowledge the terminal identity")
		})
	}
}

func TestTerminalFailureRetriesRecordingFromCreatedPhases(t *testing.T) {
	for _, phase := range []string{sandboxStateInitializingInfra, sandboxStatePublishingRoute, sandboxStateRunning} {
		t.Run(phase, func(t *testing.T) {
			runtime := &terminalFailureBarrierRuntime{
				terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()},
				recordEntered:              make(chan struct{}), recordRelease: make(chan struct{}),
				recordError:    errors.New("injected durable failure write error"),
				cleanupEntered: make(chan struct{}), cleanupRelease: make(chan struct{}),
			}
			close(runtime.recordRelease)
			releaseCleanup := terminalPhaseBarrierRelease(t, runtime.cleanupRelease)
			manager, request, metadata := newExitedRuntimePhaseFixture(t, runtime, phase)
			initial, _, _ := manager.State()
			require.NotNil(t, initial.Resources)
			workerContext, cancelWorker := context.WithCancel(t.Context())
			t.Cleanup(cancelWorker)
			manager.mu.Lock()
			manager.dataPlaneWorkers[request.Identity.SandboxUID] = dataPlaneWorker{metadata: metadata, cancel: cancelWorker}
			manager.mu.Unlock()

			manager.GetSandboxStatuses(t.Context())
			require.ErrorIs(t, workerContext.Err(), context.Canceled, "data-plane work is canceled before the durable write")
			inspected, err := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
			require.NoError(t, err)
			require.Equal(t, fastletapi.RuntimeStateReady, inspected.Sandbox.Runtime.State,
				"failed persistence must not expose a terminal record for Controller acknowledgement")
			requireTerminalPhaseCapacityHeld(t, manager, initial.Resources.Committed)
			runtime.mu.Lock()
			recordCalls, cleanupCalls := runtime.recordCalls, runtime.cleanupCalls
			durablePhase := runtime.sandboxes[request.Identity.SandboxUID].Phase
			runtime.mu.Unlock()
			require.Equal(t, 1, recordCalls)
			require.Zero(t, cleanupCalls)
			require.Equal(t, "Stopped", durablePhase)
			select {
			case <-runtime.cleanupEntered:
				t.Fatal("physical cleanup started before terminal persistence succeeded")
			default:
			}

			// The first heartbeat is synchronous and its writer has returned;
			// subsequent writes now use the normal persistent mock handler.
			runtime.recordEntered, runtime.recordError = nil, nil
			manager.GetSandboxStatuses(t.Context())
			awaitTerminalPhaseSignal(t, runtime.cleanupEntered, "a later heartbeat did not retry the failed durable write")
			requireTerminalPhaseViews(t, manager, request)
			requireTerminalPhaseCapacityHeld(t, manager, initial.Resources.Committed)
			releaseCleanup()
			awaitTerminalPhaseCapacityReleased(t, manager)
			requireTerminalPhaseViews(t, manager, request)
			runtime.mu.Lock()
			recordCalls, cleanupCalls = runtime.recordCalls, runtime.cleanupCalls
			runtime.mu.Unlock()
			require.Equal(t, 2, recordCalls)
			require.Equal(t, 1, cleanupCalls)
		})
	}
}

func TestTerminalFailureRecordingFencesConcurrentHealthTransition(t *testing.T) {
	runtime := &terminalFailureBarrierRuntime{
		terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()},
		recordEntered:              make(chan struct{}), recordRelease: make(chan struct{}),
		cleanupEntered: make(chan struct{}), cleanupRelease: make(chan struct{}),
	}
	releaseRecord := terminalPhaseBarrierRelease(t, runtime.recordRelease)
	releaseCleanup := terminalPhaseBarrierRelease(t, runtime.cleanupRelease)
	manager, request, metadata := newExitedRuntimePhaseFixture(t, runtime, sandboxStateRunning)
	initial, _, _ := manager.State()
	require.NotNil(t, initial.Resources)
	workerContext, cancelWorker := context.WithCancel(t.Context())
	t.Cleanup(cancelWorker)
	manager.mu.Lock()
	manager.dataPlaneWorkers[request.Identity.SandboxUID] = dataPlaneWorker{metadata: metadata, cancel: cancelWorker}
	manager.mu.Unlock()
	heartbeatDone := make(chan struct{})
	go func() {
		manager.GetSandboxStatuses(t.Context())
		close(heartbeatDone)
	}()
	awaitTerminalPhaseSignal(t, runtime.recordEntered, "heartbeat did not reach terminal persistence")
	require.ErrorIs(t, workerContext.Err(), context.Canceled, "the data-plane worker must be canceled before writing")

	// Model a health request that was already in flight and returns after
	// cancellation, while terminal persistence is still blocked.
	manager.markDataPlaneUnhealthy(metadata, errors.New("late health probe failure"))
	manager.mu.RLock()
	phase := metadata.Phase
	manager.mu.RUnlock()
	require.Equal(t, sandboxStateRunning, phase, "a late health callback cannot change the phase while terminal recording owns it")
	inspected, err := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Equal(t, fastletapi.RuntimeStateReady, inspected.Sandbox.Runtime.State,
		"Controller cannot acknowledge a terminal identity before its write completes")
	requireTerminalPhaseCapacityHeld(t, manager, initial.Resources.Committed)
	runtime.mu.Lock()
	cleanupCalls := runtime.cleanupCalls
	durablePhase := runtime.sandboxes[request.Identity.SandboxUID].Phase
	runtime.mu.Unlock()
	require.Zero(t, cleanupCalls)
	require.Equal(t, "Stopped", durablePhase)

	releaseRecord()
	awaitTerminalPhaseSignal(t, heartbeatDone, "heartbeat did not finish after the terminal write")
	awaitTerminalPhaseSignal(t, runtime.cleanupEntered, "terminal write did not transfer ownership to cleanup")
	manager.markDataPlaneUnhealthy(metadata, errors.New("another late health probe failure"))
	requireTerminalPhaseViews(t, manager, request)
	requireTerminalPhaseCapacityHeld(t, manager, initial.Resources.Committed)
	releaseCleanup()
	awaitTerminalPhaseCapacityReleased(t, manager)
	requireTerminalPhaseViews(t, manager, request)
	runtime.mu.Lock()
	recordCalls, cleanupCalls := runtime.recordCalls, runtime.cleanupCalls
	runtime.mu.Unlock()
	require.Equal(t, 1, recordCalls)
	require.Equal(t, 1, cleanupCalls)
}
