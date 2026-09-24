package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

type admissionRuntime struct {
	mu            sync.Mutex
	sandboxes     map[string]*SandboxMetadata
	managed       []*SandboxMetadata
	ensureCalls   int
	deleteCalls   int
	ensureError   error
	deleteError   error
	ensureEntered chan struct{}
	ensureBlock   chan struct{}
	deleteEntered chan struct{}
	deleteBlock   chan struct{}
	pullEntered   chan struct{}
	pullBlock     chan struct{}
	pulledImages  []string
	images        []string
	resourceReady *bool
}

type failingDeleteActionCaller struct {
	deleteCalls atomic.Int32
}

type restartAwareActionCaller struct {
	mu       sync.Mutex
	instance string
	block    <-chan struct{}
}

func (c *restartAwareActionCaller) Status(context.Context, int32) (actionapi.HandlerStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return actionapi.HandlerStatus{APIVersion: actionapi.APIVersion, Ready: true, InstanceID: c.instance}, nil
}

func (c *restartAwareActionCaller) Invoke(ctx context.Context, _ int32, _ actionapi.Request) error {
	c.mu.Lock()
	block := c.block
	c.mu.Unlock()
	if block == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-block:
		return nil
	}
}

func (c *restartAwareActionCaller) setInstance(instance string) {
	c.mu.Lock()
	c.instance = instance
	c.mu.Unlock()
}

func (c *restartAwareActionCaller) setInvokeBlock(block <-chan struct{}) {
	c.mu.Lock()
	c.block = block
	c.mu.Unlock()
}

func (*failingDeleteActionCaller) Status(context.Context, int32) (actionapi.HandlerStatus, error) {
	return actionapi.HandlerStatus{APIVersion: actionapi.APIVersion, Ready: true, InstanceID: "handler-a"}, nil
}

func (c *failingDeleteActionCaller) Invoke(_ context.Context, _ int32, request actionapi.Request) error {
	if request.Operation == actionapi.OperationRemoveBinding {
		c.deleteCalls.Add(1)
		return errors.New("injected Handler RemoveBinding failure")
	}
	return nil
}

func TestInspectUsesProbeInvalidatedActionStatusBeforeControllerProjection(t *testing.T) {
	caller := &restartAwareActionCaller{instance: "handler-1"}
	actionManager, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	_, _, err = actionManager.Reconcile(context.Background(), fastletaction.Attachment{
		ID: "attachment-a", SandboxUID: "uid-a", SandboxName: "sandbox-a", Namespace: "default",
		InstanceGeneration: 1, AssignmentAttempt: 1, RuntimeInstanceID: "runtime-a",
	}, 1, []fastletaction.DesiredInput{{Handler: "egress", Input: `{}`}})
	require.NoError(t, err)

	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "uid-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a",
		InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1,
	}}, Phase: "running", AppliedGeneration: 1, ActionBindingStatuses: []fastletapi.ActionBindingStatus{{Handler: "egress", State: "Ready"}}}

	replayBlock := make(chan struct{})
	caller.setInvokeBlock(replayBlock)
	defer close(replayBlock)
	caller.setInstance("handler-2")
	var liveStatuses []fastletapi.ActionBindingStatus
	require.Eventually(t, func() bool {
		liveStatuses, _ = actionManager.Statuses("uid-a")
		return len(liveStatuses) == 1 && liveStatuses[0].State != "Ready"
	}, 2*time.Second, 10*time.Millisecond)

	inspected, err := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "uid-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a",
		InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1,
	}})
	require.NoError(t, err)
	require.Equal(t, liveStatuses[0].State, inspected.Sandbox.ActionBindings[0].State)
	require.NotEqual(t, "Ready", inspected.Sandbox.ActionBindings[0].State)
}

func TestReconcileWithoutActionManagerAdvancesAppliedGeneration(t *testing.T) {
	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-a"})
	require.NoError(t, err)
	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "uid-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a",
		InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1,
	}}, Phase: "running", AppliedGeneration: 1}

	response, err := manager.ReconcileBindings(context.Background(), &fastletapi.ReconcileBindingsRequest{
		Identity: fastletapi.SandboxIdentity{
			SandboxUID: "uid-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a",
			InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1,
		},
		SpecGeneration: 2,
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), response.Sandbox.AppliedGeneration)
}

func (r *admissionRuntime) RuntimeResourceAvailable() bool {
	return r.resourceReady == nil || *r.resourceReady
}

