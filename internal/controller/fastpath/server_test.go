package fastpath

import (
	"context"
	"errors"
	"sync"
	"testing"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/controller/sandboxorchestrator"

	"github.com/stretchr/testify/require"
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
	mu             sync.Mutex
	createFailure  error
	createFailures map[string]error
	createRequests []*api.CreateSandboxRequest
	createIPs      []string
	createPhase    string
	diagnostics    *api.SandboxDiagnosticsResponse
	diagnosticsErr error
}

func (f *fastpathFastlet) CreateSandbox(_ context.Context, fastletIP string, request *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createRequests = append(f.createRequests, request)
	f.createIPs = append(f.createIPs, fastletIP)
	if f.createFailure != nil {
		return &api.CreateSandboxResponse{}, f.createFailure
	}
	if failure := f.createFailures[fastletIP]; failure != nil {
		return &api.CreateSandboxResponse{}, failure
	}
	phase := f.createPhase
	if phase == "" {
		phase = "running"
	}
	return &api.CreateSandboxResponse{
		Accepted: true, Created: true,
		Sandbox: &api.SandboxStatus{SandboxID: request.Identity.SandboxUID, RuntimeInstanceID: request.Identity.RuntimeInstanceID, Phase: phase},
	}, nil
}

func (*fastpathFastlet) InspectSandbox(_ context.Context, _ string, request *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
	return &api.InspectSandboxResponse{Sandbox: &api.SandboxStatus{SandboxID: request.Identity.SandboxUID, Phase: "running"}}, nil
}

func (*fastpathFastlet) DeleteSandboxV2(context.Context, string, *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error) {
	return &api.DeleteSandboxV2Response{Accepted: true}, nil
}

func (f *fastpathFastlet) SandboxDiagnostics(context.Context, string, *api.SandboxDiagnosticsRequest) (*api.SandboxDiagnosticsResponse, error) {
	if f.diagnostics != nil || f.diagnosticsErr != nil {
		return f.diagnostics, f.diagnosticsErr
	}
	return &api.SandboxDiagnosticsResponse{}, nil
}

type countingUIDClient struct {
	client.Client
	mu      sync.Mutex
	creates int
	gets    int
	lists   int
	patches int
}

func (c *countingUIDClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.mu.Lock()
	c.creates++
	if sandbox, ok := object.(*apiv1alpha1.Sandbox); ok && sandbox.UID == "" {
		sandbox.UID = types.UID("uid-" + sandbox.Name)
	}
	c.mu.Unlock()
	return c.Client.Create(ctx, object, options...)
}

func (c *countingUIDClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.Client.Get(ctx, key, object, options...)
}

func (c *countingUIDClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.Client.List(ctx, list, options...)
}

func (c *countingUIDClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	c.mu.Lock()
	c.patches++
	c.mu.Unlock()
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *countingUIDClient) counts() (int, int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates, c.gets, c.lists, c.patches
}

func TestCreateHappyPathUsesExactlyTwoDownstreamIO(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	response, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	require.Equal(t, "request-a", response.SandboxName)
	require.Equal(t, "fastlet-a", response.FastletPod)

	creates, gets, lists, patches := k8sClient.counts()
	require.Equal(t, 1, creates)
	require.Zero(t, gets)
	require.Zero(t, lists)
	require.Zero(t, patches)
	fastlet.mu.Lock()
	require.Len(t, fastlet.createRequests, 1)
	require.Equal(t, response.SandboxUid, fastlet.createRequests[0].Identity.SandboxUID)
	require.NotEmpty(t, fastlet.createRequests[0].Identity.RuntimeInstanceID)
	fastlet.mu.Unlock()

	var persisted apiv1alpha1.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	require.Nil(t, persisted.Status.Assignment, "Controller projection is asynchronous")
	envelope, err := common.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, "fastlet-a", envelope.FastletName)
}

func TestCreateReturnsWhenRuntimeIsReadyAndDataPlaneIsPending(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	fastlet.createPhase = "infra-pending"

	response, err := server.CreateSandbox(context.Background(), createRequest("request-runtime-ready"))
	require.NoError(t, err)
	require.Equal(t, "request-runtime-ready", response.SandboxName)

	creates, gets, lists, patches := k8sClient.counts()
	require.Equal(t, 1, creates)
	require.Zero(t, gets)
	require.Zero(t, lists)
	require.Zero(t, patches)
	require.Len(t, fastlet.createRequests, 1)
}

