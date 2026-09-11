package fastpath

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fastpathRegistry struct {
	mu         sync.Mutex
	candidates []placement.FastletInfo
	fastlets   map[placement.FastletID]placement.FastletInfo
	feedback   []placement.FastletID
}

func (r *fastpathRegistry) TopK(placement.CandidateRequest, int) []placement.FastletInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]placement.FastletInfo(nil), r.candidates...)
}

func (r *fastpathRegistry) GetFastletByID(id placement.FastletID) (placement.FastletInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.fastlets[id]
	return value, ok
}

func (r *fastpathRegistry) RecordFeedback(id placement.FastletID, _ placement.LocalFeedback) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.feedback = append(r.feedback, id)
}

type fastpathFastlet struct {
	mu             sync.Mutex
	createFailure  error
	createFailures map[string]error
	createRequests []*fastletapi.CreateSandboxRequest
	createIPs      []string
	createPhase    string
	nilCreateReply bool
	inspectStatus  *fastletapi.SandboxStatus
	inspectError   error
	diagnostics    *fastletapi.SandboxDiagnosticsResponse
	diagnosticsErr error
	snapshotErr    error
}

func (f *fastpathFastlet) CreateSandbox(_ context.Context, fastletIP string, request *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createRequests = append(f.createRequests, request)
	f.createIPs = append(f.createIPs, fastletIP)
	if f.createFailure != nil {
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionFromError(f.createFailure)}, f.createFailure
	}
	if failure := f.createFailures[fastletIP]; failure != nil {
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionFromError(failure)}, failure
	}
	if f.nilCreateReply {
		return nil, nil
	}
	phase := f.createPhase
	if phase == "" {
		phase = "running"
	}
	bindings := make([]fastletapi.ActionBindingStatus, 0, len(request.ActionBindings))
	for _, binding := range request.ActionBindings {
		bindings = append(bindings, fastletapi.ActionBindingStatus{Handler: binding.Handler, State: "Ready"})
	}
	observed := testObservedStatus(request.Identity.SandboxUID, phase, request.SpecGeneration, bindings)
	observed.RuntimeInstanceID = request.Identity.RuntimeInstanceID
	return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionCreated, Sandbox: observed}, nil
}

func (f *fastpathFastlet) InspectSandbox(_ context.Context, _ string, request *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectError != nil {
		return nil, f.inspectError
	}
	if f.inspectStatus != nil {
		copy := *f.inspectStatus
		return &fastletapi.InspectSandboxResponse{Sandbox: &copy}, nil
	}
	return &fastletapi.InspectSandboxResponse{Sandbox: testObservedStatus(request.Identity.SandboxUID, "running", 1, nil)}, nil
}

func (*fastpathFastlet) DeleteSandbox(context.Context, string, *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	return &fastletapi.DeleteSandboxResponse{}, nil
}

func (f *fastpathFastlet) CreateSnapshot(_ context.Context, _ string, request *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
	f.mu.Lock()
	snapshotErr := f.snapshotErr
	f.mu.Unlock()
	if snapshotErr != nil {
		return &fastletapi.CreateSnapshotResponse{Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects}, snapshotErr
	}
	return &fastletapi.CreateSnapshotResponse{
		Disposition: fastletapi.CreateDispositionCreated,
		Snapshot: &fastletapi.SnapshotStatus{
			SnapshotID: "snap-" + request.Identity.SnapshotUID, Phase: fastletapi.SnapshotPhaseCreating,
		},
	}, nil
}

func (*fastpathFastlet) InspectSnapshot(_ context.Context, _ string, request *fastletapi.InspectSnapshotRequest) (*fastletapi.InspectSnapshotResponse, error) {
	return &fastletapi.InspectSnapshotResponse{Snapshot: &fastletapi.SnapshotStatus{
		SnapshotID: "snap-" + request.Identity.SnapshotUID, Phase: fastletapi.SnapshotPhaseCreating,
	}}, nil
}

func (*fastpathFastlet) DeleteSnapshot(context.Context, string, *fastletapi.DeleteSnapshotRequest) (*fastletapi.DeleteSnapshotResponse, error) {
	return &fastletapi.DeleteSnapshotResponse{}, nil
}