func (r *admissionRuntime) GetAccessDescriptor(sandboxID string) (dataplane.AccessDescriptor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxes[sandboxID] == nil && r.managed == nil {
		return dataplane.AccessDescriptor{}, ErrSandboxNotFound
	}
	return dataplane.AccessDescriptor{Kind: dataplane.AccessKindDirectIP, Address: "10.42.0.2"}, nil
}

func newAdmissionRuntime() *admissionRuntime {
	return &admissionRuntime{sandboxes: make(map[string]*SandboxMetadata)}
}

func (*admissionRuntime) Initialize(context.Context, string) error { return nil }
func (*admissionRuntime) SetNamespace(string)                      {}
func (*admissionRuntime) Close() error                             { return nil }
func (*admissionRuntime) ProbeCapabilities(context.Context) CapabilityReport {
	return CapabilityReport{State: runtimecatalog.CapabilityReady, Reason: "TestRuntimeReady"}
}

func (r *admissionRuntime) EnsureSandbox(_ context.Context, input *fastletapi.EnsureSandboxInput) (*SandboxMetadata, error) {
	r.mu.Lock()
	r.ensureCalls++
	err := r.ensureError
	entered, block := r.ensureEntered, r.ensureBlock
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	config := input.Sandbox
	sandboxUID := config.Identity.SandboxUID
	metadata := &SandboxMetadata{Config: config, ContainerID: sandboxUID, Phase: "running", CreatedAt: time.Now().Unix()}
	r.mu.Lock()
	r.sandboxes[sandboxUID] = metadata
	r.mu.Unlock()
	copy := *metadata
	return &copy, nil
}

func (r *admissionRuntime) InspectSandbox(_ context.Context, sandboxID string) (*SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	metadata := r.sandboxes[sandboxID]
	if metadata == nil {
		return nil, ErrSandboxNotFound
	}
	copy := *metadata
	return &copy, nil
}

func (r *admissionRuntime) DeleteSandbox(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	r.deleteCalls++
	entered, block := r.deleteEntered, r.deleteBlock
	err := r.deleteError
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.sandboxes, sandboxID)
	r.mu.Unlock()
	return nil
}

func (r *admissionRuntime) ListManagedSandboxes(context.Context) ([]*SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.managed != nil {
		result := make([]*SandboxMetadata, 0, len(r.managed))
		for _, metadata := range r.managed {
			copy := *metadata
			result = append(result, &copy)
		}
		return result, nil
	}
	result := make([]*SandboxMetadata, 0, len(r.sandboxes))
	for _, metadata := range r.sandboxes {
		copy := *metadata
		result = append(result, &copy)
	}
	return result, nil
}

func (r *admissionRuntime) ListImages(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.images...), nil
}

func (r *admissionRuntime) PullImage(_ context.Context, image string) error {
	r.mu.Lock()
	entered, block := r.pullEntered, r.pullBlock
	r.pulledImages = append(r.pulledImages, image)
	r.images = append(r.images, image)
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return nil
}

func (r *admissionRuntime) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureCalls, r.deleteCalls
}

func newAdmissionManager(t *testing.T, runtime RuntimeDriver, capacity int) *SandboxManager {
	t.Helper()
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: capacity, FastletPodUID: "pod-uid-a",
	})
	require.NoError(t, err)
	return manager
}

func ensureRequest(uid string, generation, attempt int64) *fastletapi.CreateSandboxRequest {
	return &fastletapi.CreateSandboxRequest{
		RequestID:      "request-" + uid,
		SpecGeneration: generation,
		Identity: fastletapi.SandboxIdentity{
			SandboxUID: uid, Namespace: "default", Name: uid,
			InstanceGeneration: generation, RuntimeInstanceID: fmt.Sprintf("runtime-%s-%d-%d", uid, generation, attempt),
			AssignmentAttempt: attempt, RouteGeneration: 1, FastletPodUID: "pod-uid-a",
		},
		Sandbox: fastletapi.SandboxSpec{Image: "alpine:latest"},
	}
}

type admissionRoutePublisher struct {
	mu             sync.Mutex
	applied        []RoutePublication
	removed        []RoutePublication
	reconciled     [][]RoutePublication
	applyError     error
	applyEntered   chan struct{}
	applyBlock     chan struct{}
	removeError    error
	reconcileError error
}

