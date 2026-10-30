package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletaction "fast-sandbox/internal/fastlet/action"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func newReviewStrictManager(t *testing.T, runtime *admissionRuntime, caller fastletaction.Caller) *SandboxManager {
	t.Helper()
	actions, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	actions.Start(ctx)
	require.Eventually(t, actions.Ready, time.Second, time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actions})
	require.NoError(t, err)
	return manager
}

func TestStrictCreateDeletePendingDoesNotLeakToNextGeneration(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureError = errors.New("injected create failure")
	runtime.ensureEntered = make(chan struct{}, 1)
	runtime.ensureBlock = make(chan struct{})
	caller := &strictDeleteActionCaller{failRemove: true}
	manager := newReviewStrictManager(t, runtime, caller)
	request := ensureRequest("pending-reuse", 1, 1)
	created := make(chan error, 1)
	go func() { _, err := manager.CreateSandbox(context.Background(), request); created <- err }()
	select {
	case <-runtime.ensureEntered:
	case <-time.After(time.Second):
		t.Fatal("Ensure did not start")
	}
	_, err := manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.Error(t, err)
	close(runtime.ensureBlock)
	require.ErrorContains(t, <-created, "cleanup failed")
	manager.mu.RLock()
	require.True(t, manager.createDeletePending[request.Identity.SandboxUID])
	manager.mu.RUnlock()
	caller.mu.Lock()
	caller.failRemove = false
	caller.mu.Unlock()
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.sandboxes[request.Identity.SandboxUID] == nil
	}, time.Second, time.Millisecond)
	manager.mu.RLock()
	pending := manager.createDeletePending[request.Identity.SandboxUID]
	manager.mu.RUnlock()
	require.False(t, pending)
	runtime.mu.Lock()
	runtime.ensureError = nil
	runtime.ensureBlock = nil
	runtime.mu.Unlock()
	// The fake receiver admits a newer owner; the action manager still carries
	// its terminal fence and must compare the new runtime generation.
	caller.mu.Lock()
	caller.ownerSet = false
	caller.retired = false
	caller.mu.Unlock()
	next := ensureRequest(request.Identity.SandboxUID, 2, 1)
	_, err = manager.CreateSandbox(context.Background(), next)
	require.NoError(t, err)
	caller.mu.Lock()
	caller.failRemove = true
	caller.mu.Unlock()
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: next.Identity})
	require.Error(t, err)
	caller.mu.Lock()
	caller.failRemove = false
	caller.mu.Unlock()
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: next.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.sandboxes[next.Identity.SandboxUID] == nil
	}, time.Second, time.Millisecond, "retry must actually release the runtime and slot")
	admission, _, _ := manager.State()
	require.Zero(t, admission.Used)
}