func (f *fastpathFastlet) ReconcileBindings(_ context.Context, _ string, request *fastletapi.ReconcileBindingsRequest) (*fastletapi.ReconcileBindingsResponse, error) {
	bindings := make([]fastletapi.ActionBindingStatus, 0, len(request.ActionBindings))
	for _, binding := range request.ActionBindings {
		bindings = append(bindings, fastletapi.ActionBindingStatus{Handler: binding.Handler, State: "Ready"})
	}
	return &fastletapi.ReconcileBindingsResponse{Sandbox: testObservedStatus(request.Identity.SandboxUID, "running", request.SpecGeneration, bindings)}, nil
}

func testObservedStatus(uid, phase string, generation int64, bindings []fastletapi.ActionBindingStatus) *fastletapi.SandboxStatus {
	runtime := fastletapi.RuntimeStateReady
	dataPlane := fastletapi.DataPlaneStateReady
	switch phase {
	case "creating":
		runtime, dataPlane = fastletapi.RuntimeStateCreating, fastletapi.DataPlaneStatePending
	case "infra-pending":
		dataPlane = fastletapi.DataPlaneStatePublishing
	case "infra-unavailable":
		dataPlane = fastletapi.DataPlaneStateUnavailable
	}
	return &fastletapi.SandboxStatus{
		SandboxID: uid, AcceptedGeneration: generation, AppliedGeneration: generation,
		Runtime: fastletapi.RuntimeObservation{State: runtime}, DataPlane: fastletapi.DataPlaneObservation{State: dataPlane},
		ActionBindings: bindings,
	}
}

func (f *fastpathFastlet) SandboxDiagnostics(context.Context, string, *fastletapi.SandboxDiagnosticsRequest) (*fastletapi.SandboxDiagnosticsResponse, error) {
	if f.diagnostics != nil || f.diagnosticsErr != nil {
		return f.diagnostics, f.diagnosticsErr
	}
	return &fastletapi.SandboxDiagnosticsResponse{}, nil
}

type countingUIDClient struct {
	client.Client
	mu         sync.Mutex
	creates    int
	gets       int
	lists      int
	patches    int
	patchError error
}

func (c *countingUIDClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.mu.Lock()
	c.creates++
	if sandbox, ok := object.(*apiv1alpha2.Sandbox); ok {
		if sandbox.UID == "" {
			sandbox.UID = types.UID("uid-" + sandbox.Name)
		}
		if sandbox.Generation == 0 {
			sandbox.Generation = 1
		}
	}
	if snapshot, ok := object.(*apiv1alpha2.SandboxSnapshot); ok && snapshot.UID == "" {
		snapshot.UID = types.UID("snapshot-uid-" + snapshot.Name)
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
	patchError := c.patchError
	c.mu.Unlock()
	if patchError != nil {
		return patchError
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *countingUIDClient) counts() (int, int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates, c.gets, c.lists, c.patches
}

func TestCreateHappyPathUsesOneRemoteWriteAndOneFastletCall(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	response, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	require.Equal(t, "request-a", response.Sandbox.GetIdentity().GetName())
	require.True(t, response.Sandbox.Ready)
	require.Equal(t, fastpathv2.CreateCompletion_CREATE_COMPLETION_READY, response.Completion)

	creates, gets, lists, patches := k8sClient.counts()
	require.Equal(t, 1, creates)
	require.Zero(t, gets, "Pool lookup is served by the local cache")
	require.Zero(t, lists)
	require.Zero(t, patches)
	fastlet.mu.Lock()
	require.Len(t, fastlet.createRequests, 1)
	require.Equal(t, response.Sandbox.GetIdentity().GetUid(), fastlet.createRequests[0].Identity.SandboxUID)
	require.Equal(t, fastletapi.CreateCompletionReady, fastlet.createRequests[0].Completion)
	fastlet.mu.Unlock()

	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	require.Empty(t, persisted.Status.Placement.FastletName, "Controller projection is asynchronous")
	envelope, err := assignment.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, "fastlet-a", envelope.FastletName)
}

func TestCreateSandboxAppliesSnapshotRecordedBindings(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	// A Succeeded snapshot whose templateName matches the create image,
	// carrying the source sandbox's policy provenance.
	snapshot := &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snap-uid-a"),
			CreationTimestamp: metav1.Now()},
		Spec: apiv1alpha2.SandboxSnapshotSpec{
			SandboxRef:   apiv1alpha2.SandboxRef{Name: "sandbox-old", Namespace: "default"},
			TemplateName: "app-snap-v1",
		},
		Status: apiv1alpha2.SandboxSnapshotStatus{Phase: apiv1alpha2.SandboxSnapshotPhaseSucceeded},
	}
	provenance, err := json.Marshal([]apiv1alpha2.ActionBinding{
		{Handler: "audit", Input: `{"a":1}`},
		{Handler: "missing-in-pool", Input: `{}`},
	})
	require.NoError(t, err)
	snapshot.Annotations = map[string]string{assignment.AnnotationSourceActionBindings: string(provenance)}
	require.NoError(t, k8sClient.Client.Create(context.Background(), snapshot))

	request := createRequest("restored-1")
	request.Image = "app-snap-v1"
	request.ActionBindings = []*fastpathv2.ActionBinding{{Handler: "audit", Input: `{"explicit":true}`}}
	response, err := server.CreateSandbox(context.Background(), request)
	require.NoError(t, err)

	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "restored-1"}, &persisted))
	require.Equal(t, response.Sandbox.GetIdentity().GetUid(), string(persisted.UID))
	// Explicit wins per handler; undeclared handlers are dropped.
	require.Equal(t, []apiv1alpha2.ActionBinding{{Handler: "audit", Input: `{"explicit":true}`}}, persisted.Spec.ActionBindings)

	// Without explicit bindings the recorded policy applies verbatim.
	request2 := createRequest("restored-2")
	request2.Image = "app-snap-v1"
	_, err = server.CreateSandbox(context.Background(), request2)
	require.NoError(t, err)
	var second apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "restored-2"}, &second))
	require.Equal(t, []apiv1alpha2.ActionBinding{{Handler: "audit", Input: `{"a":1}`}}, second.Spec.ActionBindings)

	// An image with no snapshot provenance is untouched.
	request3 := createRequest("plain-1")
	_, err = server.CreateSandbox(context.Background(), request3)
	require.NoError(t, err)
	var plain apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "plain-1"}, &plain))
	require.Empty(t, plain.Spec.ActionBindings)
}