func (p *admissionRoutePublisher) ApplyRoute(_ context.Context, route RoutePublication) error {
	p.mu.Lock()
	p.applied = append(p.applied, route)
	err, entered, block := p.applyError, p.applyEntered, p.applyBlock
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return err
}

func (p *admissionRoutePublisher) RemoveRoute(_ context.Context, route RoutePublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, route)
	return p.removeError
}

func (p *admissionRoutePublisher) ReconcileRoutes(_ context.Context, routes []RoutePublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reconciled = append(p.reconciled, append([]RoutePublication(nil), routes...))
	return p.reconcileError
}

func requireFastletCode(t *testing.T, err error, code fastletapi.FastletErrorCode) {
	t.Helper()
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, code, failure.Code)
}

func TestSandboxDiagnosticsAreBoundedAndIdentityFenced(t *testing.T) {
	manager := newAdmissionManager(t, newAdmissionRuntime(), 2)
	request := ensureRequest("sandbox-a", 1, 1)
	created, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, created.Disposition)

	diagnostics, err := manager.SandboxDiagnostics(&fastletapi.SandboxDiagnosticsRequest{Identity: request.Identity, Limit: 2})
	require.NoError(t, err)
	require.NotNil(t, diagnostics.Sandbox)
	require.Equal(t, fastletapi.RuntimeStateReady, diagnostics.Sandbox.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateReady, diagnostics.Sandbox.DataPlane.State)
	require.Len(t, diagnostics.Events, 2)
	require.Equal(t, "fastlet", diagnostics.Events[1].Source)
	require.Equal(t, "running", diagnostics.Events[1].Phase)

	stale := request.Identity
	stale.RuntimeInstanceID = "different-runtime"
	_, err = manager.SandboxDiagnostics(&fastletapi.SandboxDiagnosticsRequest{Identity: stale})
	requireFastletCode(t, err, fastletapi.ErrorConflict)
}

func TestAdmissionNeverExceedsCapacityUnderConcurrency(t *testing.T) {
	runtime := newAdmissionRuntime()
	manager := newAdmissionManager(t, runtime, 5)
	var successes atomic.Int64
	var rejected atomic.Int64
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			_, err := manager.CreateSandbox(context.Background(), ensureRequest(fmt.Sprintf("sandbox-%03d", index), 1, 1))
			if err == nil {
				successes.Add(1)
				return
			}
			var failure *fastletapi.FastletError
			if errors.As(err, &failure) && failure.Code == fastletapi.ErrorCapacityRejected {
				rejected.Add(1)
				return
			}
			t.Errorf("unexpected Ensure error: %v", err)
		}(i)
	}
	close(start)
	group.Wait()
	require.EqualValues(t, 5, successes.Load())
	require.EqualValues(t, 95, rejected.Load())
	admission, _, _ := manager.State()
	require.Equal(t, 5, admission.Used)
	require.Equal(t, 5, admission.Running)
}

func TestDuplicateEnsureCreatesRuntimeOnce(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureEntered = make(chan struct{}, 1)
	runtime.ensureBlock = make(chan struct{})
	manager := newAdmissionManager(t, runtime, 5)
	request := ensureRequest("sandbox-a", 1, 1)

	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			response, err := manager.CreateSandbox(context.Background(), request)
			if err != nil {
				var failure *fastletapi.FastletError
				require.True(t, errors.As(err, &failure))
				require.Equal(t, fastletapi.ErrorInProgress, failure.Code)
				require.Equal(t, fastletapi.CreateDispositionInProgress, response.Disposition)
			}
		}()
	}
	close(start)
	<-runtime.ensureEntered
	time.Sleep(10 * time.Millisecond)
	close(runtime.ensureBlock)
	group.Wait()
	ensureCalls, _ := runtime.counts()
	require.Equal(t, 1, ensureCalls)
}

// TestEnsureIncompatibleArtifactMapsToProfileMismatch pins the rejection
// mapping the candidate-advance behavior relies on: a restore-admission
// failure is a deterministic ProfileMismatch (not retryable) with the
// rejected-before-side-effects disposition.
func TestEnsureIncompatibleArtifactMapsToProfileMismatch(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureError = fmt.Errorf("%w: unmasked snapshot identity mismatch", runtimecontract.ErrIncompatibleArtifact)
	manager := newAdmissionManager(t, runtime, 1)
	response, err := manager.CreateSandbox(context.Background(), ensureRequest("sandbox-a", 1, 1))
	requireFastletCode(t, err, fastletapi.ErrorProfileMismatch)
	var failure *fastletapi.FastletError
	require.True(t, errors.As(err, &failure))
	require.False(t, failure.Retryable)
	require.Equal(t, fastletapi.CreateDispositionRejectedBeforeSideEffects, response.Disposition)
}