func TestStrictCreateCommitDoesNotAckDeleteBeforeRetirement(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	caller := &blockingStrictDeleteActionCaller{strictDeleteActionCaller: &strictDeleteActionCaller{failRemove: true}, removeEntered: make(chan struct{}, 2), removeRelease: release, removeStarted: make(chan struct{}), ownerReadEntered: make(chan struct{}, 2), ownerReadRelease: release}
	runtime := newAdmissionRuntime()
	manager := newReviewStrictManager(t, runtime, caller)
	request := ensureRequest("commit-delete", 1, 1)
	config := fastletapi.RuntimeSandboxConfig{Identity: request.Identity, ActionsAPIVersion: actionapi.APIVersionV2, Desired: request.Sandbox}
	placeholder := &SandboxMetadata{Config: config, Phase: sandboxStateTerminating}
	manager.mu.Lock()
	manager.sandboxes[request.Identity.SandboxUID] = placeholder
	manager.createDeletePending[request.Identity.SandboxUID] = true
	manager.mu.Unlock()
	metadata := &SandboxMetadata{}
	_, _, _, failure := manager.commitRuntimeCreate(request, fastletapi.EnsureSandboxInput{Sandbox: config}, placeholder, metadata)
	require.NotNil(t, failure)
	select {
	case <-caller.removeEntered:
	case <-time.After(time.Second):
		t.Fatal("REMOVE did not start")
	}
	manager.mu.RLock()
	phase := metadata.Phase
	manager.mu.RUnlock()
	require.Equal(t, sandboxStateTerminating, phase)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := manager.DeleteSandboxContext(ctx, &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.Error(t, err, "a concurrent Delete cannot ACK while retirement is blocked")
	_, deletes := runtime.counts()
	require.Zero(t, deletes)
}

func TestStrictCreateRetiredOwnerIsNonRetryable(t *testing.T) {
	caller := &strictDeleteActionCaller{}
	manager := newReviewStrictManager(t, newAdmissionRuntime(), caller)
	request := ensureRequest("retired-before-create", 1, 1)
	request.Identity.RouteGeneration = 0
	_, err := manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err, "fresh owner normalizes omitted route generation")
	caller.mu.Lock()
	require.Equal(t, int64(1), caller.lastRemove.Owner.RouteGeneration)
	caller.mu.Unlock()
	restarted := newReviewStrictManager(t, newAdmissionRuntime(), caller)
	_, err = restarted.CreateSandbox(context.Background(), request)
	requireFastletCode(t, err, fastletapi.ErrorGenerationFenced)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.False(t, failure.Retryable)
	caller.mu.Lock()
	caller.failRead = true
	caller.mu.Unlock()
	_, err = restarted.CreateSandbox(context.Background(), request)
	requireFastletCode(t, err, fastletapi.ErrorActionUnavailable)
	require.ErrorAs(t, err, &failure)
	require.True(t, failure.Retryable)
}

func TestStrictAdmissionFailureClearsPendingCreateDelete(t *testing.T) {
	caller := &strictDeleteActionCaller{failRead: true}
	manager := newReviewStrictManager(t, newAdmissionRuntime(), caller)
	request := ensureRequest("probe-failure", 1, 1)
	placeholder := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: request.Identity, ActionsAPIVersion: actionapi.APIVersionV2}, Phase: sandboxStateTerminating}
	// A concurrent Delete marked the still-reserved Create before its probe
	// returned an unavailable result. No runtime side effect has started.
	manager.mu.Lock()
	manager.sandboxes[request.Identity.SandboxUID] = placeholder
	manager.createDeletePending[request.Identity.SandboxUID] = true
	manager.mu.Unlock()
	failure, _ := manager.registerDesiredBindings(context.Background(), placeholder, request)
	require.NotNil(t, failure)
	require.True(t, failure.Retryable)
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	require.Nil(t, manager.sandboxes[request.Identity.SandboxUID])
	require.False(t, manager.createDeletePending[request.Identity.SandboxUID])
}

func TestStrictLocalTerminalFenceIsNonRetryable(t *testing.T) {
	caller := &strictDeleteActionCaller{}
	manager := newReviewStrictManager(t, newAdmissionRuntime(), caller)
	request := ensureRequest("local-terminal", 1, 1)
	placeholder := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: request.Identity, ActionsAPIVersion: actionapi.APIVersionV2}, Phase: sandboxStateCreating}
	require.NoError(t, manager.actionManager.DeleteOwned(context.Background(), actionAttachment(placeholder)))
	// Simulate a receiver losing its local record; a retained Fastlet terminal
	// fence must still reject this exact owner even if the probe says absent.
	caller.mu.Lock()
	caller.ownerSet = false
	caller.retired = false
	caller.mu.Unlock()
	manager.mu.Lock()
	manager.sandboxes[request.Identity.SandboxUID] = placeholder
	manager.mu.Unlock()
	failure, _ := manager.registerDesiredBindings(context.Background(), placeholder, request)
	require.NotNil(t, failure)
	require.Equal(t, fastletapi.ErrorGenerationFenced, failure.Code)
	require.False(t, failure.Retryable)
}