func TestGetAndListCacheHitsAvoidDurableKubernetesReads(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)

	_, err = server.GetSandbox(context.Background(), &fastpathv2.GetSandboxRequest{
		Sandbox: expectedReference("sandbox-a", "default"), ExpectedGeneration: created.Generation,
	})
	require.NoError(t, err)
	_, err = server.ListSandboxes(context.Background(), &fastpathv2.ListSandboxesRequest{Namespace: "default"})
	require.NoError(t, err)

	_, gets, lists, _ := k8sClient.counts()
	require.Zero(t, gets)
	require.Zero(t, lists)
}

func TestNameCacheMissFallsBackToOneDurableGet(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	_, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	emptyCache := fake.NewClientBuilder().WithScheme(k8sClient.Client.Scheme()).Build()
	server.RouteCache = emptyCache

	_, err = server.GetSandbox(context.Background(), &fastpathv2.GetSandboxRequest{
		Sandbox: expectedReference("sandbox-a", "default"),
	})
	require.NoError(t, err)
	_, gets, lists, _ := k8sClient.counts()
	require.Equal(t, 1, gets)
	require.Zero(t, lists)
}

func TestCreateCompletionRuntimeReadyIsForwarded(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	request := createRequest("runtime-ready")
	request.Completion = fastpathv2.CreateCompletion_CREATE_COMPLETION_RUNTIME_READY
	fastlet.createPhase = "infra-pending"
	response, err := server.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	require.False(t, response.Sandbox.Ready)
	require.Equal(t, fastpathv2.CreateCompletion_CREATE_COMPLETION_RUNTIME_READY, response.Completion)
	fastlet.mu.Lock()
	require.Equal(t, fastletapi.CreateCompletionRuntimeReady, fastlet.createRequests[0].Completion)
	fastlet.mu.Unlock()
}

func TestCreateCompletionRejectsUnknownEnum(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	request := createRequest("invalid-completion")
	request.Completion = fastpathv2.CreateCompletion(99)
	_, err := server.CreateSandbox(context.Background(), request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Empty(t, fastlet.createRequests)
}

func TestCreateNilFastletReplyIsProtocolFailureWithoutNilFormatting(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	fastlet.nilCreateReply = true
	_, err := server.CreateSandbox(context.Background(), createRequest("nil-reply"))
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.NotContains(t, err.Error(), "<nil>")
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "nil-reply"}, &persisted))
}