func TestEnsureFailureReleasesCapacity(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureError = errors.New("create failed")
	manager := newAdmissionManager(t, runtime, 1)
	_, err := manager.CreateSandbox(context.Background(), ensureRequest("sandbox-a", 1, 1))
	requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
	runtime.mu.Lock()
	runtime.ensureError = nil
	runtime.mu.Unlock()
	_, err = manager.CreateSandbox(context.Background(), ensureRequest("sandbox-b", 1, 1))
	require.NoError(t, err)
	admission, _, _ := manager.State()
	require.Equal(t, 1, admission.Running)
}

func TestFailedCreateCleanupIsRetriedBySameIdentity(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureError = errors.New("create failed")
	runtime.deleteError = errors.New("containerd task still exiting")
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 1)

	response, err := manager.CreateSandbox(context.Background(), request)
	requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
	require.Equal(t, fastletapi.CreateDispositionFailedNeedsCleanup, response.Disposition)
	statuses := manager.GetSandboxStatuses(context.Background())
	require.Len(t, statuses, 1)
	require.Equal(t, fastletapi.RuntimeStateFailed, statuses[0].Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateFailed, statuses[0].DataPlane.State)

	runtime.mu.Lock()
	runtime.ensureError = nil
	runtime.deleteError = nil
	runtime.mu.Unlock()
	response, err = manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)
	require.Equal(t, fastletapi.RuntimeStateReady, response.Sandbox.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateReady, response.Sandbox.DataPlane.State)
	ensureCalls, deleteCalls := runtime.counts()
	require.Equal(t, 2, ensureCalls)
	require.Equal(t, 2, deleteCalls)
}

func TestUserDeleteFailureCannotBeResurrectedByCreateRetry(t *testing.T) {
	runtime := newAdmissionRuntime()
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 1)
	_, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	runtime.mu.Lock()
	runtime.deleteError = errors.New("delete failed")
	runtime.mu.Unlock()
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		statuses := manager.GetSandboxStatuses(context.Background())
		return len(statuses) == 1 && statuses[0].Runtime.State == fastletapi.RuntimeStateFailed
	}, time.Second, 10*time.Millisecond)
	_, err = manager.CreateSandbox(context.Background(), request)
	requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
}

func TestActionDeleteFailureDoesNotBlockRuntimeDeletion(t *testing.T) {
	runtime := newAdmissionRuntime()
	caller := &failingDeleteActionCaller{}
	actionManager, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-a", 1, 1)
	request.ActionBindings = []fastletapi.ActionBindingInput{{Handler: "egress", Input: `{}`}}
	request.Completion = fastletapi.CreateCompletionReady
	_, err = manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	response, err := manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.EqualValues(t, 1, caller.deleteCalls.Load())
	require.Eventually(t, func() bool {
		_, deleteCalls := runtime.counts()
		return deleteCalls == 1
	}, time.Second, 10*time.Millisecond)
}

func TestAtomicCreateRejectsUnavailableNetworkResource(t *testing.T) {
	available := false
	runtime := newAdmissionRuntime()
	runtime.resourceReady = &available
	manager := newAdmissionManager(t, runtime, 1)
	_, err := manager.CreateSandbox(context.Background(), ensureRequest("sandbox-a", 1, 1))
	requireFastletCode(t, err, fastletapi.ErrorNetworkUnavailable)

	available = true
	_, err = manager.CreateSandbox(context.Background(), ensureRequest("sandbox-a", 1, 1))
	require.NoError(t, err)
}

func TestDeletedIdentityCannotBeResurrectedByDelayedCreate(t *testing.T) {
	manager := newAdmissionManager(t, newAdmissionRuntime(), 1)
	request := ensureRequest("sandbox-a", 1, 1)
	_, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	fenced, err := manager.CreateSandbox(context.Background(), request)
	requireFastletCode(t, err, fastletapi.ErrorGenerationFenced)
	require.Equal(t, fastletapi.CreateDispositionGenerationFenced, fenced.Disposition)

	next := ensureRequest("sandbox-a", 1, 2)
	_, err = manager.CreateSandbox(context.Background(), next)
	require.NoError(t, err, "a higher assignment attempt is a new fenced runtime identity")
}

