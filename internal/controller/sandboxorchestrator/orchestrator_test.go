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
	reserve func(string, *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error)
	ensure  func(string, *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error)
	inspect func(string, *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error)
	deleted bool
}

func (f *fakeFastletClient) ReserveSandbox(_ context.Context, ip string, request *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error) {
	return f.reserve(ip, request)
}

func (*fakeFastletClient) CancelReservation(context.Context, string, *api.CancelReservationRequest) (*api.CancelReservationResponse, error) {
	return &api.CancelReservationResponse{Canceled: true}, nil
}

func (f *fakeFastletClient) EnsureSandbox(_ context.Context, ip string, request *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error) {
	return f.ensure(ip, request)
}

func (f *fakeFastletClient) InspectSandbox(_ context.Context, ip string, request *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
	return f.inspect(ip, request)
}

func (f *fakeFastletClient) DeleteSandboxV2(context.Context, string, *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error) {
	f.deleted = true
	return &api.DeleteSandboxV2Response{Accepted: true}, nil
}

func TestReserveForCreateTriesTopKWithoutWritingSandbox(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	registry.candidates = []fastletpool.FastletInfo{
		{ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1"},
		{ID: "fastlet-b", PodName: "fastlet-b", PodUID: "pod-b", PodIP: "10.0.0.2"},
	}
	fastletClient.reserve = func(ip string, request *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error) {
		if ip == "10.0.0.1" {
			failure := &api.FastletError{Code: api.ErrorCapacityRejected, Message: "full", Retryable: true}
			return &api.ReserveSandboxResponse{Error: failure}, failure
		}
		return &api.ReserveSandboxResponse{ReservationToken: "token-b", FastletPodUID: request.FastletPodUID}, nil
	}

	reservation, err := orchestrator.ReserveForCreate(context.Background(), sandbox, "request-a", "spec-a")
	require.NoError(t, err)
	require.Equal(t, "token-b", reservation.Token)
	require.Equal(t, fastletpool.FastletID("fastlet-b"), reservation.Fastlet.ID)
	require.Equal(t, []fastletpool.FastletID{"fastlet-a"}, registry.feedback)

	var list apiv1alpha1.SandboxList
	require.NoError(t, orchestrator.Client.List(context.Background(), &list))
	require.Empty(t, list.Items, "reservation gate must not create a CRD")
}

func TestEnsureAssignmentAndRuntimeUseDurableUIDAndFences(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	parameters, err := orchestrator.ResolveRuntime(context.Background(), sandbox)
	require.NoError(t, err)
	candidate := fastletpool.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeProfileHash: parameters.RuntimeProfileHash, ResourceProfileHash: parameters.ResourceProfileHash,
	}
	registry.fastlets[candidate.ID] = candidate

	persisted := sandbox.DeepCopy()
	persisted.UID = "sandbox-uid-a"
	require.NoError(t, orchestrator.Client.Create(context.Background(), persisted))
	persisted, won, err := orchestrator.EnsureAssignment(context.Background(), persisted, candidate)
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, int64(1), persisted.Status.Assignment.Attempt)

	fastletClient.ensure = func(ip string, request *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error) {
		require.Equal(t, candidate.PodIP, ip)
		require.Equal(t, "sandbox-uid-a", request.Identity.SandboxUID)
		require.Equal(t, int64(1), request.Identity.InstanceGeneration)
		require.Equal(t, int64(1), request.Identity.AssignmentAttempt)
		require.Equal(t, "pod-a", request.Identity.FastletPodUID)
		require.Equal(t, "sandbox-uid-a", request.Sandbox.ClaimUID)
		return &api.EnsureSandboxResponse{Accepted: true, Sandbox: &api.SandboxStatus{SandboxID: "sandbox-uid-a", Phase: "running"}}, nil
	}
	require.NoError(t, orchestrator.EnsureRuntime(context.Background(), persisted, nil))

	var ready apiv1alpha1.Sandbox
	require.NoError(t, orchestrator.Client.Get(context.Background(), client.ObjectKeyFromObject(persisted), &ready))
	require.Equal(t, apiv1alpha1.ObservedStateReady, ready.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateReady, ready.Status.DataPlaneState)
}

func TestLostEnsureResponseInspectsSameAssignment(t *testing.T) {
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	sandbox.UID = "sandbox-uid-a"
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 2}
	sandbox.Status = apiv1alpha1.SandboxStatus{Assignment: &assignment, AssignmentAttempt: 2, InstanceGeneration: 1}
	require.NoError(t, orchestrator.Client.Create(context.Background(), sandbox))
	registry.fastlets["fastlet-a"] = fastletpool.FastletInfo{ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1"}
	fastletClient.ensure = func(string, *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error) {
		return nil, errors.New("response lost")
	}
	fastletClient.inspect = func(_ string, request *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
		require.Equal(t, int64(2), request.Identity.AssignmentAttempt)
		return &api.InspectSandboxResponse{Sandbox: &api.SandboxStatus{SandboxID: "sandbox-uid-a", Phase: "running"}}, nil
	}

	require.NoError(t, orchestrator.EnsureRuntime(context.Background(), sandbox, nil))
	require.Empty(t, registry.feedback)
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
		reserve: func(string, *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error) {
			return nil, errors.New("unexpected reserve")
		},
		ensure: func(string, *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error) {
			return nil, errors.New("unexpected ensure")
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