func TestCreateRetryUsesSameCRDAndRuntimeIdentity(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	first, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	second, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	require.Equal(t, first.Sandbox.GetIdentity().GetUid(), second.Sandbox.GetIdentity().GetUid())
	fastlet.mu.Lock()
	require.Equal(t, fastlet.createRequests[0].Identity.RuntimeInstanceID, fastlet.createRequests[1].Identity.RuntimeInstanceID)
	fastlet.mu.Unlock()

	conflict := createRequest("request-a")
	conflict.Image = "ubuntu:24.04"
	_, err = server.CreateSandbox(context.Background(), conflict)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestNoCandidateAndExplicitRejection(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	registry.candidates = nil
	_, err := server.CreateSandbox(context.Background(), createRequest("no-candidate"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	creates, _, _, _ := k8sClient.counts()
	require.Zero(t, creates)

	registry.candidates = []placement.FastletInfo{testCandidate("fastlet-a", "pod-a", "10.0.0.1")}
	fastlet.createFailure = &fastletapi.CreateCallError{Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects, Failure: &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "full", Retryable: true}}
	_, err = server.CreateSandbox(context.Background(), createRequest("rejected"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	var persisted apiv1alpha2.Sandbox
	require.Error(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "rejected"}, &persisted))
}

func TestExplicitRejectionCASesToSecondCandidate(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	second := testCandidate("fastlet-b", "pod-b", "10.0.0.2")
	registry.candidates = append(registry.candidates, second)
	registry.fastlets[second.ID] = second
	fastlet.createFailures = map[string]error{"10.0.0.1": &fastletapi.CreateCallError{
		Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects,
		Failure:     &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "full", Retryable: true},
	}}

	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.NoError(t, err)
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	envelope, err := assignment.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, "fastlet-b", envelope.FastletName)
	require.Equal(t, int64(2), envelope.Attempt)
	require.Equal(t, int64(2), envelope.RouteGeneration)
}

func TestFallbackCASConflictAbortsBeforeCallingSecondCandidate(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	second := testCandidate("fastlet-b", "pod-b", "10.0.0.2")
	registry.candidates = append(registry.candidates, second)
	registry.fastlets[second.ID] = second
	fastlet.createFailures = map[string]error{"10.0.0.1": &fastletapi.CreateCallError{
		Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects,
		Failure:     &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "full", Retryable: true},
	}}
	k8sClient.patchError = apierrors.NewConflict(schema.GroupResource{Group: apiv1alpha2.GroupVersion.Group, Resource: "sandboxes"}, "request-a", errors.New("assignment changed"))

	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Equal(t, codes.Aborted, status.Code(err))
	fastlet.mu.Lock()
	require.Equal(t, []string{"10.0.0.1"}, fastlet.createIPs)
	fastlet.mu.Unlock()

	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	envelope, err := assignment.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, "fastlet-a", envelope.FastletName)
}

func TestExistingIntentExplicitRejectionIsNeverDeleted(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	fastlet.createFailure = errors.New("transport response lost")
	_, err := server.CreateSandbox(context.Background(), createRequest("existing-intent"))
	require.Equal(t, codes.Unavailable, status.Code(err))

	fastlet.createFailure = &fastletapi.CreateCallError{
		Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects,
		Failure:     &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "still full", Retryable: true},
	}
	_, err = server.CreateSandbox(context.Background(), createRequest("existing-intent"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "existing-intent"}, &persisted))
}

func TestAmbiguousFailureNeverReassigns(t *testing.T) {
	server, k8sClient, registry, fastlet := newV2Server(t)
	second := testCandidate("fastlet-b", "pod-b", "10.0.0.2")
	registry.candidates = append(registry.candidates, second)
	registry.fastlets[second.ID] = second
	fastlet.createFailure = errors.New("transport response lost")
	_, err := server.CreateSandbox(context.Background(), createRequest("request-a"))
	require.Equal(t, codes.Unavailable, status.Code(err))
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "request-a"}, &persisted))
	envelope, err := assignment.AssignmentFromAnnotation(&persisted)
	require.NoError(t, err)
	require.Equal(t, "fastlet-a", envelope.FastletName)
}

