package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/fastletsettings"
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
	allocation    fastletapi.RuntimeAllocation
	ensureEntered chan struct{}
	ensureBlock   chan struct{}
	deleteEntered chan struct{}
	deleteBlock   chan struct{}
	pullEntered   chan struct{}
	pullBlock     chan struct{}
	listEntered   chan struct{}
	listRelease   chan struct{}
	pulledImages  []string
	images        []string
	resourceReady *bool
}

type failingDeleteActionCaller struct {
	deleteCalls atomic.Int32
}

type legacyOnlyActionCaller struct {
	statusCalls atomic.Int32
	invokeCalls atomic.Int32
}

func (c *legacyOnlyActionCaller) Status(_ context.Context, _ int32, _ string) (actionapi.HandlerStatus, error) {
	c.statusCalls.Add(1)
	return actionapi.HandlerStatus{APIVersion: actionapi.APIVersion, Ready: true, InstanceID: "legacy-egress"}, nil
}

func (c *legacyOnlyActionCaller) Invoke(_ context.Context, _ int32, _ actionapi.Request) error {
	c.invokeCalls.Add(1)
	return nil
}

func (*legacyOnlyActionCaller) Relay(context.Context, int32, string, actionapi.Owner, string, string, []byte) (int, []byte, error) {
	return http.StatusNotFound, nil, nil
}

type strictDeleteActionCaller struct {
	mu              sync.Mutex
	owner           actionapi.Owner
	ownerSet        bool
	retired         bool
	failRemove      bool
	failRead        bool
	removeCalls     int
	lostRemoveReply bool
	lastRemove      actionapi.Request
}

type blockingStrictDeleteActionCaller struct {
	*strictDeleteActionCaller
	removeEntered    chan struct{}
	removeRelease    <-chan struct{}
	removeStarted    chan struct{}
	removeStartOnce  sync.Once
	ownerReadEntered chan struct{}
	ownerReadRelease <-chan struct{}
}

type gatedDeleteBootRuntime struct {
	*asyncBootRuntime
	deliveryCalls   atomic.Int32
	deliveryEntered chan struct{}
	deliveryRelease <-chan struct{}
}

func (r *gatedDeleteBootRuntime) DeliverImage(context.Context, string) (ImageDeliveryStatus, error) {
	// Create polls synchronously before parking; only gate the later worker.
	if r.deliveryCalls.Add(1) == 1 {
		return ImageDelivering, nil
	}
	select {
	case r.deliveryEntered <- struct{}{}:
	default:
	}
	<-r.deliveryRelease
	return ImageDelivering, nil
}

