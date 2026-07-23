package sandboxorchestrator

import (
	"context"
	"errors"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeRegistry struct {
	candidates []fastletpool.FastletInfo
	fastlets   map[fastletpool.FastletID]fastletpool.FastletInfo
	feedback   []fastletpool.FastletID
}

func (r *fakeRegistry) TopK(fastletpool.CandidateRequest, int) []fastletpool.FastletInfo {
	return append([]fastletpool.FastletInfo(nil), r.candidates...)
}

func (r *fakeRegistry) GetFastletByID(id fastletpool.FastletID) (fastletpool.FastletInfo, bool) {
	value, ok := r.fastlets[id]
	return value, ok
}

func (r *fakeRegistry) RecordFeedback(id fastletpool.FastletID, _ fastletpool.LocalFeedback) {
	r.feedback = append(r.feedback, id)
}

type fakeFastletClient struct {
	create       func(string, *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error)
	inspect      func(string, *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error)
	inspectCalls int
	deleted      bool
}

func (f *fakeFastletClient) CreateSandbox(_ context.Context, ip string, request *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
	return f.create(ip, request)
}

func (f *fakeFastletClient) InspectSandbox(_ context.Context, ip string, request *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
	f.inspectCalls++
	return f.inspect(ip, request)
}

func (f *fakeFastletClient) DeleteSandboxV2(context.Context, string, *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error) {
	f.deleted = true
	return &api.DeleteSandboxV2Response{Accepted: true}, nil
}

func TestFastPathCandidatesIsRegistryOnly(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	candidate := fastletpool.FastletInfo{ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1"}
	registry.candidates = []fastletpool.FastletInfo{candidate}

	candidates, err := orchestrator.FastPathCandidates(sandbox, "request-a")
	require.NoError(t, err)
	require.Equal(t, candidate.ID, candidates[0].ID)
}

func TestAssignDeclarativeProjectsAnnotationAndReconcilesRuntime(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.candidates = []fastletpool.FastletInfo{candidate}
	registry.fastlets[candidate.ID] = candidate

	sandbox.UID = types.UID("sandbox-uid-a")
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	assigned, won, err := orchestrator.AssignDeclarative(context.Background(), sandbox, "sandbox-uid-a")
	require.NoError(t, err)
	require.True(t, won)
	require.NotNil(t, assigned.Status.Assignment)
	envelope, err := common.EffectiveAssignment(assigned)
	require.NoError(t, err)
	require.NotEmpty(t, envelope.RuntimeInstanceID)

	fastletClient.create = func(ip string, request *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
		require.Equal(t, candidate.PodIP, ip)
		require.Equal(t, "sandbox-uid-a", request.Identity.SandboxUID)
		require.Equal(t, envelope.RuntimeInstanceID, request.Identity.RuntimeInstanceID)
		require.Empty(t, request.Sandbox.CPU, "Fastlet injects its fixed resource profile")
		return &api.CreateSandboxResponse{Accepted: true, Sandbox: &api.SandboxStatus{SandboxID: "sandbox-uid-a", Phase: "running"}}, nil
	}
	require.NoError(t, orchestrator.ReconcileRuntime(context.Background(), assigned))

	var ready apiv1alpha1.Sandbox
	require.NoError(t, orchestrator.Client.Get(context.Background(), client.ObjectKeyFromObject(assigned), &ready))
	require.Equal(t, apiv1alpha1.ObservedStateReady, ready.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateReady, ready.Status.DataPlaneState)
}

func TestReconcileRuntimeProjectsRuntimeAndDataPlaneIndependently(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := candidateFor(parameters)
	registry.candidates = []fastletpool.FastletInfo{candidate}
	registry.fastlets[candidate.ID] = candidate

	sandbox.UID = types.UID("sandbox-uid-a")
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	assigned, _, err := orchestrator.AssignDeclarative(context.Background(), sandbox, "sandbox-uid-a")
	require.NoError(t, err)

	phase := "infra-pending"
	fastletClient.create = func(string, *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
		return &api.CreateSandboxResponse{Accepted: true, Sandbox: &api.SandboxStatus{SandboxID: "sandbox-uid-a", Phase: phase}}, nil
	}
	err = orchestrator.ReconcileRuntime(context.Background(), assigned)
	require.ErrorIs(t, err, ErrDataPlaneInProgress)

	var current apiv1alpha1.Sandbox
	require.NoError(t, orchestrator.Client.Get(context.Background(), client.ObjectKeyFromObject(assigned), &current))
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateCreating, current.Status.DataPlaneState)

	phase = "infra-unavailable"
	err = orchestrator.ReconcileRuntime(context.Background(), &current)
	require.ErrorIs(t, err, ErrDataPlaneUnavailable)
	require.NoError(t, orchestrator.Client.Get(context.Background(), client.ObjectKeyFromObject(assigned), &current))
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateUnavailable, current.Status.DataPlaneState)

	phase = "running"
	require.NoError(t, orchestrator.ReconcileRuntime(context.Background(), &current))
	require.NoError(t, orchestrator.Client.Get(context.Background(), client.ObjectKeyFromObject(assigned), &current))
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.DataPlaneState)
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
	require.NoError(t, common.SetAssignmentAnnotation(sandbox, envelope))
	assignment := envelope.StatusAssignment()
	sandbox.Status = apiv1alpha1.SandboxStatus{
		Assignment: &assignment, AssignmentAttempt: 2, InstanceGeneration: 1, RouteGeneration: 3,
	}
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	fastletClient.create = func(string, *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
		return nil, errors.New("response lost")
	}

	err = orchestrator.ReconcileRuntime(context.Background(), sandbox)
	require.ErrorIs(t, err, ErrUnknownFastletOutcome)
	require.Zero(t, fastletClient.inspectCalls)
	current, parseErr := common.AssignmentFromAnnotation(sandbox)
	require.NoError(t, parseErr)
	require.Equal(t, envelope, *current)
}