func TestOrderedActionBindingsCreateAndAtomicReplace(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	request := createRequest("action-sandbox")
	request.ActionBindings = []*fastpathv2.ActionBinding{
		{Handler: "audit", Input: `{"z":1,"a":2}`},
		{Handler: "egress", Input: `{"allow":["example.com"]}`},
	}
	created, err := server.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	fastlet.mu.Lock()
	require.Equal(t, []string{"audit", "egress"}, []string{fastlet.createRequests[0].ActionBindings[0].Handler, fastlet.createRequests[0].ActionBindings[1].Handler})
	require.Equal(t, `{"z":1,"a":2}`, fastlet.createRequests[0].ActionBindings[0].Input)
	fastlet.mu.Unlock()

	updated, err := server.UpdateSandbox(context.Background(), &fastpathv2.UpdateSandboxRequest{
		Sandbox: expectedReference(created.Sandbox.GetIdentity().GetName(), "default"), ExpectedGeneration: created.Generation,
		Update: &fastpathv2.UpdateSandboxRequest_ActionBindings{ActionBindings: &fastpathv2.ReplaceActionBindings{Items: []*fastpathv2.ActionBinding{
			{Handler: "egress", Input: `{"allow":[]}`},
		}}},
	})
	require.NoError(t, err)
	require.NotZero(t, updated.CommittedGeneration)
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "action-sandbox"}, &persisted))
	require.Len(t, persisted.Spec.ActionBindings, 1)
	require.Equal(t, "egress", persisted.Spec.ActionBindings[0].Handler)
}

func TestCreateRejectsUndeclaredOrDuplicateActionBindings(t *testing.T) {
	server, _, _, _ := newV2Server(t)
	undeclared := createRequest("undeclared")
	undeclared.ActionBindings = []*fastpathv2.ActionBinding{{Handler: "missing", Input: `{}`}}
	_, err := server.CreateSandbox(context.Background(), undeclared)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	duplicate := createRequest("duplicate")
	duplicate.ActionBindings = []*fastpathv2.ActionBinding{{Handler: "egress", Input: `{}`}, {Handler: "egress", Input: `null`}}
	_, err = server.CreateSandbox(context.Background(), duplicate)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGetUsesLiveFastletStateAndGenerationFence(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	fastlet.inspectStatus = testObservedStatus(created.Sandbox.GetIdentity().GetUid(), "infra-pending", created.Generation, nil)
	response, err := server.GetSandbox(context.Background(), &fastpathv2.GetSandboxRequest{
		Sandbox: expectedReference("sandbox-a", "default"), ExpectedGeneration: created.Generation,
	})
	require.NoError(t, err)
	require.Equal(t, fastpathv2.RuntimeState_RUNTIME_STATE_READY, response.Sandbox.Runtime.State)
	require.Equal(t, fastpathv2.DataPlaneState_DATA_PLANE_STATE_PUBLISHING, response.Sandbox.DataPlane.State)
	require.False(t, response.Sandbox.Ready)

	_, err = server.GetSandbox(context.Background(), &fastpathv2.GetSandboxRequest{Sandbox: expectedReference("sandbox-a", "default"), ExpectedGeneration: created.Generation + 1})
	require.Equal(t, codes.Aborted, status.Code(err))
}

func TestSandboxReferenceExpectedUIDFencesAllMutatingAndRoutingAPIs(t *testing.T) {
	server, _, _, _ := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	wrong := &fastpathv2.SandboxReference{
		NamespacedName: &fastpathv2.NamespacedName{Namespace: "default", Name: "sandbox-a"},
		ExpectedUid:    "replacement-uid",
	}

	_, err = server.GetSandbox(context.Background(), &fastpathv2.GetSandboxRequest{Sandbox: wrong})
	require.Equal(t, codes.Aborted, status.Code(err))
	_, err = server.UpdateSandbox(context.Background(), &fastpathv2.UpdateSandboxRequest{
		Sandbox: wrong, ExpectedGeneration: created.Generation,
		Update: &fastpathv2.UpdateSandboxRequest_ExpiresAtUnixSeconds{ExpiresAtUnixSeconds: time.Now().Add(time.Hour).Unix()},
	})
	require.Equal(t, codes.Aborted, status.Code(err))
	_, err = server.ResolveEndpoint(context.Background(), &fastpathv2.ResolveEndpointRequest{
		Sandbox: wrong, Target: &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 8080}},
	})
	require.Equal(t, codes.Aborted, status.Code(err))
	_, err = server.DeleteSandbox(context.Background(), &fastpathv2.DeleteRequest{Sandbox: wrong})
	require.Equal(t, codes.Aborted, status.Code(err))
}