func (c *blockingStrictDeleteActionCaller) Invoke(ctx context.Context, port int32, request actionapi.Request) error {
	if request.Operation == actionapi.OperationRemoveBinding {
		c.removeStartOnce.Do(func() { close(c.removeStarted) })
		select {
		case c.removeEntered <- struct{}{}:
		default:
		}
		select {
		case <-c.removeRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.strictDeleteActionCaller.Invoke(ctx, port, request)
}

func (c *blockingStrictDeleteActionCaller) Relay(ctx context.Context, port int32, apiVersion string, owner actionapi.Owner, method, path string, body []byte) (int, []byte, error) {
	status, response, err := c.strictDeleteActionCaller.Relay(ctx, port, apiVersion, owner, method, path, body)
	if method == http.MethodGet {
		select {
		case <-c.removeStarted:
		default:
			return status, response, err
		}
		select {
		case c.ownerReadEntered <- struct{}{}:
		default:
		}
		select {
		case <-c.ownerReadRelease:
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	return status, response, err
}

type recoveryAdmissionRuntime struct {
	*admissionRuntime
	recoverFn func(context.Context, []*SandboxMetadata, runtimecontract.ResourceRecoveryOwnerRetirer) error
}

func (r *recoveryAdmissionRuntime) RecoverRuntimeResources(ctx context.Context, managed []*SandboxMetadata, retireOwner runtimecontract.ResourceRecoveryOwnerRetirer) error {
	if r.recoverFn == nil {
		return nil
	}
	return r.recoverFn(ctx, managed, retireOwner)
}

func (c *strictDeleteActionCaller) Status(_ context.Context, _ int32, apiVersion string) (actionapi.HandlerStatus, error) {
	return actionapi.HandlerStatus{APIVersion: apiVersion, Ready: true, InstanceID: "handler-v2", OwnerStatus: true}, nil
}

func (c *strictDeleteActionCaller) Invoke(_ context.Context, _ int32, request actionapi.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch request.Operation {
	case actionapi.OperationSetBinding:
		if request.Owner == nil {
			return errors.New("Actions v2 SET omitted owner")
		}
		c.owner, c.ownerSet = *request.Owner, true
	case actionapi.OperationRemoveBinding:
		c.removeCalls++
		c.lastRemove = request
		if c.failRemove {
			return errors.New("injected strict REMOVE timeout")
		}
		c.retired = true
		c.owner, c.ownerSet = *request.Owner, true
		if c.lostRemoveReply {
			return errors.New("injected lost REMOVE response")
		}
	}
	return nil
}

func (c *strictDeleteActionCaller) Relay(_ context.Context, _ int32, apiVersion string, owner actionapi.Owner, method, path string, _ []byte) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failRead {
		return 0, nil, errors.New("injected owner read failure")
	}
	if apiVersion != actionapi.APIVersionV2 || method != http.MethodGet || path != "/_fastlet/v2/owners/"+owner.SandboxUID || c.ownerSet && c.owner.SandboxUID == owner.SandboxUID && c.owner != owner {
		return http.StatusConflict, nil, nil
	}
	state := actionapi.OwnerStateCurrent
	if !c.ownerSet || c.owner.SandboxUID != owner.SandboxUID {
		state = actionapi.OwnerStateAbsent
	} else if c.retired {
		state = actionapi.OwnerStateRetired
	}
	body, err := json.Marshal(actionapi.OwnerObservation{State: state, Owner: owner})
	return http.StatusOK, body, err
}

func actionOwnerForTest(attachment fastletaction.Attachment) actionapi.Owner {
	return actionapi.Owner{
		SandboxUID: attachment.SandboxUID, FastletPodUID: attachment.FastletPodUID,
		RuntimeInstanceID: attachment.RuntimeInstanceID, InstanceGeneration: attachment.InstanceGeneration,
		AssignmentAttempt: attachment.AssignmentAttempt, RouteGeneration: attachment.RouteGeneration,
		AttachmentID: attachment.ID,
	}
}

type restartAwareActionCaller struct {
	mu       sync.Mutex
	instance string
	block    <-chan struct{}
}

func (c *restartAwareActionCaller) Status(_ context.Context, _ int32, apiVersion string) (actionapi.HandlerStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return actionapi.HandlerStatus{APIVersion: apiVersion, Ready: true, InstanceID: c.instance, OwnerStatus: apiVersion == actionapi.APIVersionV2}, nil
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

func (c *restartAwareActionCaller) Relay(ctx context.Context, _ int32, _ string, _ actionapi.Owner, _, _ string, _ []byte) (int, []byte, error) {
	c.Invoke(ctx, 0, actionapi.Request{})
	return http.StatusOK, nil, nil
}

func (*failingDeleteActionCaller) Status(_ context.Context, _ int32, apiVersion string) (actionapi.HandlerStatus, error) {
	return actionapi.HandlerStatus{APIVersion: apiVersion, Ready: true, InstanceID: "handler-a", OwnerStatus: apiVersion == actionapi.APIVersionV2}, nil
}

func (c *failingDeleteActionCaller) Invoke(_ context.Context, _ int32, request actionapi.Request) error {
	if request.Operation == actionapi.OperationRemoveBinding {
		c.deleteCalls.Add(1)
		return errors.New("injected Handler RemoveBinding failure")
	}
	return nil
}

func (c *failingDeleteActionCaller) Relay(ctx context.Context, _ int32, _ string, _ actionapi.Owner, _, _ string, _ []byte) (int, []byte, error) {
	return http.StatusOK, nil, nil
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
		IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24", HostVeth: "host-a",
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
		// The blocked replay keeps Applying stable while Inspect reads it;
		// Pending may advance between the two reads.
		return len(liveStatuses) == 1 && liveStatuses[0].State == "Applying"
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
	metadata := &SandboxMetadata{Config: config, Allocation: r.allocation, ContainerID: sandboxUID, Phase: "running", CreatedAt: time.Now().Unix()}
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

func (r *admissionRuntime) ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error) {
	if r.listRelease != nil {
		close(r.listEntered)
		select {
		case <-r.listRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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

func newResourceAwareAdmissionManager(
	t *testing.T,
	runtime RuntimeDriver,
	slots int,
	capacityCPU string,
	capacityMemory string,
) (*SandboxManager, apiv1alpha2.SandboxResourceProfile) {
	t.Helper()
	profile := apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("2"), resource.MustParse("10Gi"), resource.MustParse("2"), resource.MustParse("2"), 256,
	)
	settings := &fastletsettings.ResourceAdmissionSettings{
		Version: "v2", Mode: apiv1alpha2.SandboxResourceModeResourceAware,
		StaticAdmission: true, MemoryAdmission: true,
		Capacity: apiv1alpha2.SandboxComputeResources{
			CPU: resource.MustParse(capacityCPU), Memory: resource.MustParse(capacityMemory),
		},
		SandboxRuntimeBudget: apiv1alpha2.SandboxComputeResources{
			CPU: resource.MustParse(capacityCPU), Memory: resource.MustParse(capacityMemory),
		},
		RuntimeEnvelope: apiv1alpha2.SandboxComputeResources{
			CPU: resource.MustParse(capacityCPU), Memory: resource.MustParse(capacityMemory),
		},
		PodMemoryBudget:     resource.MustParse(capacityMemory),
		ResourceProfileHash: profile.Hash(),
	}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: slots, FastletPodUID: "pod-uid-a", ResourceProfile: &profile,
		ResourceAdmission: settings, ResourceEpoch: "resource-epoch-a",
	})
	require.NoError(t, err)
	return manager, profile
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
		Sandbox: fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
	}
}

func resourceAwareRequest(uid string, profile apiv1alpha2.SandboxResourceProfile) *fastletapi.CreateSandboxRequest {
	request := ensureRequest(uid, 1, 1)
	request.Sandbox.ResourceProfileHash = profile.Hash()
	requests, _ := profile.RequestResources()
	limits, _ := profile.LimitResources()
	return resourceAwareRequestWithResources(uid, profile, requests, limits)
}

func resourceAwareRequestWithLimits(uid string, profile apiv1alpha2.SandboxResourceProfile, cpu, memory string) *fastletapi.CreateSandboxRequest {
	limits := apiv1alpha2.SandboxComputeResources{CPU: resource.MustParse(cpu), Memory: resource.MustParse(memory)}
	requests, err := profile.DeriveRequests(limits)
	if err != nil {
		panic(err)
	}
	return resourceAwareRequestWithResources(uid, profile, requests, limits)
}

func resourceAwareRequestWithResources(
	uid string,
	profile apiv1alpha2.SandboxResourceProfile,
	requests apiv1alpha2.SandboxComputeResources,
	limits apiv1alpha2.SandboxComputeResources,
) *fastletapi.CreateSandboxRequest {
	request := ensureRequest(uid, 1, 1)
	request.Sandbox.ResourceProfileHash = profile.Hash()
	request.Resources = &fastletapi.ResolvedSandboxResources{
		Requests: fastletapi.SandboxResourceVector{CPU: requests.CPU.String(), Memory: requests.Memory.String()},
		Limits:   fastletapi.SandboxRuntimeLimits{CPU: limits.CPU.String(), Memory: limits.Memory.String(), PIDs: profile.PIDs},
	}
	return request
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

func TestResourceAwareAdmissionUsesTighterCPUCommitment(t *testing.T) {
	manager, profile := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 10, "8", "50Gi")
	for index := 0; index < 8; index++ {
		response, err := manager.CreateSandbox(context.Background(), resourceAwareRequest(fmt.Sprintf("sandbox-%d", index), profile))
		require.NoError(t, err)
		require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)
	}

	rejected, err := manager.CreateSandbox(context.Background(), resourceAwareRequest("sandbox-rejected", profile))
	requireFastletCode(t, err, fastletapi.ErrorCapacityRejected)
	require.Equal(t, fastletapi.CreateDispositionRejectedBeforeSideEffects, rejected.Disposition)
	require.Equal(t, fastletapi.AdmissionReasonStaticCPUExhausted, rejected.Error.Reason)
	require.Equal(t, 8, rejected.Admission.Used)
	require.NotNil(t, rejected.Admission.Resources)
	require.Equal(t, "resource-epoch-a", rejected.Admission.Resources.Epoch)
}

func TestResourceAwareAdmissionUsesTighterMemoryCommitment(t *testing.T) {
	manager, profile := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 10, "10", "25Gi")
	for index := 0; index < 5; index++ {
		_, err := manager.CreateSandbox(context.Background(), resourceAwareRequest(fmt.Sprintf("sandbox-%d", index), profile))
		require.NoError(t, err)
	}

	rejected, err := manager.CreateSandbox(context.Background(), resourceAwareRequest("sandbox-rejected", profile))
	requireFastletCode(t, err, fastletapi.ErrorCapacityRejected)
	require.Equal(t, fastletapi.AdmissionReasonStaticMemoryExhausted, rejected.Error.Reason)
	require.Equal(t, 5, rejected.Admission.Used)
}

func TestResourceAwareDeleteReturnsLatestAdmissionLedger(t *testing.T) {
	manager, profile := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 2, "2", "10Gi")
	request := resourceAwareRequest("sandbox-a", profile)
	_, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	response, err := manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.NotNil(t, response.Admission.Resources)
	require.Equal(t, "v2", response.Admission.Resources.Version)
	require.Equal(t, "2", response.Admission.Resources.Capacity.CPU)
	require.Equal(t, "10Gi", response.Admission.Resources.Capacity.Memory)
}

