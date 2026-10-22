package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type fakeRegistry struct {
	candidates  []placement.FastletInfo
	fastlets    map[placement.FastletID]placement.FastletInfo
	feedback    []placement.FastletID
	feedbacks   []placement.LocalFeedback
	lastRequest placement.CandidateRequest
}

func (r *fakeRegistry) TopK(request placement.CandidateRequest, _ int) []placement.FastletInfo {
	r.lastRequest = request
	return append([]placement.FastletInfo(nil), r.candidates...)
}

func (r *fakeRegistry) GetFastletByID(id placement.FastletID) (placement.FastletInfo, bool) {
	value, ok := r.fastlets[id]
	return value, ok
}

func (r *fakeRegistry) RecordFeedback(id placement.FastletID, feedback placement.LocalFeedback) {
	r.feedback = append(r.feedback, id)
	r.feedbacks = append(r.feedbacks, feedback)
}

func (*fakeRegistry) ApplyHeartbeat(placement.FastletID, string, *fastletapi.HeartbeatResponse, time.Time) error {
	return nil
}

type fakeFastletClient struct {
	heartbeat func(string, *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error)

	create       func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error)
	inspect      func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error)
	delete       func(string, *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error)
	inspectCalls int
	deleted      bool
	actions      *fastletapi.ReconcileBindingsRequest
}

func (f *fakeFastletClient) Heartbeat(_ context.Context, ip string, request *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error) {
	if f.heartbeat != nil {
		return f.heartbeat(ip, request)
	}
	return &fastletapi.HeartbeatResponse{}, nil
}

func (f *fakeFastletClient) ReconcileBindings(_ context.Context, _ string, request *fastletapi.ReconcileBindingsRequest) (*fastletapi.ReconcileBindingsResponse, error) {
	f.actions = request
	return &fastletapi.ReconcileBindingsResponse{Sandbox: &fastletapi.SandboxStatus{
		SandboxID:          request.Identity.SandboxUID,
		Runtime:            fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane:          fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateReady},
		AcceptedGeneration: request.SpecGeneration,
		AppliedGeneration:  request.SpecGeneration,
		ActionBindings:     []fastletapi.ActionBindingStatus{{Handler: "egress", State: "Ready", ObservedSpecGeneration: request.SpecGeneration}},
	}}, nil
}

func (f *fakeFastletClient) CreateSandbox(_ context.Context, ip string, request *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
	return f.create(ip, request)
}

func (f *fakeFastletClient) InspectSandbox(_ context.Context, ip string, request *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
	f.inspectCalls++
	return f.inspect(ip, request)
}

func (f *fakeFastletClient) DeleteSandbox(_ context.Context, ip string, request *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	f.deleted = true
	if f.delete != nil {
		return f.delete(ip, request)
	}
	return &fastletapi.DeleteSandboxResponse{}, nil
}

func (f *fakeFastletClient) CreateSnapshot(context.Context, string, *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
	return nil, errors.New("snapshot create is not configured in this fake")
}

func (f *fakeFastletClient) InspectSnapshot(context.Context, string, *fastletapi.InspectSnapshotRequest) (*fastletapi.InspectSnapshotResponse, error) {
	return nil, errors.New("snapshot inspect is not configured in this fake")
}

func (f *fakeFastletClient) DeleteSnapshot(context.Context, string, *fastletapi.DeleteSnapshotRequest) (*fastletapi.DeleteSnapshotResponse, error) {
	return nil, errors.New("snapshot delete is not configured in this fake")
}

func TestFastPathCandidatesIsRegistryOnly(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a",
	}
	registry.candidates = []placement.FastletInfo{candidate}

	candidates, err := orchestrator.FastPathCandidates(sandbox, "request-a")
	require.NoError(t, err)
	require.Equal(t, candidate.ID, candidates[0].ID)
}

func TestCandidatesPassesImmutablePoolResourceDemandToPlacement(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-a"}, &pool))
	pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("768Mi"), resource.MustParse("4"), resource.MustParse("2"), 256,
	)
	require.NoError(t, orchestrator.Client.Update(context.Background(), &pool))
	registry.candidates = []placement.FastletInfo{{ID: "fastlet-a"}}

	_, parameters, err := orchestrator.Candidates(context.Background(), sandbox, "request-a")
	require.NoError(t, err)
	require.Equal(t, apiv1alpha2.SandboxResourceModeResourceAware, parameters.ResourceMode)
	require.Equal(t, int64(250), registry.lastRequest.Demand.CPU.MilliValue())
	expectedMemory := resource.MustParse("384Mi")
	require.Equal(t, expectedMemory.Value(), registry.lastRequest.Demand.Memory.Value())
}