func TestIdentityFencingAndClaimConflict(t *testing.T) {
	manager := newAdmissionManager(t, newAdmissionRuntime(), 2)
	request := ensureRequest("sandbox-a", 2, 3)
	_, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	stale := request.Identity
	stale.InstanceGeneration = 1
	_, err = manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: stale})
	requireFastletCode(t, err, fastletapi.ErrorStaleGeneration)

	wrongPod := request.Identity
	wrongPod.FastletPodUID = "pod-uid-b"
	_, err = manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: wrongPod})
	requireFastletCode(t, err, fastletapi.ErrorStaleAssignment)

	conflict := *request
	conflict.Identity = request.Identity
	conflict.Identity.Name = "another-sandbox"
	conflicted, err := manager.CreateSandbox(context.Background(), &conflict)
	requireFastletCode(t, err, fastletapi.ErrorConflict)
	require.Equal(t, fastletapi.CreateDispositionUnknown, conflicted.Disposition)
}

func TestSameNameWithDifferentUIDCreatesDistinctSandboxes(t *testing.T) {
	runtime := newAdmissionRuntime()
	manager := newAdmissionManager(t, runtime, 2)
	first := ensureRequest("sandbox-uid-a", 1, 1)
	second := ensureRequest("sandbox-uid-b", 1, 1)
	first.Identity.Name = "shared-name"
	second.Identity.Name = "shared-name"

	_, err := manager.CreateSandbox(context.Background(), first)
	require.NoError(t, err)
	_, err = manager.CreateSandbox(context.Background(), second)
	require.NoError(t, err)

	statuses := manager.GetSandboxStatuses(context.Background())
	require.Len(t, statuses, 2)
	require.Equal(t, 2, len(runtime.sandboxes))
}

func TestDeleteIsIdempotentAndFenced(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.deleteEntered = make(chan struct{}, 1)
	runtime.deleteBlock = make(chan struct{})
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 2)
	_, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	stale := request.Identity
	stale.AssignmentAttempt = 1
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: stale})
	requireFastletCode(t, err, fastletapi.ErrorStaleGeneration)

	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	<-runtime.deleteEntered
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	close(runtime.deleteBlock)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	_, deletes := runtime.counts()
	require.Equal(t, 1, deletes)
}

func TestDeleteDuringCreateWinsWithoutOrphan(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureEntered = make(chan struct{}, 1)
	runtime.ensureBlock = make(chan struct{})
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 1)
	result := make(chan error, 1)
	go func() {
		_, err := manager.CreateSandbox(context.Background(), request)
		result <- err
	}()
	<-runtime.ensureEntered
	_, err := manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	close(runtime.ensureBlock)
	requireFastletCode(t, <-result, fastletapi.ErrorConflict)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	_, deletes := runtime.counts()
	require.Equal(t, 1, deletes)
}

func TestRecoveryBlocksReadinessAndRestoresCapacity(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.managed = []*SandboxMetadata{
		{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", FastletPodUID: "pod-uid-a", InstanceGeneration: 2, AssignmentAttempt: 3}}, Phase: "running"},
		{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{SandboxUID: "stale-sandbox", FastletPodUID: "old-pod-uid", InstanceGeneration: 1, AssignmentAttempt: 1}}, Phase: "running"},
	}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RecoverOnStart: true,
	})
	require.NoError(t, err)
	require.False(t, manager.Ready())
	_, err = manager.CreateSandbox(context.Background(), ensureRequest("sandbox-b", 1, 1))
	requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
	require.NoError(t, manager.Recover(context.Background()))
	require.True(t, manager.Ready())
	admission, recovering, _ := manager.State()
	require.False(t, recovering)
	require.Equal(t, 1, admission.Running)
	_, err = manager.CreateSandbox(context.Background(), ensureRequest("sandbox-b", 1, 1))
	requireFastletCode(t, err, fastletapi.ErrorCapacityRejected)
}