func TestResourceAwareAdmissionAccountsHeterogeneousSandboxRequests(t *testing.T) {
	manager, profile := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 10, "4", "10Gi")

	first, err := manager.CreateSandbox(context.Background(), resourceAwareRequestWithLimits("sandbox-small", profile, "1", "2Gi"))
	require.NoError(t, err)
	require.Equal(t, "500m", first.Admission.Resources.Committed.CPU)
	require.Equal(t, "1Gi", first.Admission.Resources.Committed.Memory)

	second, err := manager.CreateSandbox(context.Background(), resourceAwareRequestWithLimits("sandbox-large", profile, "4", "10Gi"))
	require.NoError(t, err)
	require.Equal(t, "2500m", second.Admission.Resources.Committed.CPU)
	require.Equal(t, "6Gi", second.Admission.Resources.Committed.Memory)

	third, err := manager.CreateSandbox(context.Background(), resourceAwareRequestWithLimits("sandbox-too-large", profile, "4", "6Gi"))
	requireFastletCode(t, err, fastletapi.ErrorCapacityRejected)
	require.Equal(t, fastletapi.AdmissionReasonStaticCPUExhausted, third.Error.Reason)
	require.Equal(t, "2500m", third.Admission.Resources.Committed.CPU)
	require.Equal(t, "6Gi", third.Admission.Resources.Committed.Memory)

	stored := manager.sandboxes["sandbox-large"]
	require.Equal(t, "4", stored.Config.Limits.CPU)
	require.Equal(t, "10Gi", stored.Config.Limits.Memory)
}