func TestReassignDeclarativeAfterRejectionCASesDirectlyToAlternative(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	first := candidateFor(parameters)
	second := first
	second.ID, second.PodName, second.PodUID, second.PodIP, second.NodeName = "fastlet-b", "fastlet-b", "pod-b", "10.0.0.2", "node-b"
	registry.candidates = []fastletpool.FastletInfo{first, second}
	registry.fastlets[first.ID] = first
	registry.fastlets[second.ID] = second

	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(first, 3, 2, 5, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, common.SetAssignmentAnnotation(sandbox, envelope))
	assignment := envelope.StatusAssignment()
	sandbox.Status = apiv1alpha1.SandboxStatus{
		Assignment: &assignment, AssignmentAttempt: 3, InstanceGeneration: 2, RouteGeneration: 5,
	}
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	updated, moved, err := orchestrator.ReassignDeclarativeAfterRejection(context.Background(), sandbox, string(sandbox.UID))
	require.NoError(t, err)
	require.True(t, moved)
	next, err := common.AssignmentFromAnnotation(updated)
	require.NoError(t, err)
	require.Equal(t, second.PodName, next.FastletName)
	require.Equal(t, second.PodUID, next.FastletPodUID)
	require.Equal(t, int64(4), next.Attempt)
	require.Equal(t, int64(2), next.InstanceGeneration)
	require.Equal(t, int64(6), next.RouteGeneration)
	require.NotEqual(t, envelope.RuntimeInstanceID, next.RuntimeInstanceID)
	// Status remains an asynchronous projection; the annotation CAS never
	// passes through an unassigned value.
	require.Equal(t, first.PodName, updated.Status.Assignment.FastletName)
}

func TestReassignDeclarativeAfterRejectionPreservesAssignmentWithoutAlternative(t *testing.T) {
	orchestrator, registry, _, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	first := candidateFor(parameters)
	registry.candidates = []fastletpool.FastletInfo{first}
	registry.fastlets[first.ID] = first

	sandbox.UID = types.UID("sandbox-uid-a")
	envelope, err := AssignmentForCandidate(first, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, common.SetAssignmentAnnotation(sandbox, envelope))
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	updated, moved, err := orchestrator.ReassignDeclarativeAfterRejection(context.Background(), sandbox, string(sandbox.UID))
	require.NoError(t, err)
	require.False(t, moved)
	current, err := common.AssignmentFromAnnotation(updated)
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
	require.NoError(t, common.SetAssignmentAnnotation(sandbox, envelope))
	assignment := envelope.StatusAssignment()
	sandbox.Status = apiv1alpha1.SandboxStatus{
		Assignment: &assignment, AssignmentAttempt: 4, InstanceGeneration: 2, RouteGeneration: 5,
	}
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))

	cleared, err := orchestrator.ClearAssignment(context.Background(), sandbox, true)
	require.NoError(t, err)
	require.Nil(t, cleared.Status.Assignment)
	require.Equal(t, int64(3), cleared.Status.InstanceGeneration)
	require.Equal(t, int64(6), cleared.Status.RouteGeneration)
	current, err := common.AssignmentFromAnnotation(cleared)
	require.NoError(t, err)
	require.Nil(t, current)
}

func candidateFor(parameters RuntimeParameters) fastletpool.FastletInfo {
	return fastletpool.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeName: parameters.RuntimeName, RuntimeProfileHash: parameters.RuntimeProfileHash,
		ResourceProfileHash: parameters.ResourceProfileHash,
		InfraProfile:        parameters.InfraProfile, InfraProfileHash: parameters.InfraProfileHash, InfraReady: true,
	}
}

func newHarness(t *testing.T) (*Orchestrator, *fakeRegistry, *fakeFastletClient, *apiv1alpha1.Sandbox) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeContainer, Capacity: apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: 8,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("512Mi"), PIDs: 256,
			},
			FastletTemplate: corev1.PodTemplateSpec{},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha1.Sandbox{}).WithObjects(pool).Build()
	registry := &fakeRegistry{fastlets: make(map[fastletpool.FastletID]fastletpool.FastletInfo)}
	fastletClient := &fakeFastletClient{
		create: func(string, *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
			return nil, errors.New("unexpected create")
		},
		inspect: func(string, *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
			return nil, errors.New("unexpected inspect")
		},
	}
	orchestrator := &Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastletClient}
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", Annotations: map[string]string{
			common.AnnotationRequestID: "request-a", common.AnnotationCreateSpecHash: "spec-a",
		}},
		Spec: apiv1alpha1.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	return orchestrator, registry, fastletClient, sandbox
}