func TestResourceAwarePlacementCanBeDisabledWithoutChangingPoolProfile(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-a"}, &pool))
	pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("768Mi"), resource.MustParse("4"), resource.MustParse("2"), 256,
	)
	require.NoError(t, orchestrator.Client.Update(context.Background(), &pool))
	registry.candidates = []placement.FastletInfo{{ID: "fastlet-a"}}
	orchestrator.DisableResourceAwarePlacement = true

	_, parameters, err := orchestrator.Candidates(context.Background(), sandbox, "request-a")
	require.NoError(t, err)
	require.Equal(t, apiv1alpha2.SandboxResourceModeResourceAware, parameters.ResourceMode)
	require.Equal(t, apiv1alpha2.SandboxResourceModeLegacySlot, registry.lastRequest.ResourceMode)
}

func TestResolveSandboxResourcesRejectsLegacyOverrides(t *testing.T) {
	_, _, _, sandbox := newHarness(t)
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{
		MaxSandboxesPerPod: 8,
		SandboxResources: apiv1alpha2.NewLegacySandboxResourceProfile(
			resource.MustParse("1"), resource.MustParse("1Gi"), 256,
		),
	}}
	sandbox.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
		CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"),
	}}

	_, _, err := ResolveSandboxResources(pool, sandbox)
	require.ErrorContains(t, err, "per-Sandbox resources require ResourceAware mode")
}

func TestResolveSandboxResourcesDerivesRequestAndRejectsLimitAboveEnvelope(t *testing.T) {
	_, _, _, sandbox := newHarness(t)
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{
		MaxSandboxesPerPod: 8,
		SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
			resource.MustParse("1"), resource.MustParse("4Gi"), resource.MustParse("4"), resource.MustParse("2"), 256,
		),
	}}
	sandbox.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
		CPU: resource.MustParse("2"), Memory: resource.MustParse("6Gi"),
	}}

	resolved, demand, err := ResolveSandboxResources(pool, sandbox)
	require.NoError(t, err)
	require.Equal(t, "500m", resolved.Requests.CPU)
	require.Equal(t, "3Gi", resolved.Requests.Memory)
	require.Equal(t, "2", resolved.Limits.CPU)
	require.Equal(t, "6Gi", resolved.Limits.Memory)
	require.Equal(t, int64(500), demand.CPU.MilliValue())

	sandbox.Spec.Resources.Limits.CPU = resource.MustParse("3")
	_, _, err = ResolveSandboxResources(pool, sandbox)
	require.ErrorContains(t, err, "exceed Fastlet runtime envelope")
}

func TestResolveRuntimeRejectsResourceMutationAfterAssignment(t *testing.T) {
	orchestrator, _, _, sandbox := newHarness(t)
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-a"}, &pool))
	pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("4Gi"), resource.MustParse("4"), resource.MustParse("2"), 256,
	)
	require.NoError(t, orchestrator.Client.Update(context.Background(), &pool))
	sandbox.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
		CPU: resource.MustParse("2"), Memory: resource.MustParse("6Gi"),
	}}

	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	sandbox.Annotations = resolvedResourceAnnotations(parameters)
	// 1999m still derives the same rounded 500m request. The resolved-limit
	// fence must therefore detect a change that request annotations alone miss.
	sandbox.Spec.Resources.Limits.CPU = resource.MustParse("1999m")
	_, err = orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.ErrorContains(t, err, assignment.AnnotationResolvedLimitCPU)
}

func TestReconcileBindingsPreservesOpaqueInputAndReturnsObservation(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-a"}, &pool))
	pool.Spec.ActionHandlers = []apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}
	require.NoError(t, orchestrator.Client.Update(context.Background(), &pool))
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a",
	}
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid")
	sandbox.Generation = 4
	sandbox.Spec.ActionBindings = []apiv1alpha2.ActionBinding{{Handler: "egress", Input: `{"z":1,"a":2}`}}
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	observed, err := orchestrator.ReconcileBindings(context.Background(), sandbox)
	require.NoError(t, err)
	require.NotNil(t, fastletClient.actions)
	require.Equal(t, int64(4), fastletClient.actions.SpecGeneration)
	require.Equal(t, `{"z":1,"a":2}`, fastletClient.actions.ActionBindings[0].Input)
	require.Equal(t, int64(4), observed.AppliedGeneration)
	// Orchestrator returns an observation; only SandboxReconciler owns CRD Status.
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &persisted))
	require.Empty(t, persisted.Status.ActionBindings)
}