func TestResourceAwareAdmissionFeatureGatesAreIndependent(t *testing.T) {
	manager, _ := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 10, "2", "10Gi")
	manager.staticResourceAdmission = false
	manager.resourceAdmission.MemoryAdmission = false
	manager.resourceState = fastletapi.AdmissionStateBlocked
	manager.resourceReason = fastletapi.AdmissionReasonFastletMemoryHighWatermark

	require.Nil(t, manager.resourceCapacityFailureLocked(&fastletapi.RuntimeSandboxConfig{Limits: fastletapi.SandboxRuntimeLimits{CPU: "2", Memory: "10Gi", PIDs: 256}}),
		"shadow telemetry may remain active while static and memory admission decisions are disabled")
}

func TestResourceAdmissionRevisionOrdersPressureFeedback(t *testing.T) {
	manager, profile := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 10, "10", "50Gi")
	initial, _, _ := manager.State()
	require.Equal(t, fastletapi.AdmissionStateOpen, initial.Resources.State)
	require.Equal(t, uint64(1), initial.Resources.Revision)

	created, err := manager.CreateSandbox(context.Background(), resourceAwareRequest("sandbox-a", profile))
	require.NoError(t, err)
	require.Equal(t, uint64(2), created.Admission.Resources.Revision)

	require.NoError(t, manager.SetResourceAdmissionState(
		fastletapi.AdmissionStateBlocked,
		fastletapi.AdmissionReasonFastletMemoryHighWatermark,
	))
	rejected, err := manager.CreateSandbox(context.Background(), resourceAwareRequest("sandbox-b", profile))
	requireFastletCode(t, err, fastletapi.ErrorCapacityRejected)
	require.Equal(t, fastletapi.AdmissionReasonFastletMemoryHighWatermark, rejected.Error.Reason)
	require.Equal(t, fastletapi.AdmissionStateBlocked, rejected.Admission.Resources.State)
	require.Equal(t, uint64(3), rejected.Admission.Resources.Revision)

	repeated, _, _ := manager.State()
	require.Equal(t, rejected.Admission.Resources.Revision, repeated.Resources.Revision)
}

