package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletaction "fast-sandbox/internal/fastlet/action"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type terminalFailureBarrierRuntime struct {
	*terminalFailureMockRuntime
	recordEntered  chan struct{}
	recordRelease  chan struct{}
	recordError    error
	cleanupEntered chan struct{}
	cleanupRelease chan struct{}
}

func (r *terminalFailureBarrierRuntime) RecordTerminalFailure(ctx context.Context, identity fastletapi.SandboxIdentity, message string) error {
	if r.recordEntered == nil {
		return r.terminalFailureMockRuntime.RecordTerminalFailure(ctx, identity, message)
	}
	// Model the driver's load/check/save transaction with no lock held over
	// the disk write, so a delayed write could otherwise replace newer state.
	r.mu.Lock()
	metadata := r.sandboxes[identity.SandboxUID]
	if metadata == nil || metadata.Config.Identity != identity {
		r.mu.Unlock()
		return errors.New("terminal identity mismatch")
	}
	state := *metadata
	r.recordCalls++
	r.mu.Unlock()
	close(r.recordEntered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.recordRelease:
	}
	if r.recordError != nil {
		return r.recordError
	}
	state.Phase, state.RuntimeMessage = sandboxStateRuntimeFailed, message
	r.mu.Lock()
	r.sandboxes[identity.SandboxUID] = &state
	r.mu.Unlock()
	return nil
}

func (r *terminalFailureBarrierRuntime) CleanupTerminalFailure(ctx context.Context, identity fastletapi.SandboxIdentity) error {
	if r.cleanupEntered != nil {
		close(r.cleanupEntered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.cleanupRelease:
		}
	}
	return r.terminalFailureMockRuntime.CleanupTerminalFailure(ctx, identity)
}

func TestTerminalFailureWriteFencesDeleteAndReplacement(t *testing.T) {
	for _, recordFails := range []bool{false, true} {
		name := "record-succeeds"
		if recordFails {
			name = "record-fails"
		}
		t.Run(name, func(t *testing.T) {
			runtime := &terminalFailureBarrierRuntime{
				terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()},
				recordEntered:              make(chan struct{}), recordRelease: make(chan struct{}),
			}
			if recordFails {
				runtime.recordError = errors.New("injected terminal record write failure")
			}
			var release sync.Once
			releaseRecord := func() { release.Do(func() { close(runtime.recordRelease) }) }
			t.Cleanup(releaseRecord)
			manager := newAdmissionManager(t, runtime, 1)
			request := ensureRequest("terminal-write", 7, 1)
			_, err := manager.CreateSandbox(t.Context(), request)
			require.NoError(t, err)
			runtime.mu.Lock()
			exited := *runtime.sandboxes[request.Identity.SandboxUID]
			exited.Phase = "Stopped"
			runtime.sandboxes[request.Identity.SandboxUID] = &exited
			runtime.mu.Unlock()
			heartbeatDone := make(chan struct{})
			go func() {
				manager.GetSandboxStatuses(t.Context())
				close(heartbeatDone)
			}()
			select {
			case <-runtime.recordEntered:
			case <-time.After(time.Second):
				t.Fatal("heartbeat did not reach terminal persistence")
			}

			_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
			require.NoError(t, err)
			require.Never(t, runtime.GetDeleteCalled, 50*time.Millisecond, time.Millisecond,
				"Delete must wait until the old identity's write has completed")
			replacement := ensureRequest(request.Identity.SandboxUID, 8, 1)
			_, err = manager.CreateSandbox(t.Context(), replacement)
			require.Error(t, err, "replacement cannot start while the old write is pending")

			releaseRecord()
			select {
			case <-heartbeatDone:
			case <-time.After(time.Second):
				t.Fatal("heartbeat did not finish after terminal persistence")
			}
			require.Eventually(t, func() bool {
				manager.mu.RLock()
				defer manager.mu.RUnlock()
				return manager.sandboxes[request.Identity.SandboxUID] == nil
			}, time.Second, time.Millisecond, "Delete must resume after either write success or failure")
			_, err = manager.CreateSandbox(t.Context(), replacement)
			require.NoError(t, err)
			manager.preserveExitedRuntime(t.Context(), request.Identity.SandboxUID, request.Identity, "stale heartbeat")
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			require.Equal(t, replacement.Identity, runtime.sandboxes[request.Identity.SandboxUID].Config.Identity)
			require.Equal(t, 1, runtime.recordCalls, "a stale heartbeat must be rejected before writing runtime state")
		})
	}
}