func TestProjectObservedDemotesReadyFromOlderAppliedGeneration(t *testing.T) {
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Generation: 5},
		Spec:       apiv1alpha2.SandboxSpec{ActionBindings: []apiv1alpha2.ActionBinding{{Handler: "egress"}}},
	}
	status := &apiv1alpha2.SandboxStatus{}
	ProjectObservedStatus(status, sandbox, &fastletapi.SandboxStatus{
		Runtime:           fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane:         fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateUnavailable},
		AppliedGeneration: 4,
		ActionBindings:    []fastletapi.ActionBindingStatus{{Handler: "egress", State: "Ready", ObservedSpecGeneration: 4}},
	})

	require.Equal(t, int64(5), status.ObservedGeneration)
	require.Equal(t, apiv1alpha2.DataPlaneUnavailable, status.DataPlane.State)
	require.Equal(t, apiv1alpha2.ActionPending, status.ActionBindings[0].State)
	require.False(t, status.HasCondition(ConditionReady, metav1.ConditionTrue, "Ready"))
}

func TestProjectObservedUsesLatestCurrentGenerationActionTransition(t *testing.T) {
	oldTransition := metav1.NewTime(time.Date(2026, time.August, 30, 10, 12, 25, 0, time.UTC))
	newTransition := oldTransition.Add(time.Minute)
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       apiv1alpha2.SandboxSpec{ActionBindings: []apiv1alpha2.ActionBinding{{Handler: "egress"}}},
	}
	status := &apiv1alpha2.SandboxStatus{ActionBindings: []apiv1alpha2.ActionBindingStatus{{
		Handler: "egress", State: apiv1alpha2.ActionReady, LastTransitionTime: &oldTransition,
	}}}

	ProjectObservedStatus(status, sandbox, &fastletapi.SandboxStatus{
		Runtime:           fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane:         fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateReady},
		AppliedGeneration: 2,
		ActionBindings: []fastletapi.ActionBindingStatus{{
			Handler: "egress", State: "Ready", ObservedSpecGeneration: 2, LastTransitionTime: newTransition,
		}},
	})

	require.Equal(t, newTransition, status.ActionBindings[0].LastTransitionTime.Time)
	require.True(t, status.HasCondition(ConditionReady, metav1.ConditionTrue, "Ready"))
}

func TestProjectObservedNeverMovesActionTransitionBackward(t *testing.T) {
	currentTransition := metav1.NewTime(time.Date(2026, time.August, 30, 10, 13, 25, 0, time.UTC))
	olderTransition := currentTransition.Add(-time.Minute)
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       apiv1alpha2.SandboxSpec{ActionBindings: []apiv1alpha2.ActionBinding{{Handler: "egress"}}},
	}
	status := &apiv1alpha2.SandboxStatus{ActionBindings: []apiv1alpha2.ActionBindingStatus{{
		Handler: "egress", State: apiv1alpha2.ActionReady, LastTransitionTime: &currentTransition,
	}}}

	ProjectObservedStatus(status, sandbox, &fastletapi.SandboxStatus{
		Runtime:           fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane:         fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateReady},
		AppliedGeneration: 2,
		ActionBindings: []fastletapi.ActionBindingStatus{{
			Handler: "egress", State: "Ready", ObservedSpecGeneration: 2, LastTransitionTime: olderTransition,
		}},
	})

	require.Equal(t, currentTransition.Time, status.ActionBindings[0].LastTransitionTime.Time)
}