func TestResourceAdmissionRejectsUnsupportedState(t *testing.T) {
	manager, _ := newResourceAwareAdmissionManager(t, newAdmissionRuntime(), 10, "10", "50Gi")
	require.ErrorContains(t, manager.SetResourceAdmissionState(fastletapi.AdmissionState("DEGRADED"), ""), "unsupported resource admission state")
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
	// The Binding must reach the Handler before Delete for REMOVE_BINDING to
	// fire, so the fake runtime materializes a network allocation (#136: an
	// unattached Binding parks as Pending and is never applied nor removed).
	runtime.allocation = fastletapi.RuntimeAllocation{Network: fastletapi.NetworkAllocation{
		IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24",
	}}
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

func TestActionsV2DeleteKeepsRuntimeUntilExactRemoveAcknowledged(t *testing.T) {
	runtime := newAdmissionRuntime()
	caller := &strictDeleteActionCaller{failRemove: true}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
		Name: "egress", TargetHTTPPort: 18080,
	}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	identity := fastletapi.SandboxIdentity{
		SandboxUID: "uid-v2", Namespace: "default", Name: "sandbox-v2", FastletPodUID: "pod-uid-a",
		InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-v2",
	}
	metadata := &SandboxMetadata{
		Config: fastletapi.RuntimeSandboxConfig{Identity: identity, ActionsAPIVersion: actionapi.APIVersionV2, Desired: fastletapi.SandboxDesiredSpec{
			Image: "alpine:latest",
		}},
		Allocation: fastletapi.RuntimeAllocation{Network: fastletapi.NetworkAllocation{
			IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24", HostVeth: "host-v2",
		}},
		Phase: sandboxStateRunning, AppliedGeneration: 1,
	}
	manager.sandboxes[identity.SandboxUID] = metadata
	_, _, err = actionManager.Reconcile(context.Background(), actionAttachment(metadata), 1, []fastletaction.DesiredInput{{Handler: "egress", Input: `{}`}})
	require.NoError(t, err)

	request := &fastletapi.DeleteSandboxRequest{Identity: identity}
	_, err = manager.DeleteSandboxContext(context.Background(), request)
	require.Error(t, err)
	require.ErrorContains(t, err, "strict REMOVE_BINDING remains pending")
	_, deleteCalls := runtime.counts()
	require.Zero(t, deleteCalls, "unknown strict REMOVE outcome must block runtime teardown")
	require.Equal(t, sandboxStateTerminating, manager.sandboxes[identity.SandboxUID].Phase)

	caller.mu.Lock()
	caller.failRemove = false
	caller.mu.Unlock()
	_, err = manager.DeleteSandboxContext(context.Background(), request)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, deleteCalls = runtime.counts()
		return deleteCalls == 1
	}, time.Second, 10*time.Millisecond, "runtime teardown starts only after exact-owner REMOVE ACK")
}

func TestActionsV2ColdCreateDeleteWaitsForRetirementAndCanRetry(t *testing.T) {
	runtime := newAsyncBootRuntime(ImageDelivering)
	caller := &strictDeleteActionCaller{failRemove: true}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
		Name: "egress", TargetHTTPPort: 18080,
	}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	request := ensureRequest("cold-v2-delete", 1, 1)
	request.ActionBindings = []fastletapi.ActionBindingInput{{Handler: "egress", Input: `{}`}}
	created, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.RuntimeStateCreating, created.Sandbox.Runtime.State)

	manager.mu.RLock()
	metadata := manager.sandboxes[request.Identity.SandboxUID]
	manager.mu.RUnlock()
	attachment := actionAttachment(metadata)
	caller.mu.Lock()
	caller.owner, caller.ownerSet = actionOwnerForTest(attachment), true
	caller.mu.Unlock()

	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.ErrorContains(t, err, "strict REMOVE_BINDING remains pending")
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.sandboxes[request.Identity.SandboxUID] == metadata && metadata.Phase == sandboxStateDeleteFailed && !manager.createDeletePending[request.Identity.SandboxUID]
	}, time.Second, 10*time.Millisecond, "the canceled image worker must finish before a later delete retry")
	require.False(t, runtime.MockRuntime.GetDeleteCalled(), "worker cleanup must not bypass failed REMOVE")
	caller.mu.Lock()
	require.Equal(t, 1, caller.removeCalls, "the worker only reads back owner state; it must not retry REMOVE")
	caller.failRemove = false
	caller.mu.Unlock()

	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, runtime.MockRuntime.GetDeleteCalled, time.Second, 10*time.Millisecond, "a later Delete retries retirement then cleans the parked runtime")
}