func TestNoCandidateFailsBeforeCRDCreate(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	registry.candidates = nil
	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	creates, _, _, _ := k8sClient.counts()
	require.Zero(t, creates)
	fastlet.mu.Lock()
	require.Empty(t, fastlet.createRequests)
	fastlet.mu.Unlock()
}

func TestFastletRejectionKeepsPersistedIntent(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	fastlet.createFailure = &api.FastletError{
		Code: api.ErrorCapacityRejected, Message: "full", Retryable: true, Outcome: api.OutcomeRejectedBeforeSideEffects,
	}
	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	var persisted apiv1alpha1.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	require.NotEmpty(t, persisted.Annotations[common.AnnotationAssignment])
}

func TestCreateRetryUsesSameCRDAndRuntimeIdentity(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	first, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	second, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	require.Equal(t, first.SandboxUid, second.SandboxUid)
	require.Equal(t, first.SandboxName, second.SandboxName)
	fastlet.mu.Lock()
	require.Len(t, fastlet.createRequests, 2)
	require.Equal(t, fastlet.createRequests[0].Identity.RuntimeInstanceID, fastlet.createRequests[1].Identity.RuntimeInstanceID)
	fastlet.mu.Unlock()

	conflict := createRequest("request-a")
	conflict.Image = "ubuntu:24.04"
	_, err = server.CreateSandbox(context.Background(), conflict)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	creates, gets, lists, _ := k8sClient.counts()
	require.Equal(t, 3, creates)
	require.Equal(t, 2, gets)
	require.Zero(t, lists)
}

func TestExplicitRejectionCASesToSecondCandidate(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	second := testCandidate("fastlet-b", "pod-b", "10.0.0.2")
	registry.candidates = append(registry.candidates, second)
	registry.fastlets[second.ID] = second
	fastlet.createFailures = map[string]error{
		"10.0.0.1": &api.FastletError{Code: api.ErrorCapacityRejected, Message: "full", Retryable: true, Outcome: api.OutcomeRejectedBeforeSideEffects},
	}

	response, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	require.Equal(t, "fastlet-b", response.FastletPod)
	var persisted apiv1alpha1.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	envelope, err := common.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, "fastlet-b", envelope.FastletName)
	require.Equal(t, int64(2), envelope.Attempt)
	require.Equal(t, int64(2), envelope.RouteGeneration)
	fastlet.mu.Lock()
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, fastlet.createIPs)
	fastlet.mu.Unlock()
}

func TestAmbiguousFailureNeverReassigns(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	second := testCandidate("fastlet-b", "pod-b", "10.0.0.2")
	registry.candidates = append(registry.candidates, second)
	registry.fastlets[second.ID] = second
	fastlet.createFailure = errors.New("transport response lost")

	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Equal(t, codes.Unavailable, status.Code(err))
	var persisted apiv1alpha1.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	envelope, parseErr := common.AssignmentFromAnnotation(&persisted)
	require.NoError(t, parseErr)
	require.Equal(t, "fastlet-a", envelope.FastletName)
	fastlet.mu.Lock()
	require.Len(t, fastlet.createRequests, 1)
	fastlet.mu.Unlock()
}

func TestConcurrentSameRequestConvergesToOneCRDAndIdentity(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	const workers = 20
	responses := make(chan *fastpathv1.CreateResponse, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
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
	require.NoError(t, k8sClient.Client.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	fastlet.mu.Lock()
	var runtimeID string
	for _, request := range fastlet.createRequests {
		if runtimeID == "" {
			runtimeID = request.Identity.RuntimeInstanceID
		}
		require.Equal(t, runtimeID, request.Identity.RuntimeInstanceID)
	}
	fastlet.mu.Unlock()
}

func TestCreateRejectsSplitNameAndRequestID(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	request := createRequest("request-a")
	request.Name = "different"
	_, err := server.CreateSandbox(context.Background(), request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	creates, _, _, _ := k8sClient.counts()
	require.Zero(t, creates)
}

func TestDeleteAndUpdateOnlyCommitDesiredState(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", Finalizers: []string{"sandbox.fast.io/cleanup"}},
		Spec:       apiv1alpha1.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	require.NoError(t, k8sClient.Client.Create(context.Background(), sandbox))
	update, err := server.UpdateSandbox(context.Background(), &fastpathv1.UpdateRequest{
		SandboxName: "sandbox-a", Namespace: "default",
		Update: &fastpathv1.UpdateRequest_ExpireTimeSeconds{ExpireTimeSeconds: 1234},
	})
	require.NoError(t, err)
	require.True(t, update.Success)
	deleted, err := server.DeleteSandbox(context.Background(), &fastpathv1.DeleteRequest{SandboxName: "sandbox-a", Namespace: "default"})
	require.NoError(t, err)
	require.True(t, deleted.Success)
	var terminating apiv1alpha1.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), client.ObjectKeyFromObject(sandbox), &terminating))
	require.NotNil(t, terminating.DeletionTimestamp)
}