func TestAssignDeclarativeProjectsAnnotationAndEnsureReturnsObservation(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.candidates = []placement.FastletInfo{candidate}
	registry.fastlets[candidate.ID] = candidate

	sandbox.UID = types.UID("sandbox-uid-a")
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	assigned, won, err := orchestrator.AssignDeclarative(context.Background(), sandbox, "sandbox-uid-a")
	require.NoError(t, err)
	require.True(t, won)
	require.NotEmpty(t, assigned.Status.Placement.FastletName)
	envelope, err := assignment.EffectiveAssignment(assigned)
	require.NoError(t, err)
	require.NotEmpty(t, envelope.RuntimeInstanceID)

	fastletClient.create = func(ip string, request *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
		require.Equal(t, candidate.PodIP, ip)
		require.Equal(t, "sandbox-uid-a", request.Identity.SandboxUID)
		require.Equal(t, envelope.RuntimeInstanceID, request.Identity.RuntimeInstanceID)
		require.Equal(t, parameters.ResourceProfileHash, request.Sandbox.ResourceProfileHash)
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionCreated, Sandbox: readyFastletObservation("sandbox-uid-a", assigned.Generation)}, nil
	}
	observed, err := orchestrator.EnsureRuntime(context.Background(), assigned)
	require.NoError(t, err)
	require.Equal(t, fastletapi.RuntimeStateReady, observed.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateReady, observed.DataPlane.State)
	// EnsureRuntime itself never patches the persisted business Status.
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &persisted))
	require.Empty(t, persisted.Status.Runtime.State)
}

func TestAssignDeclarativeProjectsResolvedResourceAnnotations(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-a"}, &pool))
	pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("4Gi"), resource.MustParse("4"), resource.MustParse("2"), 256,
	)
	require.NoError(t, orchestrator.Client.Update(context.Background(), &pool))
	sandbox.Spec.Resources = &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
		CPU: resource.MustParse("2"), Memory: resource.MustParse("6Gi"),
	}}
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.candidates = []placement.FastletInfo{candidate}
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	assigned, won, err := orchestrator.AssignDeclarative(context.Background(), sandbox, "sandbox-uid-a")
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, pool.Spec.SandboxResources.Hash(), assigned.Annotations[assignment.AnnotationResourcePolicyHash])
	require.Equal(t, "500m", assigned.Annotations[assignment.AnnotationResolvedRequestCPU])
	require.Equal(t, "3Gi", assigned.Annotations[assignment.AnnotationResolvedRequestMemory])
	require.Equal(t, "2", assigned.Annotations[assignment.AnnotationResolvedLimitCPU])
	require.Equal(t, "6Gi", assigned.Annotations[assignment.AnnotationResolvedLimitMemory])
}

func TestObserveRuntimeReturnsStructuredRuntimeAndDataPlaneIndependently(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.candidates = []placement.FastletInfo{candidate}
	registry.fastlets[candidate.ID] = candidate

	sandbox.UID = types.UID("sandbox-uid-a")
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	assigned, _, err := orchestrator.AssignDeclarative(context.Background(), sandbox, "sandbox-uid-a")
	require.NoError(t, err)

	observation := &fastletapi.SandboxStatus{
		SandboxID: "sandbox-uid-a",
		Runtime:   fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane: fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStatePublishing},
	}
	fastletClient.inspect = func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
		return &fastletapi.InspectSandboxResponse{Sandbox: observation}, nil
	}
	observed, err := orchestrator.ObserveRuntime(context.Background(), assigned)
	require.NoError(t, err)
	require.Equal(t, fastletapi.RuntimeStateReady, observed.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStatePublishing, observed.DataPlane.State)

	observation.DataPlane.State = fastletapi.DataPlaneStateUnavailable
	observed, err = orchestrator.ObserveRuntime(context.Background(), assigned)
	require.NoError(t, err)
	require.Equal(t, fastletapi.DataPlaneStateUnavailable, observed.DataPlane.State)
}

func TestLostCreateResponseDoesNotInspectOrChangeIdentity(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(candidate, 2, 1, 3, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = statusFromEnvelope(envelope)
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	fastletClient.create = func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
		return nil, errors.New("response lost")
	}

	_, err = orchestrator.EnsureRuntime(context.Background(), sandbox)
	require.ErrorIs(t, err, ErrUnknownFastletOutcome)
	require.Zero(t, fastletClient.inspectCalls)
	current, parseErr := assignment.AssignmentFromAnnotation(sandbox)
	require.NoError(t, parseErr)
	require.Equal(t, envelope, *current)
}

func TestEnsureRuntimeAcceptsParkedColdCreate(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(candidate, 2, 1, 3, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = statusFromEnvelope(envelope)
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	fastletClient.create = func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
		return &fastletapi.CreateSandboxResponse{
			Disposition: fastletapi.CreateDispositionCreated,
			Sandbox: &fastletapi.SandboxStatus{
				SandboxID: string(sandbox.UID),
				Runtime:   fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateCreating, Message: "sandbox image is being delivered to the node"},
				DataPlane: fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStatePending},
			},
		}, nil
	}

	observed, err := orchestrator.EnsureRuntime(context.Background(), sandbox)
	require.NoError(t, err, "a parked cold create is an accepted create, not a failure")
	require.Equal(t, fastletapi.RuntimeStateCreating, observed.Runtime.State)
}