func TestActionsV2ColdCreateDeleteQueuesCleanupAfterConcurrentRemoveAck(t *testing.T) {
	deliveryEntered := make(chan struct{}, 1)
	deliveryRelease := make(chan struct{})
	var deliveryReleaseOnce sync.Once
	releaseDelivery := func() { deliveryReleaseOnce.Do(func() { close(deliveryRelease) }) }
	defer releaseDelivery()
	runtime := &gatedDeleteBootRuntime{
		asyncBootRuntime: newAsyncBootRuntime(ImageDelivering),
		deliveryEntered:  deliveryEntered, deliveryRelease: deliveryRelease,
	}
	baseCaller := &strictDeleteActionCaller{}
	removeRelease := make(chan struct{})
	ownerReadRelease := make(chan struct{})
	var removeReleaseOnce, ownerReadReleaseOnce sync.Once
	releaseRemove := func() { removeReleaseOnce.Do(func() { close(removeRelease) }) }
	releaseOwnerRead := func() { ownerReadReleaseOnce.Do(func() { close(ownerReadRelease) }) }
	defer releaseRemove()
	defer releaseOwnerRead()
	caller := &blockingStrictDeleteActionCaller{
		strictDeleteActionCaller: baseCaller,
		removeEntered:            make(chan struct{}, 1), removeRelease: removeRelease, removeStarted: make(chan struct{}),
		ownerReadEntered: make(chan struct{}, 1), ownerReadRelease: ownerReadRelease,
	}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
		Name: "egress", TargetHTTPPort: 18080,
	}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	request := ensureRequest("cold-v2-delete-race", 1, 1)
	request.ActionBindings = []fastletapi.ActionBindingInput{{Handler: "egress", Input: `{}`}}
	created, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, fastletapi.RuntimeStateCreating, created.Sandbox.Runtime.State)
	select {
	case <-deliveryEntered:
	case <-time.After(time.Second):
		t.Fatal("cold create image worker did not enter delivery")
	}

	manager.mu.RLock()
	metadata := manager.sandboxes[request.Identity.SandboxUID]
	manager.mu.RUnlock()
	baseCaller.mu.Lock()
	baseCaller.owner, baseCaller.ownerSet = actionOwnerForTest(actionAttachment(metadata)), true
	baseCaller.mu.Unlock()
	deleteDone := make(chan error, 1)
	go func() {
		_, deleteErr := manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
		deleteDone <- deleteErr
	}()
	select {
	case <-caller.removeEntered:
	case deleteErr := <-deleteDone:
		t.Fatalf("Delete returned before strict REMOVE entered the Handler: %v", deleteErr)
	case <-time.After(time.Second):
		t.Fatal("strict REMOVE did not reach the Handler")
	}
	releaseDelivery()
	select {
	case <-caller.ownerReadEntered:
	case <-time.After(time.Second):
		t.Fatal("worker cleanup did not read back the owner while REMOVE was pending")
	}
	require.False(t, runtime.MockRuntime.GetDeleteCalled())

	releaseRemove()
	require.NoError(t, <-deleteDone)
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.deleteRetryRequested[request.Identity.SandboxUID] == metadata
	}, time.Second, 10*time.Millisecond, "an ACK arriving during the worker's stale read-back must queue a retry")
	releaseOwnerRead()
	require.Eventually(t, runtime.MockRuntime.GetDeleteCalled, time.Second, 10*time.Millisecond,
		"the queued cleanup must re-read retirement and delete the parked runtime")
}

func TestRecoverRetriesStrictOwnerRetirementBeforeRuntimeCleanup(t *testing.T) {
	identity := fastletapi.SandboxIdentity{
		SandboxUID: "recover-v2", Namespace: "default", Name: "recover-v2", FastletPodUID: "pod-uid-a",
		InstanceGeneration: 3, RuntimeInstanceID: "runtime-recover-v2", AssignmentAttempt: 2, RouteGeneration: 4,
	}
	metadata := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{
		Identity: identity, ActionsAPIVersion: actionapi.APIVersionV2,
		Desired: fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
	}, Phase: "running"}
	baseRuntime := newAdmissionRuntime()
	baseRuntime.managed = []*SandboxMetadata{metadata}
	runtime := &recoveryAdmissionRuntime{admissionRuntime: baseRuntime}
	cleanupCalls := 0
	runtime.recoverFn = func(ctx context.Context, managed []*SandboxMetadata, retireOwner runtimecontract.ResourceRecoveryOwnerRetirer) error {
		require.Len(t, managed, 1)
		if err := retireOwner(ctx, managed[0]); err != nil {
			return err
		}
		cleanupCalls++
		baseRuntime.mu.Lock()
		baseRuntime.managed = nil
		baseRuntime.mu.Unlock()
		return nil
	}
	caller := &strictDeleteActionCaller{failRemove: true}
	attachment := actionAttachment(metadata)
	caller.owner, caller.ownerSet = actionOwnerForTest(attachment), true
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
		Name: "egress", TargetHTTPPort: 18080,
	}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)

	require.ErrorContains(t, manager.Recover(context.Background()), "injected strict REMOVE timeout")
	require.Zero(t, cleanupCalls, "runtime resource cleanup must wait for exact-owner retirement")
	require.False(t, manager.Ready())
	caller.mu.Lock()
	caller.failRemove = false
	caller.mu.Unlock()
	require.NoError(t, manager.Recover(context.Background()), "startup recovery must be retryable after owner retirement succeeds")
	require.Equal(t, 1, cleanupCalls)
	require.True(t, manager.Ready())
}