func TestRoutePublicationContinuesAfterRuntimeReadyWithoutRecreatingRuntime(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &admissionRoutePublisher{applyError: errors.New("proxy control unavailable")}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-a", 1, 2)

	response, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)
	require.Equal(t, fastletapi.RuntimeStateReady, response.Sandbox.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStatePublishing, response.Sandbox.DataPlane.State)
	ensures, _ := runtime.counts()
	require.Equal(t, 1, ensures)
	require.Eventually(t, func() bool {
		inspected, inspectErr := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
		return inspectErr == nil && inspected.Sandbox.DataPlane.State == fastletapi.DataPlaneStateUnavailable
	}, time.Second, 10*time.Millisecond)

	idempotent, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionExisting, idempotent.Disposition)
	ensures, _ = runtime.counts()
	require.Equal(t, 1, ensures, "a duplicate Create must not wait for or recreate the data plane")

	publisher.mu.Lock()
	publisher.applyError = nil
	publisher.mu.Unlock()
	require.Eventually(t, func() bool {
		inspected, inspectErr := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
		return inspectErr == nil && inspected.Sandbox.DataPlane.State == fastletapi.DataPlaneStateReady
	}, 3*time.Second, 10*time.Millisecond)
	ensures, _ = runtime.counts()
	require.Equal(t, 1, ensures, "asynchronous route retry must not create a second runtime")
	publisher.mu.Lock()
	require.GreaterOrEqual(t, len(publisher.applied), 2)
	last := publisher.applied[len(publisher.applied)-1]
	require.Equal(t, int64(1), last.RouteGeneration)
	require.Equal(t, int64(2), last.AssignmentAttempt)
	publisher.mu.Unlock()
}

func TestDeleteFencesAsynchronousRoutePublication(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &admissionRoutePublisher{applyEntered: make(chan struct{}, 1), applyBlock: make(chan struct{})}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-a", 1, 1)

	response, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.DataPlaneStatePublishing, response.Sandbox.DataPlane.State)
	<-publisher.applyEntered

	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	close(publisher.applyBlock)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		publisher.mu.Lock()
		defer publisher.mu.Unlock()
		return len(publisher.removed) >= 1
	}, time.Second, 10*time.Millisecond)
}

func TestDrainingRejectsNewEnsureButKeepsExistingSandboxIdempotent(t *testing.T) {
	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{
		Capacity: 2, FastletPodUID: "pod-uid-a",
	})
	require.NoError(t, err)
	existing := ensureRequest("sandbox-a", 1, 1)
	created, err := manager.CreateSandbox(context.Background(), existing)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, created.Disposition)

	manager.SetDraining(true, "pool scale-down")
	reconciled, err := manager.CreateSandbox(context.Background(), existing)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionExisting, reconciled.Disposition)
	require.Equal(t, fastletapi.RuntimeStateReady, reconciled.Sandbox.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateReady, reconciled.Sandbox.DataPlane.State)

	_, err = manager.CreateSandbox(context.Background(), ensureRequest("sandbox-b", 1, 1))
	requireFastletCode(t, err, fastletapi.ErrorDraining)
}

func TestRouteRemovalPrecedesAndGatesRuntimeDeletion(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &admissionRoutePublisher{removeError: errors.New("proxy control unavailable")}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-a", 1, 1)
	_, err = manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		publisher.mu.Lock()
		removals := len(publisher.removed)
		publisher.mu.Unlock()
		_, deletes := runtime.counts()
		return removals == 1 && deletes == 0
	}, time.Second, 10*time.Millisecond)

	publisher.mu.Lock()
	publisher.removeError = nil
	publisher.mu.Unlock()
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, deletes := runtime.counts()
		return deletes == 1
	}, time.Second, 10*time.Millisecond)
}

func TestDeleteRemovesRouteWhenRuntimeAccessIsAlreadyAbsent(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &admissionRoutePublisher{}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-a", 1, 1)
	_, err = manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	// Model a partial prior delete: the runtime/container is already gone, but
	// the manager still owns admission state and must retry route removal.
	runtime.mu.Lock()
	delete(runtime.sandboxes, request.Identity.SandboxUID)
	runtime.mu.Unlock()

	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	publisher.mu.Lock()
	require.Len(t, publisher.removed, 1)
	require.Equal(t, request.Identity.SandboxUID, publisher.removed[0].SandboxUID)
	require.Equal(t, request.Identity.RouteGeneration, publisher.removed[0].RouteGeneration)
	publisher.mu.Unlock()
}