func TestTerminalFailureCleanupHandsOffConcurrentDelete(t *testing.T) {
	for _, strict := range []bool{false, true} {
		name := "legacy-actions"
		if strict {
			name = "strict-actions"
		}
		t.Run(name, func(t *testing.T) {
			runtime := &terminalFailureBarrierRuntime{
				terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()},
				cleanupEntered:             make(chan struct{}), cleanupRelease: make(chan struct{}),
			}
			var release sync.Once
			releaseCleanup := func() { release.Do(func() { close(runtime.cleanupRelease) }) }
			t.Cleanup(releaseCleanup)
			config := SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a"}
			version := actionapi.APIVersion
			if strict {
				version = actionapi.APIVersionV2
				actions, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
					Name: "egress", TargetHTTPPort: 18080,
				}}, version, &strictDeleteActionCaller{})
				require.NoError(t, err)
				config.ActionManager = actions
			}
			manager, err := NewSandboxManagerWithConfig(runtime, config)
			require.NoError(t, err)
			request := ensureRequest("terminal-cleanup", 7, 1)
			metadata := &SandboxMetadata{
				Config: fastletapi.RuntimeSandboxConfig{Identity: request.Identity, ActionsAPIVersion: version, Desired: request.Sandbox},
				Phase:  sandboxStateRunning, AppliedGeneration: 7,
			}
			manager.sandboxes[request.Identity.SandboxUID] = metadata
			exited := *metadata
			exited.Phase = "Stopped"
			runtime.sandboxes[request.Identity.SandboxUID] = &exited
			manager.GetSandboxStatuses(t.Context())
			select {
			case <-runtime.cleanupEntered:
			case <-time.After(time.Second):
				t.Fatal("terminal worker did not reach physical cleanup")
			}

			_, err = manager.DeleteSandboxContext(t.Context(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
			require.NoError(t, err)
			require.False(t, runtime.GetDeleteCalled(), "ordinary Delete must not overlap terminal cleanup")
			releaseCleanup()
			require.Eventually(t, func() bool {
				manager.mu.RLock()
				defer manager.mu.RUnlock()
				return manager.sandboxes[request.Identity.SandboxUID] == nil
			}, time.Second, time.Millisecond, "the terminal worker must hand off both Terminating and Deleting")
			require.True(t, runtime.GetDeleteCalled())
			require.False(t, runtime.HasSandbox(request.Identity.SandboxUID))
		})
	}
}

type exitedCaptureRuntime struct{ *terminalFailureMockRuntime }

func (r *exitedCaptureRuntime) ReconcileSandbox(context.Context, fastletapi.SandboxIdentity) error {
	return nil
}

func TestRootfsConfirmedExitLeavesQuarantineThroughTerminalCleanup(t *testing.T) {
	r := &exitedCaptureRuntime{terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()}}
	m, err := NewSandboxManagerWithConfig(r, SandboxManagerConfig{Capacity: 2})
	require.NoError(t, err)
	identity := fastletapi.SandboxIdentity{SandboxUID: "dead-capture", FastletPodUID: "pod", RuntimeInstanceID: "runtime"}
	metadata := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: identity}, Phase: sandboxStateRunning, RecoveryRequired: true}
	m.sandboxes[identity.SandboxUID] = metadata
	r.sandboxes[identity.SandboxUID] = &SandboxMetadata{Config: metadata.Config, Phase: sandboxStateRuntimeFailed, RuntimeMessage: "capture source exited"}
	m.refreshRuntimeRecovery(t.Context(), identity, false)
	require.Eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return metadata.Phase == sandboxStateRuntimeFailed && !metadata.RecoveryRequired
	}, time.Second, time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Equal(t, 1, r.recordCalls)
	require.Equal(t, 1, r.cleanupCalls)
}