func TestActionsV2DeleteDoesNotDowngradeMissingOrUnknownRuntimeMode(t *testing.T) {
	for _, storedVersion := range []string{"", "sandbox.fast.io/actions/v3"} {
		t.Run(fmt.Sprintf("stored-%q", storedVersion), func(t *testing.T) {
			runtime := newAdmissionRuntime()
			caller := &strictDeleteActionCaller{}
			actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, actionapi.APIVersionV2, caller)
			require.NoError(t, err)
			probeCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			actionManager.Start(probeCtx)
			require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
			manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager})
			require.NoError(t, err)
			identity := fastletapi.SandboxIdentity{
				SandboxUID: "uid-unmarked", Namespace: "default", Name: "sandbox-unmarked", FastletPodUID: "pod-uid-a",
				InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-unmarked",
			}
			manager.sandboxes[identity.SandboxUID] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{
				Identity: identity, ActionsAPIVersion: storedVersion, Desired: fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
			}, Phase: sandboxStateRunning, AppliedGeneration: 1}

			_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: identity})
			require.Error(t, err)
			_, deleteCalls := runtime.counts()
			require.Zero(t, deleteCalls, "unknown or absent durable mode must not use legacy teardown")
			caller.mu.Lock()
			require.Zero(t, caller.removeCalls)
			caller.mu.Unlock()
		})
	}
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

func TestActionsV2CreateRejectsSnapshotRestoreBeforeRuntimeEffects(t *testing.T) {
	runtime := newAdmissionRuntime()
	caller := &restartAwareActionCaller{instance: "handler-1"}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
		Name: "egress", TargetHTTPPort: 18080,
	}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-v2-restore", 1, 1)
	request.Sandbox.Restore = &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/manifest", ArtifactDigest: "sha256:digest"}

	_, err = manager.CreateSandbox(context.Background(), request)
	require.Error(t, err)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorSnapshotUnsupported, failure.Code)
	ensureCalls, deleteCalls := runtime.counts()
	require.Zero(t, ensureCalls, "unsupported restore must not reach the runtime")
	require.Zero(t, deleteCalls)
	admission, _, _ := manager.State()
	require.Zero(t, admission.Used)
}

func TestActionsV2HandlerMismatchRejectsCreateBeforeRuntimeEffects(t *testing.T) {
	runtime := newAdmissionRuntime()
	caller := &legacyOnlyActionCaller{}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, func() bool { return caller.statusCalls.Load() > 0 }, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager})
	require.NoError(t, err)
	response, err := manager.CreateSandbox(context.Background(), ensureRequest("sandbox-v2-mismatch", 1, 1))
	require.Error(t, err)
	require.Equal(t, fastletapi.CreateDispositionRejectedBeforeSideEffects, response.Disposition)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorActionUnavailable, failure.Code)
	ensureCalls, deleteCalls := runtime.counts()
	require.Zero(t, ensureCalls)
	require.Zero(t, deleteCalls)
	require.Zero(t, caller.invokeCalls.Load())
}