func TestMetadataUpdateDoesNotChangeSpecGeneration(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	updated, err := server.UpdateSandbox(context.Background(), &fastpathv2.UpdateSandboxRequest{
		Sandbox: expectedReference("sandbox-a", "default"), ExpectedGeneration: created.Generation,
		MetadataUpsert: map[string]string{"team": "runtime"},
	})
	require.NoError(t, err)
	require.Equal(t, created.Generation, updated.CommittedGeneration)
	var persisted apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &persisted))
	require.Equal(t, "runtime", persisted.Labels[metadataLabelKey("team")])
}

func TestGetPoolCacheMissFallsBackToAPIReader(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	emptyCache := fake.NewClientBuilder().WithScheme(k8sClient.Client.Scheme()).Build()
	server.RouteCache = emptyCache
	pool, err := server.GetPool(context.Background(), &fastpathv2.GetPoolRequest{Namespace: "default", PoolName: "pool-a"})
	require.NoError(t, err)
	require.Equal(t, "pool-a", pool.Name)
	_, gets, _, _ := k8sClient.counts()
	require.Equal(t, 1, gets)
}

func TestDeleteOnlyCommitsDesiredState(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Finalizers = []string{"sandbox.fast.io/cleanup"}
	require.NoError(t, k8sClient.Client.Update(context.Background(), &sandbox))
	deleted, err := server.DeleteSandbox(context.Background(), &fastpathv2.DeleteRequest{Sandbox: &fastpathv2.SandboxReference{
		NamespacedName: &fastpathv2.NamespacedName{Name: created.Sandbox.GetIdentity().GetName(), Namespace: "default"},
		ExpectedUid:    created.Sandbox.GetIdentity().GetUid(),
	}})
	require.NoError(t, err)
	require.NotNil(t, deleted)
	require.NoError(t, k8sClient.Client.Get(context.Background(), client.ObjectKeyFromObject(&sandbox), &sandbox))
	require.NotNil(t, sandbox.DeletionTimestamp)
}

func TestGetAndListPoolsExposeCapabilitiesWithoutRegistryProjection(t *testing.T) {
	server, _, _, _ := newV2Server(t)
	info, err := server.GetPool(context.Background(), &fastpathv2.GetPoolRequest{Namespace: "default", PoolName: "pool-a"})
	require.NoError(t, err)
	require.Equal(t, "container", info.Runtime)
	require.Equal(t, "500m", info.SandboxCpu)
	require.Equal(t, int32(2), info.ReadyFastlets)
	require.Equal(t, "execd", info.Components[0].Name)
	require.Equal(t, uint32(44772), info.Components[0].Port)
	require.Equal(t, "alpine:latest", info.WarmImages[0].Image)

	listed, err := server.ListPools(context.Background(), &fastpathv2.ListPoolsRequest{Namespace: "default"})
	require.NoError(t, err)
	require.Len(t, listed.Items, 1)
}

func TestGetSandboxDiagnosticsUsesAnnotationAndDegrades(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("sandbox-a"))
	require.NoError(t, err)
	fastlet.diagnostics = &fastletapi.SandboxDiagnosticsResponse{
		Sandbox: testObservedStatus(created.Sandbox.GetIdentity().GetUid(), "running", created.Generation, nil),
		Events:  []fastletapi.SandboxDiagnosticEvent{{Timestamp: metav1.Now().Time, Level: "info", Source: "runtime", Phase: "running", Message: "ready"}},
	}
	diagnostics, err := server.GetSandboxDiagnostics(context.Background(), &fastpathv2.SandboxDiagnosticsRequest{SandboxName: "sandbox-a", Namespace: "default"})
	require.NoError(t, err)
	require.True(t, diagnostics.FastletReachable)
	require.Equal(t, "status-projection-pending", diagnostics.AssignmentState)
	require.Equal(t, "ready", diagnostics.Events[0].Message)

	fastlet.diagnostics, fastlet.diagnosticsErr = nil, errors.New("connection refused")
	diagnostics, err = server.GetSandboxDiagnostics(context.Background(), &fastpathv2.SandboxDiagnosticsRequest{SandboxName: "sandbox-a", Namespace: "default"})
	require.NoError(t, err)
	require.False(t, diagnostics.FastletReachable)
	require.Contains(t, diagnostics.FastletError, "connection refused")
}