func TestCreateRejectionImmediatelyForwardsAdmissionSnapshot(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	resources := &fastletapi.ResourceAdmissionStatus{
		Version: "v2", Epoch: "resource-epoch-a", Revision: 9,
		Capacity:  fastletapi.SandboxResourceVector{CPU: "2", Memory: "8Gi"},
		Committed: fastletapi.SandboxResourceVector{CPU: "1", Memory: "4Gi"},
		State:     fastletapi.AdmissionStateBlocked, Reason: fastletapi.AdmissionReasonFastletMemoryHighWatermark,
	}
	response := &fastletapi.CreateSandboxResponse{
		Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects,
		Admission:   fastletapi.AdmissionStatus{Capacity: 8, Used: 5, Running: 5, Resources: resources},
	}
	failure := &fastletapi.FastletError{
		Code: fastletapi.ErrorCapacityRejected, Reason: fastletapi.AdmissionReasonFastletMemoryHighWatermark,
		Message: "memory pressure blocks admission", Retryable: true,
	}
	fastletClient.create = func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
		return response, &fastletapi.CreateCallError{Disposition: response.Disposition, Failure: failure}
	}

	returned, err := orchestrator.CreateRuntime(context.Background(), sandbox)
	require.Error(t, err)
	require.Same(t, response, returned)
	require.Len(t, registry.feedbacks, 1)
	require.Equal(t, failure.Reason, registry.feedbacks[0].Reason)
	require.NotNil(t, registry.feedbacks[0].Admission)
	require.Equal(t, uint64(9), registry.feedbacks[0].Admission.Resources.Revision)
}

func TestCreateSuccessImmediatelyForwardsAdmissionSnapshot(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, orchestrator.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-a"}, &pool))
	pool.Spec.SandboxResources = apiv1alpha2.NewResourceAwareSandboxResourceProfile(
		resource.MustParse("2"), resource.MustParse("8Gi"), resource.MustParse("4"), resource.MustParse("2"), 256,
	)
	require.NoError(t, orchestrator.Client.Update(context.Background(), &pool))
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	resources := &fastletapi.ResourceAdmissionStatus{
		Version: "v2", Epoch: "resource-epoch-a", Revision: 3,
		Capacity:  fastletapi.SandboxResourceVector{CPU: "4", Memory: "16Gi"},
		Committed: fastletapi.SandboxResourceVector{CPU: "500m", Memory: "4Gi"},
		State:     fastletapi.AdmissionStateOpen,
	}
	fastletClient.create = func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
		return &fastletapi.CreateSandboxResponse{
			Disposition: fastletapi.CreateDispositionCreated,
			Sandbox:     readyFastletObservation(string(sandbox.UID), sandbox.Generation),
			Admission:   fastletapi.AdmissionStatus{Capacity: 8, Used: 1, Running: 1, Resources: resources},
		}, nil
	}

	_, err = orchestrator.CreateRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	require.Len(t, registry.feedbacks, 1)
	require.NotNil(t, registry.feedbacks[0].Admission)
	require.Equal(t, uint64(3), registry.feedbacks[0].Admission.Resources.Revision)
	require.Equal(t, "500m", registry.feedbacks[0].Admission.Resources.Committed.CPU)
}

func TestDeleteImmediatelyForwardsAdmissionSnapshot(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	resources := &fastletapi.ResourceAdmissionStatus{
		Version: "v2", Epoch: "resource-epoch-a", Revision: 4,
		Capacity:  fastletapi.SandboxResourceVector{CPU: "2", Memory: "8Gi"},
		Committed: fastletapi.SandboxResourceVector{CPU: "0", Memory: "0"},
		State:     fastletapi.AdmissionStateOpen,
	}
	fastletClient.delete = func(string, *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
		return &fastletapi.DeleteSandboxResponse{
			Admission: fastletapi.AdmissionStatus{Capacity: 8, Resources: resources},
		}, nil
	}

	require.NoError(t, orchestrator.DeleteRuntime(context.Background(), sandbox))
	require.Len(t, registry.feedbacks, 1)
	require.NotNil(t, registry.feedbacks[0].Admission)
	require.Equal(t, uint64(4), registry.feedbacks[0].Admission.Resources.Revision)
	require.Equal(t, "0", registry.feedbacks[0].Admission.Resources.Committed.CPU)
}