func TestCreatePersistsActionsModeFromRuntimePlanNotCallerRequest(t *testing.T) {
	runtime := newAdmissionRuntime()
	caller := &strictDeleteActionCaller{}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{
		Name: "egress", TargetHTTPPort: 18080,
	}}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actionManager.Start(probeCtx)
	require.Eventually(t, actionManager.Ready, time.Second, 10*time.Millisecond)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	request := ensureRequest("sandbox-v2-mode", 1, 1)
	_, err = manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, actionapi.APIVersionV2, manager.sandboxes[request.Identity.SandboxUID].Config.ActionsAPIVersion)
	require.Equal(t, actionapi.APIVersionV2, runtime.sandboxes[request.Identity.SandboxUID].Config.ActionsAPIVersion)
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
	runtime.listEntered, runtime.listRelease = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(runtime.listRelease) })
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- manager.Recover(context.Background()) }()
	<-runtime.listEntered
	identity := ensureRequest("sandbox-a", 2, 3).Identity
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: identity})
	requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
	_, err = manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: identity})
	requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
	release()
	require.NoError(t, <-done)
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
	// Settle on the delete-failed observation instead of racing a removal-count
	// window: a concurrent route publication may legitimately lose the phase
	// race against this delete and add an extra idempotent RemoveRoute through
	// the stale-route cleanup in publishDataPlaneRoute, so exact-once removal
	// is not a system guarantee. Delete-failed is only entered by the gated
	// asyncDelete after a failed removal, so reaching it proves a removal was
	// attempted ahead of runtime deletion.
	require.Eventually(t, func() bool {
		inspected, inspectErr := manager.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: request.Identity})
		return inspectErr == nil && inspected.Sandbox.DataPlane.State == fastletapi.DataPlaneStateFailed
	}, 3*time.Second, 5*time.Millisecond)
	// Stable while removeError stays set: no path can reach the runtime delete.
	_, deletes := runtime.counts()
	require.Equal(t, 0, deletes, "runtime deletion must stay gated while route removal fails")

	// Clearing removeError only after delete-failed settled also guarantees
	// this retry cannot be swallowed by the terminating/deleting idempotency
	// fence while the first asyncDelete was still in flight.
	publisher.mu.Lock()
	publisher.removeError = nil
	publisher.mu.Unlock()
	_, err = manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, deletes := runtime.counts()
		return deletes == 1
	}, 3*time.Second, 5*time.Millisecond)
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

func TestDeleteBeforeCreateFencesDelayedAdmission(t *testing.T) {
	runtime := newAdmissionRuntime()
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 1)
	_, err := manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err)
	_, err = manager.CreateSandbox(context.Background(), request)
	requireFastletCode(t, err, fastletapi.ErrorGenerationFenced)
	creates, deletes := runtime.counts()
	require.Zero(t, creates)
	require.Zero(t, deletes)
	admission, _, _ := manager.State()
	require.Zero(t, admission.Used)
}

func TestStrictDeleteBeforeCreateSurvivesManagerRestart(t *testing.T) {
	caller := &strictDeleteActionCaller{failRemove: true}
	newManager := func() (*SandboxManager, *admissionRuntime) {
		actions, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, actionapi.APIVersionV2, caller)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		actions.Start(ctx)
		require.Eventually(t, actions.Ready, time.Second, 10*time.Millisecond)
		runtime := newAdmissionRuntime()
		manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actions})
		require.NoError(t, err)
		return manager, runtime
	}
	manager, _ := newManager()
	request := ensureRequest("never-created", 1, 1)
	request.Identity.RouteGeneration = 1
	_, err := manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.Error(t, err, "absence cannot hide failed retirement")
	caller.mu.Lock()
	caller.failRemove, caller.lostRemoveReply = false, true
	caller.mu.Unlock()
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.NoError(t, err, "exact retired read-back resolves a lost REMOVE response")
	caller.mu.Lock()
	require.Equal(t, "legacy", caller.lastRemove.Mode)
	require.Equal(t, actionOwnerForTest(actionAttachment(&SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: request.Identity, ActionsAPIVersion: actionapi.APIVersionV2}})), *caller.lastRemove.Owner)
	caller.mu.Unlock()
	restarted, runtime := newManager()
	stale := request.Identity
	stale.RuntimeInstanceID = "other-runtime"
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: stale})
	require.Error(t, err, "an absent local entry cannot confirm another owner")
	caller.mu.Lock()
	caller.failRead = true
	caller.mu.Unlock()
	_, err = manager.DeleteSandboxContext(context.Background(), &fastletapi.DeleteSandboxRequest{Identity: request.Identity})
	require.ErrorContains(t, err, "owner read failure")
	_, err = restarted.CreateSandbox(context.Background(), request)
	require.ErrorContains(t, err, "owner read failure")
	ensures, _ := runtime.counts()
	require.Zero(t, ensures)
	caller.mu.Lock()
	caller.failRead = false
	caller.mu.Unlock()
	_, err = restarted.CreateSandbox(context.Background(), request)
	require.ErrorContains(t, err, "retired")
	ensures, _ = runtime.counts()
	require.Zero(t, ensures, "reject before starting an orphan runtime")
	admission, _, _ := restarted.State()
	require.Zero(t, admission.Used)
	_, err = restarted.CreateSandbox(context.Background(), ensureRequest("new-uid", 1, 1))
	require.NoError(t, err, "a retired UID does not prevent a distinct new UID")
	ensures, _ = runtime.counts()
	require.Equal(t, 1, ensures)
}