func TestRecoveryReconcilesRoutesBeforeReadiness(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.managed = []*SandboxMetadata{{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", Namespace: "default", FastletPodUID: "pod-uid-a",
		InstanceGeneration: 1, AssignmentAttempt: 2, RouteGeneration: 3,
	}}, Phase: "running"}}
	publisher := &admissionRoutePublisher{reconcileError: errors.New("proxy recovery pending")}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RecoverOnStart: true, RoutePublisher: publisher,
	})
	require.NoError(t, err)
	require.Error(t, manager.Recover(context.Background()))
	require.False(t, manager.Ready())
	publisher.mu.Lock()
	publisher.reconcileError = nil
	publisher.mu.Unlock()
	require.NoError(t, manager.Recover(context.Background()))
	require.True(t, manager.Ready())
	publisher.mu.Lock()
	require.Len(t, publisher.reconciled, 2)
	require.Equal(t, int64(3), publisher.reconciled[1][0].RouteGeneration)
	publisher.mu.Unlock()
}

func TestRecoveryDefersDestructiveRouteReconcileUntilInfraIsRestored(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.managed = []*SandboxMetadata{{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", Namespace: "default", FastletPodUID: "pod-uid-a",
		InstanceGeneration: 1, AssignmentAttempt: 2, RouteGeneration: 3,
	}}, Phase: "running"}}
	publisher := &admissionRoutePublisher{}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RecoverOnStart: true, RoutePublisher: publisher,
		InfraManager: &fastletinfra.Manager{},
	})
	require.NoError(t, err)
	require.NoError(t, manager.Recover(context.Background()))
	require.False(t, manager.Ready(), "pending Infra must keep route readiness closed")
	publisher.mu.Lock()
	require.Empty(t, publisher.reconciled, "an empty desired set would tombstone the live sidecar route")
	publisher.mu.Unlock()

	require.ErrorIs(t, manager.ReconcileProxyRoutes(context.Background()), ErrInfraUnavailable)
	publisher.mu.Lock()
	require.Empty(t, publisher.reconciled)
	publisher.mu.Unlock()

	manager.mu.Lock()
	manager.sandboxes["sandbox-a"].Phase = "running"
	manager.mu.Unlock()
	require.NoError(t, manager.ReconcileProxyRoutes(context.Background()))
	require.True(t, manager.Ready())
	publisher.mu.Lock()
	require.Len(t, publisher.reconciled, 1)
	require.Len(t, publisher.reconciled[0], 1)
	require.Equal(t, int64(3), publisher.reconciled[0][0].RouteGeneration)
	publisher.mu.Unlock()
}

func TestProxyControlReconnectRevokesAndRestoresReadiness(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &admissionRoutePublisher{}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	_, err = manager.CreateSandbox(context.Background(), ensureRequest("sandbox-a", 1, 1))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.sandboxes["sandbox-a"].Phase == "running"
	}, time.Second, 10*time.Millisecond)
	require.True(t, manager.Ready())

	manager.MarkProxyRouteUnavailable()
	require.False(t, manager.Ready())
	request := ensureRequest("sandbox-a", 1, 1)
	inspected, inspectErr := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
	require.NoError(t, inspectErr)
	require.Equal(t, fastletapi.DataPlaneStateUnavailable, inspected.Sandbox.DataPlane.State)
	require.NoError(t, manager.ReconcileProxyRoutes(context.Background()))
	require.True(t, manager.Ready())
	publisher.mu.Lock()
	require.Len(t, publisher.reconciled, 1)
	require.Len(t, publisher.reconciled[0], 1)
	publisher.mu.Unlock()
}

func TestWarmImagesAreAsynchronousAndProtectedFromEviction(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.pullEntered = make(chan struct{}, 2)
	runtime.pullBlock = make(chan struct{})
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", WarmImages: []string{"alpine:latest", "ubuntu:24.04"},
	})
	require.NoError(t, err)
	require.True(t, manager.Ready(), "warmImages never gate Fastlet readiness")
	completed := make(chan error, 1)
	go func() { completed <- manager.WarmCache(context.Background()) }()
	<-runtime.pullEntered
	require.True(t, manager.Ready())
	close(runtime.pullBlock)
	require.NoError(t, <-completed)
	runtime.mu.Lock()
	require.ElementsMatch(t, []string{"alpine:latest", "ubuntu:24.04"}, runtime.pulledImages)
	runtime.mu.Unlock()
	require.Equal(t, []string{"cold:1"}, manager.PlanCacheEviction([]string{"ubuntu:24.04", "cold:1", "alpine:latest"}))
}