func TestCreateNeedsLocalReservationOnlyBeforeCurrentIdentityIsReported(t *testing.T) {
	sandbox := &apiv1alpha2.Sandbox{Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimePending}}}
	identity := fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", InstanceGeneration: 2, AssignmentAttempt: 3}
	fastlet := placement.FastletInfo{SandboxStatuses: map[string]fastletapi.SandboxStatus{}}
	require.True(t, createNeedsLocalReservation(sandbox, fastlet, identity))

	fastlet.SandboxStatuses[identity.SandboxUID] = fastletapi.SandboxStatus{
		SandboxID: identity.SandboxUID, InstanceGeneration: identity.InstanceGeneration, AssignmentAttempt: identity.AssignmentAttempt,
	}
	require.False(t, createNeedsLocalReservation(sandbox, fastlet, identity))

	fastlet.SandboxStatuses[identity.SandboxUID] = fastletapi.SandboxStatus{
		SandboxID: identity.SandboxUID, InstanceGeneration: identity.InstanceGeneration, AssignmentAttempt: identity.AssignmentAttempt - 1,
	}
	require.True(t, createNeedsLocalReservation(sandbox, fastlet, identity))

	sandbox.Status.Runtime.State = apiv1alpha2.RuntimeCreating
	require.False(t, createNeedsLocalReservation(sandbox, placement.FastletInfo{}, identity))
}

func readyFastletObservation(sandboxID string, generation int64) *fastletapi.SandboxStatus {
	return &fastletapi.SandboxStatus{
		SandboxID:          sandboxID,
		Runtime:            fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane:          fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateReady},
		AcceptedGeneration: generation,
		AppliedGeneration:  generation,
	}
}