func newV2Server(t *testing.T) (*Server, *countingUIDClient, *fastpathRegistry, *fastpathFastlet) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha1.Sandbox{}).Build()
	k8sClient := &countingUIDClient{Client: baseClient}
	candidate := testCandidate("fastlet-a", "pod-a", "10.0.0.1")
	registry := &fastpathRegistry{
		candidates: []fastletpool.FastletInfo{candidate},
		fastlets:   map[fastletpool.FastletID]fastletpool.FastletInfo{candidate.ID: candidate},
	}
	fastlet := &fastpathFastlet{}
	orchestrator := &sandboxorchestrator.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	return &Server{K8sClient: k8sClient, Orchestrator: orchestrator, DiagnosticsClient: fastlet}, k8sClient, registry, fastlet
}

func TestGetSandboxDiagnosticsUsesAnnotationAndDegradesWhenFastletFails(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	fastlet.diagnostics = &api.SandboxDiagnosticsResponse{Events: []api.SandboxDiagnosticEvent{{
		Timestamp: metav1.Now().Time, Level: "info", Source: "runtime", Phase: "running", Message: "ready",
	}}}

	diagnostics, err := server.GetSandboxDiagnostics(context.Background(), &fastpathv1.SandboxDiagnosticsRequest{SandboxName: created.SandboxName, Namespace: "default"})
	require.NoError(t, err)
	require.True(t, diagnostics.FastletReachable)
	require.Equal(t, "status-projection-pending", diagnostics.AssignmentState)
	require.Equal(t, "ready", diagnostics.Events[0].Message)
	require.NotEmpty(t, diagnostics.RuntimeInstanceId)

	fastlet.diagnostics = nil
	fastlet.diagnosticsErr = errors.New("connection refused")
	diagnostics, err = server.GetSandboxDiagnostics(context.Background(), &fastpathv1.SandboxDiagnosticsRequest{SandboxName: created.SandboxName, Namespace: "default"})
	require.NoError(t, err)
	require.False(t, diagnostics.FastletReachable)
	require.Contains(t, diagnostics.FastletError, "connection refused")
}

func testCandidate(name, uid, ip string) fastletpool.FastletInfo {
	return fastletpool.FastletInfo{
		ID: fastletpool.FastletID(name), PodName: name, PodUID: uid, PodIP: ip, NodeName: "node-a",
		RuntimeName: apiv1alpha1.RuntimeContainer, RuntimeProfileHash: "runtime-hash",
		ResourceProfileHash: "resource-hash", InfraProfile: "minimal", InfraProfileHash: "infra-hash", InfraReady: true,
	}
}

func createRequest(requestID string) *fastpathv1.CreateRequest {
	return &fastpathv1.CreateRequest{
		Image: "alpine:latest", PoolRef: "pool-a", Namespace: "default", RequestId: requestID,
		Envs: map[string]string{"A": "B"}, WorkingDir: "/workspace",
	}
}

func TestSandboxFromCreateRequestUsesCanonicalFields(t *testing.T) {
	request := createRequest("request-a")
	sandbox := sandboxFromCreateRequest(request, "create-hash")
	require.Equal(t, request.RequestId, sandbox.Name)
	require.Equal(t, request.Image, sandbox.Spec.Image)
	require.Equal(t, request.PoolRef, sandbox.Spec.PoolRef)
	require.Equal(t, []metav1.Condition(nil), sandbox.Status.Conditions)
}
