package fastpath

import (
	"context"
	"sync"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/controller/sandboxorchestrator"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fastpathRegistry struct {
	mu         sync.Mutex
	candidates []fastletpool.FastletInfo
	fastlets   map[fastletpool.FastletID]fastletpool.FastletInfo
	feedback   []fastletpool.FastletID
}

func (r *fastpathRegistry) TopK(fastletpool.CandidateRequest, int) []fastletpool.FastletInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fastletpool.FastletInfo(nil), r.candidates...)
}

func (r *fastpathRegistry) GetFastletByID(id fastletpool.FastletID) (fastletpool.FastletInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.fastlets[id]
	return value, ok
}

func (r *fastpathRegistry) RecordFeedback(id fastletpool.FastletID, _ fastletpool.LocalFeedback) {
	r.mu.Lock()
	r.feedback = append(r.feedback, id)
	r.mu.Unlock()
}

type fastpathFastlet struct {
	mu              sync.Mutex
	rejectReserve   bool
	ensureFailure   error
	ensureRequests  []*api.EnsureSandboxRequest
	reserveRequests []*api.ReserveSandboxRequest
}

func (f *fastpathFastlet) ReserveSandbox(_ context.Context, _ string, request *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveRequests = append(f.reserveRequests, request)
	if f.rejectReserve {
		failure := &api.FastletError{Code: api.ErrorCapacityRejected, Message: "full", Retryable: true}
		return &api.ReserveSandboxResponse{Error: failure}, failure
	}
	return &api.ReserveSandboxResponse{ReservationToken: "reservation-" + request.RequestID, FastletPodUID: request.FastletPodUID, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (*fastpathFastlet) CancelReservation(context.Context, string, *api.CancelReservationRequest) (*api.CancelReservationResponse, error) {
	return &api.CancelReservationResponse{Canceled: true}, nil
}

func (f *fastpathFastlet) EnsureSandbox(_ context.Context, _ string, request *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureRequests = append(f.ensureRequests, request)
	if f.ensureFailure != nil {
		return &api.EnsureSandboxResponse{}, f.ensureFailure
	}
	return &api.EnsureSandboxResponse{Accepted: true, Created: true, Sandbox: &api.SandboxStatus{SandboxID: request.Identity.SandboxUID, Phase: "running"}}, nil
}

func (*fastpathFastlet) InspectSandbox(_ context.Context, _ string, request *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
	return &api.InspectSandboxResponse{Sandbox: &api.SandboxStatus{SandboxID: request.Identity.SandboxUID, Phase: "running"}}, nil
}

func (*fastpathFastlet) DeleteSandboxV2(context.Context, string, *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error) {
	return &api.DeleteSandboxV2Response{Accepted: true}, nil
}

type assigningUIDClient struct {
	client.Client
	mu sync.Mutex
}

func (c *assigningUIDClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sandbox, ok := object.(*apiv1alpha1.Sandbox); ok && sandbox.UID == "" {
		sandbox.UID = types.UID("uid-" + sandbox.Name)
	}
	return c.Client.Create(ctx, object, options...)
}

func TestCreateFastFailureDoesNotPersistCRD(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	fastlet.rejectReserve = true
	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	var list apiv1alpha1.SandboxList
	require.NoError(t, k8sClient.List(context.Background(), &list))
	require.Empty(t, list.Items)
}

func TestCreateIsIdempotentAndDeprecatedConsistencyDoesNotChangeOrdering(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	request := createRequest("request-a")
	request.ConsistencyMode = fastpathv1.ConsistencyMode_STRONG
	first, err := server.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.NotEmpty(t, first.SandboxUid)

	retryRequest := createRequest("request-a")
	retryRequest.ConsistencyMode = fastpathv1.ConsistencyMode_FAST
	second, err := server.CreateSandbox(context.Background(), retryRequest)
	require.NoError(t, err)
	require.Equal(t, first.SandboxUid, second.SandboxUid)
	require.Equal(t, first.SandboxName, second.SandboxName)

	var list apiv1alpha1.SandboxList
	require.NoError(t, k8sClient.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, int64(1), list.Items[0].Status.AssignmentAttempt)
	require.Equal(t, apiv1alpha1.ObservedStateReady, list.Items[0].Status.RuntimeState)
	require.Equal(t, requestIDLabelValue("request-a"), list.Items[0].Labels[common.LabelRequestIDHash])
	fastlet.mu.Lock()
	require.Len(t, fastlet.ensureRequests, 1)
	require.Equal(t, first.SandboxUid, fastlet.ensureRequests[0].Identity.SandboxUID)
	fastlet.mu.Unlock()

	conflict := createRequest("request-a")
	conflict.Image = "ubuntu:24.04"
	_, err = server.CreateSandbox(context.Background(), conflict)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestCreateCommitSurvivesFastPathFailureAndRetryContinuesSameCRD(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	failure := &api.FastletError{Code: api.ErrorRuntimeUnavailable, Message: "temporary", Retryable: true}
	fastlet.ensureFailure = failure
	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Error(t, err)

	var list apiv1alpha1.SandboxList
	require.NoError(t, k8sClient.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	committedUID := string(list.Items[0].UID)
	require.NotNil(t, list.Items[0].Status.Assignment)

	fastlet.mu.Lock()
	fastlet.ensureFailure = nil
	fastlet.mu.Unlock()
	response, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	require.Equal(t, committedUID, response.SandboxUid)

	require.NoError(t, k8sClient.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
}

func TestConcurrentSameRequestConvergesToOneCRDAndUID(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	const workers = 20
	responses := make(chan *fastpathv1.CreateResponse, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
			if err != nil {
				errorsFound <- err
				return
			}
			responses <- response
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}
	close(responses)
	var uid string
	for response := range responses {
		if uid == "" {
			uid = response.SandboxUid
		}
		require.Equal(t, uid, response.SandboxUid)
	}
	var list apiv1alpha1.SandboxList
	require.NoError(t, k8sClient.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
}

func TestDeleteAndUpdateOnlyCommitDesiredState(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", Finalizers: []string{"sandbox.fast.io/cleanup"}},
		Spec:       apiv1alpha1.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	require.NoError(t, k8sClient.Create(context.Background(), sandbox))
	update, err := server.UpdateSandbox(context.Background(), &fastpathv1.UpdateRequest{
		SandboxName: "sandbox-a", Namespace: "default",
		Update: &fastpathv1.UpdateRequest_ExpireTimeSeconds{ExpireTimeSeconds: 1234},
	})
	require.NoError(t, err)
	require.True(t, update.Success)
	require.Equal(t, "desired state committed", update.Message)

	deleted, err := server.DeleteSandbox(context.Background(), &fastpathv1.DeleteRequest{SandboxName: "sandbox-a", Namespace: "default"})
	require.NoError(t, err)
	require.True(t, deleted.Success)
	var terminating apiv1alpha1.Sandbox
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(sandbox), &terminating))
	require.NotNil(t, terminating.DeletionTimestamp)
}

func newV2Server(t *testing.T) (*Server, client.Client, *fastpathRegistry, *fastpathFastlet) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime:         apiv1alpha1.RuntimeContainer,
			Capacity:        apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1},
			FastletTemplate: corev1.PodTemplateSpec{},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha1.Sandbox{}).
		WithObjects(pool).Build()
	k8sClient := &assigningUIDClient{Client: baseClient}
	candidate := fastletpool.FastletInfo{ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a"}
	registry := &fastpathRegistry{
		candidates: []fastletpool.FastletInfo{candidate},
		fastlets:   map[fastletpool.FastletID]fastletpool.FastletInfo{"fastlet-a": candidate},
	}
	fastlet := &fastpathFastlet{}
	orchestrator := &sandboxorchestrator.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	return &Server{K8sClient: k8sClient, Orchestrator: orchestrator}, k8sClient, registry, fastlet
}

func createRequest(requestID string) *fastpathv1.CreateRequest {
	return &fastpathv1.CreateRequest{
		Image: "alpine:latest", PoolRef: "pool-a", Namespace: "default", RequestId: requestID,
		Envs: map[string]string{"A": "B"}, WorkingDir: "/workspace",
	}
}