func TestReassignDeclarativeAfterRejectionCASesDirectlyToAlternative(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	first := candidateFor(parameters)
	second := first
	second.ID, second.PodName, second.PodUID, second.PodIP, second.NodeName = "fastlet-b", "fastlet-b", "pod-b", "10.0.0.2", "node-b"
	registry.candidates = []placement.FastletInfo{first, second}
	registry.fastlets[first.ID] = first
	registry.fastlets[second.ID] = second

	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(first, 3, 2, 5, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = statusFromEnvelope(envelope)
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	updated, moved, err := orchestrator.ReassignDeclarativeAfterRejection(context.Background(), sandbox, string(sandbox.UID))
	require.NoError(t, err)
	require.True(t, moved)
	next, err := assignment.AssignmentFromAnnotation(updated)
	require.NoError(t, err)
	require.Equal(t, second.PodName, next.FastletName)
	require.Equal(t, second.PodUID, next.FastletPodUID)
	require.Equal(t, int64(4), next.Attempt)
	require.Equal(t, int64(2), next.InstanceGeneration)
	require.Equal(t, int64(6), next.RouteGeneration)
	require.NotEqual(t, envelope.RuntimeInstanceID, next.RuntimeInstanceID)
	// Status remains an asynchronous projection; the annotation CAS never
	// passes through an unassigned value.
	require.Equal(t, first.PodName, updated.Status.Placement.FastletName)
}

func TestReassignDeclarativeAfterRejectionPreservesAssignmentWithoutAlternative(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	first := candidateFor(parameters)
	registry.candidates = []placement.FastletInfo{first}
	registry.fastlets[first.ID] = first

	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(first, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	updated, moved, err := orchestrator.ReassignDeclarativeAfterRejection(context.Background(), sandbox, string(sandbox.UID))
	require.NoError(t, err)
	require.False(t, moved)
	current, err := assignment.AssignmentFromAnnotation(updated)
	require.NoError(t, err)
	require.Equal(t, envelope, *current)
}

func TestClearAssignmentRemovesAnnotationAndAdvancesFences(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(candidate, 4, 2, 5, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = statusFromEnvelope(envelope)
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	cleared, err := orchestrator.ClearAssignment(context.Background(), sandbox, true)
	require.NoError(t, err)
	require.Empty(t, cleared.Status.Placement.FastletName)
	require.Equal(t, int64(3), cleared.Status.Runtime.Generation)
	require.Equal(t, int64(6), cleared.Status.DataPlane.RouteGeneration)
	current, err := assignment.AssignmentFromAnnotation(cleared)
	require.NoError(t, err)
	require.Nil(t, current)
}

func TestResumeAssignmentUsesDurableFencesWhenInformerLags(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.candidates = []placement.FastletInfo{candidate}
	registry.fastlets[candidate.ID] = candidate
	sandbox.UID = types.UID("sandbox-uid-a")
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	assigned, _, err := orchestrator.AssignDeclarative(context.Background(), sandbox, string(sandbox.UID))
	require.NoError(t, err)

	for generation := int64(2); generation <= 3; generation++ {
		stale := assigned.DeepCopy()
		delete(stale.Annotations, assignment.AnnotationAssignment)
		_, err := orchestrator.ClearAssignment(context.Background(), assigned, false)
		require.NoError(t, err)
		assigned, _, err = orchestrator.AssignDeclarative(context.Background(), stale, string(sandbox.UID))
		require.NoError(t, err)
		envelope, err := assignment.EffectiveAssignment(assigned)
		require.NoError(t, err)
		require.Equal(t, generation, envelope.RouteGeneration)
		require.Equal(t, generation, envelope.Attempt)
	}
}

func statusFromEnvelope(envelope assignment.AssignmentEnvelope) apiv1alpha2.SandboxStatus {
	return apiv1alpha2.SandboxStatus{
		Placement: envelope.StatusPlacement(),
		Runtime:   apiv1alpha2.RuntimeStatus{Generation: envelope.InstanceGeneration},
		DataPlane: apiv1alpha2.DataPlaneStatus{RouteGeneration: envelope.RouteGeneration},
	}
}

func candidateFor(parameters RuntimeParameters) placement.FastletInfo {
	return placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeName: parameters.RuntimeName, RuntimeProfileHash: parameters.RuntimeProfileHash,
		ResourceProfileHash: parameters.ResourceProfileHash,
		InfraRevision:       parameters.InfraRevision, InfraReady: true,
	}
}

func newHarness(t *testing.T) (*Orchestrator, *fakeRegistry, *fakeFastletClient, *apiv1alpha2.Sandbox) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, Capacity: apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: 8,
			SandboxResources: apiv1alpha2.NewLegacySandboxResourceProfile(
				resource.MustParse("1"), resource.MustParse("512Mi"), 256,
			),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet"}}}},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha2.Sandbox{}).WithObjects(pool).Build()
	registry := &fakeRegistry{fastlets: make(map[placement.FastletID]placement.FastletInfo)}
	fastletClient := &fakeFastletClient{
		create: func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
			return nil, errors.New("unexpected create")
		},
		inspect: func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
			return nil, errors.New("unexpected inspect")
		},
	}
	orchestrator := &Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastletClient}
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", Annotations: map[string]string{
			assignment.AnnotationRequestID: "request-a", assignment.AnnotationCreateSpecHash: "spec-a",
		}},
		Spec: apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	return orchestrator, registry, fastletClient, sandbox
}

func TestPreferFastletMovesResumeTargetFirst(t *testing.T) {
	candidates := []placement.FastletInfo{
		{ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a"},
		{ID: "fastlet-b", PodName: "fastlet-b", PodUID: "pod-b"},
		{ID: "fastlet-c", PodName: "fastlet-c", PodUID: "pod-c"},
	}
	preferred := preferFastlet(candidates, "fastlet-c", "pod-c")
	require.Equal(t, []placement.FastletID{"fastlet-c", "fastlet-a", "fastlet-b"},
		[]placement.FastletID{preferred[0].ID, preferred[1].ID, preferred[2].ID})

	// A replaced pod under the same name falls back to the normal order.
	replaced := preferFastlet(candidates, "fastlet-c", "other-pod")
	require.Equal(t, placement.FastletID("fastlet-a"), replaced[0].ID)

	// Empty or unmatched hints leave the order untouched.
	require.Equal(t, candidates, preferFastlet(candidates, "", ""))
	require.Equal(t, candidates, preferFastlet(candidates, "fastlet-z", "pod-z"))
	// An already-first candidate is a no-op.
	require.Equal(t, candidates, preferFastlet(candidates, "fastlet-a", "pod-a"))
}

func TestRuntimeGoneRequiresExplicitAbsence(t *testing.T) {
	o, registry, f, sandbox := newHarness(t)
	sandbox.UID = "sandbox-uid"
	candidate := placement.FastletInfo{ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a"}
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	gone, err := o.RuntimeGone(context.Background(), sandbox)
	require.ErrorIs(t, err, ErrAssignedFastletUnavailable)
	require.False(t, gone)
	registry.fastlets[candidate.ID] = candidate
	for _, reply := range []*fastletapi.InspectSandboxResponse{nil, {}} {
		f.inspect = func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
			return reply, nil
		}
		gone, err = o.RuntimeGone(context.Background(), sandbox)
		require.ErrorIs(t, err, ErrUnknownFastletOutcome)
		require.False(t, gone)
	}
	f.inspect = func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
		return nil, &fastletapi.FastletError{Code: fastletapi.ErrorNotFound}
	}
	gone, err = o.RuntimeGone(context.Background(), sandbox)
	require.NoError(t, err)
	require.True(t, gone)
}

