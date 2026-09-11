package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeRegistry struct {
	candidates []placement.FastletInfo
	fastlets   map[placement.FastletID]placement.FastletInfo
	feedback   []placement.FastletID
}

func (r *fakeRegistry) TopK(placement.CandidateRequest, int) []placement.FastletInfo {
	return append([]placement.FastletInfo(nil), r.candidates...)
}

func (r *fakeRegistry) GetFastletByID(id placement.FastletID) (placement.FastletInfo, bool) {
	value, ok := r.fastlets[id]
	return value, ok
}

func (r *fakeRegistry) RecordFeedback(id placement.FastletID, _ placement.LocalFeedback) {
	r.feedback = append(r.feedback, id)
}

type fakeFastletClient struct {
	create       func(string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error)
	inspect      func(string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error)
	inspectCalls int
	deleted      bool
	actions      *fastletapi.ReconcileBindingsRequest
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

func (f *fakeFastletClient) DeleteSandbox(context.Context, string, *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	f.deleted = true
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
		require.Empty(t, request.Sandbox.CPU, "Fastlet injects its fixed resource profile")
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
			SandboxResources: apiv1alpha2.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("512Mi"), PIDs: 256,
			},
			FastletTemplate: corev1.PodTemplateSpec{},
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