func newV2Server(t *testing.T) (*Server, *countingUIDClient, *fastpathRegistry, *fastpathFastlet) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	pool := discoveryPool("pool-a", "default")
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha2.Sandbox{}).WithObjects(pool).Build()
	k8sClient := &countingUIDClient{Client: baseClient}
	candidate := testCandidate("fastlet-a", "pod-a", "10.0.0.1")
	registry := &fastpathRegistry{candidates: []placement.FastletInfo{candidate}, fastlets: map[placement.FastletID]placement.FastletInfo{candidate.ID: candidate}}
	fastlet := &fastpathFastlet{}
	orchestrator := &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	return &Server{K8sClient: k8sClient, RouteCache: baseClient, Orchestrator: orchestrator, DiagnosticsClient: fastlet}, k8sClient, registry, fastlet
}

func discoveryPool(name, namespace string) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, Capacity: apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1}, MaxSandboxesPerPod: 8,
			SandboxResources: apiv1alpha2.SandboxResourceProfile{CPU: resource.MustParse("500m"), Memory: resource.MustParse("512Mi"), PIDs: 128},
			ActionHandlers:   []apiv1alpha2.ActionHandler{{Name: "audit", TargetHTTPPort: 18081}, {Name: "egress", TargetHTTPPort: 18080}},
			InfraComponents: []apiv1alpha2.InfraComponent{{
				Name: "execd", Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
				Process: apiv1alpha2.InfraProcess{HealthCheck: apiv1alpha2.InfraHealthCheck{HTTPGet: &apiv1alpha2.InfraHTTPGet{Path: "/health"}}},
			}},
		},
		Status: apiv1alpha2.SandboxPoolStatus{
			CurrentPods: 3, ReadyPods: 2, IdleFastlets: 1, InfraRevision: "sha256:infra", PreparedFastlets: 2,
			WarmImages: []apiv1alpha2.WarmImageStatus{{Image: "alpine:latest", DesiredFastlets: 3, CachedFastlets: 2}},
		},
	}
}

func testCandidate(name, uid, ip string) placement.FastletInfo {
	return placement.FastletInfo{ID: placement.FastletID(name), PodName: name, PodUID: uid, PodIP: ip, NodeName: "node-a",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		InfraRevision: "infra-hash", InfraReady: true}
}

func createRequest(requestID string) *fastpathv2.CreateSandboxRequest {
	return &fastpathv2.CreateSandboxRequest{Image: "alpine:latest", PoolRef: "pool-a", Namespace: "default", RequestId: requestID,
		Envs: map[string]string{"A": "B"}, WorkingDir: "/workspace", Completion: fastpathv2.CreateCompletion_CREATE_COMPLETION_READY}
}

func expectedReference(name, namespace string) *fastpathv2.SandboxReference {
	return &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Name: name, Namespace: namespace}}
}

func TestSandboxFromCreateRequestUsesCanonicalFields(t *testing.T) {
	request := createRequest("request-a")
	bindings, err := apiActionBindings(request.ActionBindings)
	require.NoError(t, err)
	sandbox := sandboxFromCreateRequest(request, bindings, "create-hash")
	require.Equal(t, request.RequestId, sandbox.Name)
	require.Equal(t, request.Image, sandbox.Spec.Image)
	require.Equal(t, request.PoolRef, sandbox.Spec.PoolRef)
	require.Equal(t, []metav1.Condition(nil), sandbox.Status.Conditions)
}

func TestCreateRejectsExpiredDeadlineBeforeCRDWrite(t *testing.T) {
	server, k8sClient, _, _ := newV2Server(t)
	request := createRequest("expired")
	request.ExpiresAtUnixSeconds = time.Now().Add(-time.Minute).Unix()
	_, err := server.CreateSandbox(context.Background(), request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	creates, _, _, _ := k8sClient.counts()
	require.Zero(t, creates)
}