func TestRuntimeGoneRefreshesCapacityAfterConfirmedDeletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		present      bool
		heartbeatErr bool
		stillFull    bool
		newer        bool
		replaced     bool
	}{
		{name: "freed slot is immediately eligible"},
		{name: "deletion in progress", present: true},
		{name: "heartbeat failure retains capacity", heartbeatErr: true},
		{name: "another sandbox occupies slot", stillFull: true},
		{name: "newer heartbeat wins", newer: true},
		{name: "replacement pod is fenced", replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, f, sandbox := newHarness(t)
			registry := placement.NewInMemoryRegistry()
			o.Registry = registry
			sandbox.UID = "sandbox-uid"
			candidate := placement.FastletInfo{
				ID: "fastlet-a", Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef,
				PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", PodReady: true,
				RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a",
			}
			registry.UpsertPod(candidate)
			heartbeat := &fastletapi.HeartbeatResponse{
				FastletStatus: fastletapi.FastletStatus{
					FastletPodUID: candidate.PodUID, RuntimeReady: true, InfraReady: true,
					RuntimeProfileHash: candidate.RuntimeProfileHash, ResourceProfileHash: candidate.ResourceProfileHash, InfraRevision: candidate.InfraRevision,
					Admission: fastletapi.AdmissionStatus{Capacity: 1, Used: 1, Running: 1},
				},
				Sequence: 1, Cache: fastletapi.CacheSnapshot{Epoch: "boot-a", Revision: 7, Full: true, Complete: true},
			}
			require.NoError(t, registry.ApplyHeartbeat(candidate.ID, candidate.PodUID, heartbeat, time.Now()))
			request := placement.CandidateRequest{Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef}
			require.Empty(t, registry.TopK(request, 1), "full Fastlet is initially filtered out")
			envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
			require.NoError(t, err)
			require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
			f.inspect = func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
				if tc.present {
					return &fastletapi.InspectSandboxResponse{Sandbox: &fastletapi.SandboxStatus{}}, nil
				}
				return nil, &fastletapi.FastletError{Code: fastletapi.ErrorNotFound}
			}
			calls := 0
			f.heartbeat = func(ip string, req *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error) {
				calls++
				require.Equal(t, candidate.PodIP, ip)
				require.Equal(t, fastletapi.CacheCursor{Epoch: "boot-a", Revision: 7}, req.Cache)
				if tc.heartbeatErr {
					return nil, errors.New("heartbeat unavailable")
				}
				heartbeat.Sequence = 2
				heartbeat.Admission = fastletapi.AdmissionStatus{Capacity: 1}
				if tc.newer {
					heartbeat.Sequence = 3
					require.NoError(t, registry.ApplyHeartbeat(candidate.ID, candidate.PodUID, heartbeat, time.Now()))
					heartbeat.Sequence = 2
				}
				if tc.stillFull || tc.newer {
					heartbeat.Admission = fastletapi.AdmissionStatus{Capacity: 1, Used: 1, Running: 1}
				}
				if tc.replaced {
					replacement := candidate
					replacement.PodUID = "replacement"
					registry.UpsertPod(replacement)
				}
				return heartbeat, nil
			}
			gone, err := o.RuntimeGone(context.Background(), sandbox)
			require.NoError(t, err)
			require.Equal(t, !tc.present, gone)
			if tc.present {
				require.Zero(t, calls, "do not refresh before runtime deletion completes")
			} else {
				require.Equal(t, 1, calls)
			}
			eligible := !tc.present && !tc.heartbeatErr && !tc.stillFull && !tc.replaced
			require.Equal(t, eligible, len(registry.TopK(request, 1)) == 1)
			if tc.replaced {
				current, ok := registry.GetFastletByID(candidate.ID)
				require.True(t, ok)
				require.Equal(t, "replacement", current.PodUID)
				require.Zero(t, current.HeartbeatSequence)
			}
		})
	}
}
