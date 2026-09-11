package fastpath

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	routeauth "fast-sandbox/internal/dataplane/auth"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type Server struct {
	fastpathv2.UnimplementedFastPathServiceServer
	K8sClient         client.Client
	RouteCache        client.Client
	Orchestrator      *orchestration.Orchestrator
	DiagnosticsClient interface {
		SandboxDiagnostics(context.Context, string, *fastletapi.SandboxDiagnosticsRequest) (*fastletapi.SandboxDiagnosticsResponse, error)
	}
	CredentialIssuer    *routeauth.Issuer
	SandboxProxyBaseURL string
	DefaultNamespace    string
}

var _ fastpathv2.FastPathServiceServer = &Server{}

const (
	metadataLabelPrefix = "metadata.sandbox.fast.io/"
)

type createCompletion struct {
	api     fastpathv2.CreateCompletion
	fastlet fastletapi.CreateCompletion
}

type plannedCreate struct {
	sandbox    *apiv1alpha2.Sandbox
	candidates []placement.FastletInfo
	assignment assignment.AssignmentEnvelope
}

type acceptedCreate struct {
	sandbox    *apiv1alpha2.Sandbox
	candidates []placement.FastletInfo
	assignment assignment.AssignmentEnvelope
	created    bool
}

func (s *Server) CreateSandbox(ctx context.Context, request *fastpathv2.CreateSandboxRequest) (_ *fastpathv2.CreateSandboxResponse, resultErr error) {
	started := time.Now()
	acceptedObserved := false
	defer func() {
		success := "true"
		if resultErr != nil {
			success = "false"
		}
		createSandboxDuration.WithLabelValues("v2", success).Observe(time.Since(started).Seconds())
		createRuntimeReadyLatency.WithLabelValues(grpcMetricResult(resultErr)).Observe(time.Since(started).Seconds())
		if !acceptedObserved {
			observeCreateAccepted("rejected", started, resultErr)
		}
	}()

	if err := s.validateCreateRequest(request); err != nil {
		return nil, err
	}
	prepStarted := time.Now()
	completion, err := parseCreateCompletion(request.Completion)
	if err != nil {
		return nil, err
	}
	pool, err := s.getPool(ctx, request.Namespace, request.PoolRef)
	if err != nil {
		return nil, err
	}
	bindings, err := buildActionBindings(request, pool)
	if err != nil {
		return nil, err
	}
	// A create whose image resolves to a snapshot re-applies the source
	// Sandbox's recorded runtime policy (network policy via the egress
	// handler, lifecycle hooks): explicit bindings win per handler, and
	// only handlers the target Pool declares are filled in.
	bindings, err = s.applySnapshotRecordedBindings(ctx, request, bindings, pool)
	if err != nil {
		return nil, err
	}

	ctx = observability.WithIdentity(ctx, observability.Identity{RequestID: request.RequestId, Namespace: request.Namespace, SandboxName: request.RequestId})
	createSpecHash, err := CreateSpecHash(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hash create request: %v", err)
	}
	orchestrator, err := s.orchestrator()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	sandbox := newSandboxCRD(request, bindings, createSpecHash)
	rankedFastlets, envelope, err := s.selectFastletsForPool(ctx, orchestrator, pool, sandbox, request.RequestId)
	if err != nil {
		return nil, err
	}
	accepted, err := s.acceptCreateIntent(ctx, plannedCreate{sandbox: sandbox, candidates: rankedFastlets, assignment: envelope})
	if err != nil {
		return nil, err
	}
	if accepted.created {
		observeCreateAccepted("crd", started, nil)
	} else {
		observeCreateAccepted("idempotent", started, nil)
	}
	acceptedObserved = true
	prepDur := time.Since(prepStarted)

	observed, err := s.provisionRuntime(ctx, accepted, completion.fastlet)
	runtimeDur := time.Since(prepStarted)
	if err != nil {
		return nil, err
	}
	klog.InfoS("fastpath sandbox created",
		"requestId", request.RequestId,
		"total", time.Since(started).String(),
		"prep", prepDur.String(),
		"runtime", runtimeDur.String(),
	)
	return &fastpathv2.CreateSandboxResponse{
		Sandbox: sandboxInfoFromFastlet(accepted.sandbox, observed), Generation: accepted.sandbox.Generation, Completion: completion.api,
	}, nil
}

func (s *Server) validateCreateRequest(request *fastpathv2.CreateSandboxRequest) error {
	if request == nil || request.Image == "" || request.PoolRef == "" {
		return status.Error(codes.InvalidArgument, "image and pool_ref are required")
	}
	if request.Namespace == "" {
		request.Namespace = s.defaultNamespace()
	}
	if err := ValidateRequestID(request.RequestId); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateCreateOptions(request, time.Now()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// applySnapshotRecordedBindings merges the action bindings recorded on the
// newest SandboxSnapshot whose templateName equals the create's image:
// checkpoint and restore thereby preserve the source's runtime policy.
// Explicit request bindings override recorded ones per handler; recorded
// handlers the target Pool does not declare are dropped (they could not
// take effect there anyway). The spec hash stays over the caller's own
// request, so a policy change between retries never conflicts a replay.
func (s *Server) applySnapshotRecordedBindings(ctx context.Context, request *fastpathv2.CreateSandboxRequest, explicit []apiv1alpha2.ActionBinding, pool *apiv1alpha2.SandboxPool) ([]apiv1alpha2.ActionBinding, error) {
	if request.Image == "" || len(pool.Spec.ActionHandlers) == 0 {
		return explicit, nil
	}
	var list apiv1alpha2.SandboxSnapshotList
	reader := s.K8sClient
	if s.RouteCache != nil {
		reader = s.RouteCache
	}
	if err := reader.List(ctx, &list, client.InNamespace(request.Namespace)); err != nil {
		// Best-effort: an unreachable cache falls back to the durable client.
		if reader == s.K8sClient || s.K8sClient.List(ctx, &list, client.InNamespace(request.Namespace)) != nil {
			return explicit, grpcKubernetesError(err)
		}
	}
	var newest *apiv1alpha2.SandboxSnapshot
	for index := range list.Items {
		item := &list.Items[index]
		if item.Spec.TemplateName != request.Image || item.Annotations[assignment.AnnotationSourceActionBindings] == "" {
			continue
		}
		if newest == nil || item.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = item
		}
	}
	if newest == nil {
		return explicit, nil
	}
	bindings, err := decodeSourceActionBindings(newest.Annotations[assignment.AnnotationSourceActionBindings])
	if err != nil {
		klog.FromContext(ctx).Error(err, "Decode snapshot source action bindings", "snapshot", newest.Name)
		return explicit, nil
	}
	handlers := make(map[string]struct{}, len(pool.Spec.ActionHandlers))
	for _, handler := range pool.Spec.ActionHandlers {
		handlers[handler.Name] = struct{}{}
	}
	overrides := make(map[string]struct{}, len(explicit))
	for _, binding := range explicit {
		overrides[binding.Handler] = struct{}{}
	}
	merged := explicit
	for _, binding := range bindings {
		if _, done := overrides[binding.Handler]; done {
			continue
		}
		if _, declared := handlers[binding.Handler]; !declared {
			continue
		}
		merged = append(merged, binding)
	}
	if len(merged) != len(explicit) {
		klog.FromContext(ctx).Info("Re-applying snapshot-recorded action bindings",
			"image", request.Image, "snapshot", newest.Name, "recorded", len(bindings), "applied", len(merged)-len(explicit))
	}
	return merged, nil
}

func buildActionBindings(request *fastpathv2.CreateSandboxRequest, pool *apiv1alpha2.SandboxPool) ([]apiv1alpha2.ActionBinding, error) {
	bindings, err := apiActionBindings(request.ActionBindings)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := pool.Spec.ValidateActionHandlers(); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid Pool Action Handlers: %v", err)
	}
	if err := (&apiv1alpha2.SandboxSpec{ActionBindings: bindings}).ValidateActionBindings(pool.Spec.ActionHandlers); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return bindings, nil
}

func newSandboxCRD(request *fastpathv2.CreateSandboxRequest, bindings []apiv1alpha2.ActionBinding, createSpecHash string) *apiv1alpha2.Sandbox {
	return sandboxFromCreateRequest(request, bindings, createSpecHash)
}

func (s *Server) selectFastletsForPool(ctx context.Context, orchestrator *orchestration.Orchestrator, pool *apiv1alpha2.SandboxPool, sandbox *apiv1alpha2.Sandbox, stableKey string) ([]placement.FastletInfo, assignment.AssignmentEnvelope, error) {
	_, finishCandidates := startCreateStage(ctx, "candidate_selection")
	rankedFastlets, err := orchestrator.FastPathCandidates(sandbox, stableKey)
	finishCandidates(err)
	if err != nil {
		if errors.Is(err, orchestration.ErrNoCandidate) {
			return nil, assignment.AssignmentEnvelope{}, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, assignment.AssignmentEnvelope{}, err
	}
	rankedFastlets = compatibleCandidates(rankedFastlets, pool.Status.FastletRevision)
	if len(rankedFastlets) == 0 {
		return nil, assignment.AssignmentEnvelope{}, status.Error(codes.ResourceExhausted, "no Fastlet has the current Pool compatibility revision")
	}
	runtimeInstanceID, err := idgen.GenerateRequestID()
	if err != nil {
		return nil, assignment.AssignmentEnvelope{}, status.Errorf(codes.Internal, "generate runtime instance ID: %v", err)
	}
	envelope, err := orchestration.AssignmentForCandidate(rankedFastlets[0], 1, apiv1alpha2.InitialInstanceGeneration, 1, runtimeInstanceID)
	if err != nil {
		return nil, assignment.AssignmentEnvelope{}, status.Errorf(codes.FailedPrecondition, "invalid Fastlet candidate: %v", err)
	}
	if err := assignment.SetAssignmentAnnotation(sandbox, envelope); err != nil {
		return nil, assignment.AssignmentEnvelope{}, status.Errorf(codes.Internal, "encode assignment: %v", err)
	}
	return rankedFastlets, envelope, nil
}

func (s *Server) acceptCreateIntent(ctx context.Context, plan plannedCreate) (*acceptedCreate, error) {
	crdContext, finishCRDCreate := startCreateStage(ctx, "crd_create")
	createErr := s.K8sClient.Create(crdContext, plan.sandbox)
	finishCRDCreate(createErr)
	if createErr == nil {
		return &acceptedCreate{sandbox: plan.sandbox, candidates: plan.candidates, assignment: plan.assignment, created: true}, nil
	}
	var existing apiv1alpha2.Sandbox
	getContext, finishRead := startCreateStage(ctx, "idempotency_read")
	getErr := s.K8sClient.Get(getContext, client.ObjectKeyFromObject(plan.sandbox), &existing)
	finishRead(getErr)
	if getErr != nil {
		return nil, grpcKubernetesError(errors.Join(createErr, getErr))
	}
	if existing.Annotations[assignment.AnnotationRequestID] != plan.sandbox.Annotations[assignment.AnnotationRequestID] ||
		existing.Annotations[assignment.AnnotationCreateSpecHash] != plan.sandbox.Annotations[assignment.AnnotationCreateSpecHash] {
		return nil, status.Errorf(codes.AlreadyExists, "Sandbox name %q belongs to another create intent", plan.sandbox.Name)
	}
	existingEnvelope, err := assignment.AssignmentFromAnnotation(&existing)
	if err != nil || existingEnvelope == nil {
		return nil, status.Errorf(codes.Unavailable, "existing Sandbox assignment is not ready: %v", err)
	}
	selected, ok := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(existingEnvelope.FastletName))
	if !ok {
		return nil, status.Error(codes.Unavailable, orchestration.ErrAssignedFastletUnavailable.Error())
	}
	return &acceptedCreate{sandbox: existing.DeepCopy(), candidates: []placement.FastletInfo{selected}, assignment: *existingEnvelope}, nil
}

func (s *Server) provisionRuntime(ctx context.Context, accepted *acceptedCreate, completion fastletapi.CreateCompletion) (*fastletapi.SandboxStatus, error) {
	for index, candidate := range accepted.candidates {
		if index > 0 {
			orchestration.RecordTopKRetry("attempt")
			if err := s.advanceCreateAssignment(ctx, accepted, candidate); err != nil {
				return nil, err
			}
		}
		fastletContext, finishFastletCreate := startCreateStage(ctx, "fastlet_create")
		response, callErr := s.Orchestrator.CreateRuntimeOnCandidateWithCompletion(fastletContext, accepted.sandbox, candidate, accepted.assignment, completion)
		finishFastletCreate(callErr)
		if callErr == nil && response != nil && response.Sandbox != nil {
			return response.Sandbox, nil
		}
		if callErr == nil {
			return nil, status.Error(codes.Unavailable, "Fastlet Create returned no Sandbox observation; Sandbox intent is persisted for Controller recovery")
		}
		if orchestration.IsCandidateRejection(callErr) && index+1 < len(accepted.candidates) {
			orchestration.RecordTopKRetry("candidate_rejected")
			s.Orchestrator.RecordCandidateFeedback(candidate.ID, callErr)
			continue
		}
		if !orchestration.IsCandidateRejection(callErr) {
			return nil, status.Errorf(codes.Unavailable, "Sandbox intent is persisted and Controller will retry: %v", callErr)
		}
		if accepted.created {
			uid := accepted.sandbox.UID
			rollbackErr := s.K8sClient.Delete(ctx, accepted.sandbox, client.Preconditions{UID: &uid})
			if rollbackErr != nil && !apierrors.IsNotFound(rollbackErr) {
				return nil, status.Errorf(codes.Unavailable, "all Fastlet candidates rejected and intent rollback failed: rejection=%v rollback=%v", callErr, rollbackErr)
			}
		}
		return nil, status.Errorf(codes.ResourceExhausted, "all Fastlet candidates rejected admission: %v", callErr)
	}
	return nil, status.Error(codes.ResourceExhausted, orchestration.ErrNoCandidate.Error())
}

func (s *Server) advanceCreateAssignment(ctx context.Context, accepted *acceptedCreate, candidate placement.FastletInfo) error {
	runtimeInstanceID, err := idgen.GenerateRequestID()
	if err != nil {
		return status.Errorf(codes.Internal, "generate runtime instance ID: %v", err)
	}
	next, err := orchestration.AssignmentForCandidate(candidate, accepted.assignment.Attempt+1, accepted.assignment.InstanceGeneration, accepted.assignment.RouteGeneration+1, runtimeInstanceID)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "invalid Fastlet candidate: %v", err)
	}
	sandbox, err := assignment.CASAssignmentAnnotation(ctx, s.K8sClient, client.ObjectKeyFromObject(accepted.sandbox), accepted.assignment, next)
	if err != nil {
		return status.Errorf(codes.Aborted, "assignment changed concurrently: %v", err)
	}
	accepted.sandbox, accepted.assignment = sandbox, next
	return nil
}

func (s *Server) GetSandbox(ctx context.Context, request *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error) {
	if request == nil || request.Sandbox == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	sandbox, err := s.sandboxFromReferenceAtGeneration(ctx, request.Sandbox, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	if err := checkGenerationFloor(sandbox, request.ExpectedGeneration); err != nil {
		return nil, err
	}
	info, _, _, err := s.inspectAssignedSandbox(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return &fastpathv2.GetSandboxResponse{Sandbox: info, Generation: sandbox.Generation}, nil
}

func (s *Server) ListSandboxes(ctx context.Context, request *fastpathv2.ListSandboxesRequest) (*fastpathv2.ListSandboxesResponse, error) {
	if request == nil {
		request = &fastpathv2.ListSandboxesRequest{}
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	if request.PageSize < 0 || request.PageSize > 500 {
		return nil, status.Error(codes.InvalidArgument, "page_size must be between 0 and 500")
	}
	if err := validateMetadata(request.Metadata); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	options := []client.ListOption{client.InNamespace(namespace)}
	if len(request.Metadata) > 0 {
		selector := make(map[string]string, len(request.Metadata))
		for name, value := range request.Metadata {
			selector[metadataLabelKey(name)] = value
		}
		options = append(options, client.MatchingLabels(selector))
	}
	if request.PageSize > 0 {
		options = append(options, client.Limit(request.PageSize))
	}
	if request.PageToken != "" {
		options = append(options, client.Continue(request.PageToken))
	}
	reader := s.K8sClient
	if s.RouteCache != nil && request.PageSize == 0 && request.PageToken == "" {
		reader = s.RouteCache
	}
	var list apiv1alpha2.SandboxList
	if err := reader.List(ctx, &list, options...); err != nil {
		return nil, grpcKubernetesError(err)
	}
	response := &fastpathv2.ListSandboxesResponse{Items: make([]*fastpathv2.SandboxSummary, 0, len(list.Items)), NextPageToken: list.Continue}
	for index := range list.Items {
		response.Items = append(response.Items, &fastpathv2.SandboxSummary{Identity: protoIdentity(&list.Items[index]), Generation: list.Items[index].Generation})
	}
	return response, nil
}

func (s *Server) UpdateSandbox(ctx context.Context, request *fastpathv2.UpdateSandboxRequest) (*fastpathv2.UpdateSandboxResponse, error) {
	if request == nil || request.Sandbox == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	if err := validateMetadata(request.MetadataUpsert); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, name := range request.MetadataDeleteKeys {
		if problems := kvalidation.IsDNS1123Label(name); len(problems) > 0 {
			return nil, status.Errorf(codes.InvalidArgument, "metadata delete key %q is invalid: %s", name, problems[0])
		}
	}
	initial, err := s.sandboxFromReferenceAtGeneration(ctx, request.Sandbox, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKeyFromObject(initial)
	var updated apiv1alpha2.Sandbox
	firstAttempt := true
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var sandbox apiv1alpha2.Sandbox
		if firstAttempt {
			sandbox = *initial.DeepCopy()
			firstAttempt = false
		} else {
			if err := s.K8sClient.Get(ctx, key, &sandbox); err != nil {
				return err
			}
		}
		if err := checkExpectedGeneration(&sandbox, request.ExpectedGeneration); err != nil {
			return err
		}
		if err := checkReferenceUID(&sandbox, request.Sandbox.ExpectedUid); err != nil {
			return err
		}
		if err := s.applySandboxUpdate(ctx, &sandbox, request); err != nil {
			return err
		}
		if sandbox.Labels == nil {
			sandbox.Labels = make(map[string]string)
		}
		for name, value := range request.MetadataUpsert {
			sandbox.Labels[metadataLabelKey(name)] = value
		}
		for _, name := range request.MetadataDeleteKeys {
			delete(sandbox.Labels, metadataLabelKey(name))
		}
		if err := s.K8sClient.Update(ctx, &sandbox); err != nil {
			return err
		}
		updated = *sandbox.DeepCopy()
		return nil
	})
	if err != nil {
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, grpcKubernetesError(err)
	}
	return &fastpathv2.UpdateSandboxResponse{Sandbox: protoIdentity(&updated), CommittedGeneration: updated.Generation}, nil
}

func (s *Server) applySandboxUpdate(ctx context.Context, sandbox *apiv1alpha2.Sandbox, request *fastpathv2.UpdateSandboxRequest) error {
	switch value := request.Update.(type) {
	case nil:
		return nil
	case *fastpathv2.UpdateSandboxRequest_ExpiresAtUnixSeconds:
		if value.ExpiresAtUnixSeconds < 0 || value.ExpiresAtUnixSeconds > 0 && !time.Unix(value.ExpiresAtUnixSeconds, 0).After(time.Now()) {
			return status.Error(codes.InvalidArgument, "expires_at_unix_seconds must be zero or in the future")
		}
		if value.ExpiresAtUnixSeconds == 0 {
			sandbox.Spec.ExpireTime = nil
		} else {
			expires := metav1.NewTime(time.Unix(value.ExpiresAtUnixSeconds, 0).UTC())
			sandbox.Spec.ExpireTime = &expires
		}
	case *fastpathv2.UpdateSandboxRequest_ResetRevision:
		parsed, err := time.Parse(time.RFC3339Nano, value.ResetRevision)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid reset_revision: %v", err)
		}
		sandbox.Spec.ResetRevision = &metav1.Time{Time: parsed}
	case *fastpathv2.UpdateSandboxRequest_FailurePolicy:
		if value.FailurePolicy != fastpathv2.FailurePolicy_MANUAL && value.FailurePolicy != fastpathv2.FailurePolicy_AUTO_RECREATE {
			return status.Error(codes.InvalidArgument, "failure_policy is invalid")
		}
		sandbox.Spec.FailurePolicy = toFailurePolicy(value.FailurePolicy)
	case *fastpathv2.UpdateSandboxRequest_RecoveryTimeoutSeconds:
		if value.RecoveryTimeoutSeconds < 0 || value.RecoveryTimeoutSeconds > 86400 {
			return status.Error(codes.InvalidArgument, "recovery_timeout_seconds must be between 0 and 86400")
		}
		sandbox.Spec.RecoveryTimeoutSeconds = value.RecoveryTimeoutSeconds
	case *fastpathv2.UpdateSandboxRequest_ActionBindings:
		if value.ActionBindings == nil {
			return status.Error(codes.InvalidArgument, "action_bindings is required")
		}
		bindings, err := apiActionBindings(value.ActionBindings.Items)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		pool, err := s.getPool(ctx, sandbox.Namespace, sandbox.Spec.PoolRef)
		if err != nil {
			return err
		}
		probe := apiv1alpha2.SandboxSpec{ActionBindings: bindings}
		if err := probe.ValidateActionBindings(pool.Spec.ActionHandlers); err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		sandbox.Spec.ActionBindings = bindings
	default:
		return status.Error(codes.InvalidArgument, "unknown Sandbox update")
	}
	return nil
}

func (s *Server) DeleteSandbox(ctx context.Context, request *fastpathv2.DeleteRequest) (*fastpathv2.DeleteResponse, error) {
	if request == nil || request.Sandbox == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	sandbox, err := s.sandboxFromReference(ctx, request.Sandbox)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &fastpathv2.DeleteResponse{}, nil
		}
		return nil, err
	}
	uid := sandbox.UID
	if err := s.K8sClient.Delete(ctx, sandbox, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return nil, grpcKubernetesError(err)
	}
	return &fastpathv2.DeleteResponse{}, nil
}

func (s *Server) ResolveEndpoint(ctx context.Context, request *fastpathv2.ResolveEndpointRequest) (*fastpathv2.ResolveEndpointResponse, error) {
	if request == nil || request.Sandbox == nil || request.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference and endpoint target are required")
	}
	sandbox, err := s.sandboxFromReferenceAtGeneration(ctx, request.Sandbox, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	if err := checkGenerationFloor(sandbox, request.ExpectedGeneration); err != nil {
		return nil, err
	}
	port, componentName, protocol, err := s.resolveEndpointTarget(ctx, sandbox, request.Target)
	if err != nil {
		return nil, err
	}
	info, envelope, fastlet, err := s.inspectAssignedSandbox(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	if !info.Ready {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox interaction is not Ready")
	}
	if componentName != "" && !componentReady(info.InfraComponents, componentName) {
		return nil, status.Errorf(codes.FailedPrecondition, "Infra Component %q route is not Ready", componentName)
	}
	credential, claims, err := s.issueRouteCredential(ctx, sandbox, port, componentName, protocol)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(s.SandboxProxyBaseURL, "/")
	if request.AccessMode == fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY {
		baseURL = "http://" + fastlet.PodIP + ":5780"
	}
	if baseURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox Proxy base URL is not configured")
	}
	path := dataplane.RoutePath(string(sandbox.UID), port)
	if componentName != "" {
		path = dataplane.ComponentRoutePath(string(sandbox.UID), componentName)
	}
	_ = envelope
	return &fastpathv2.ResolveEndpointResponse{
		SandboxUid: string(sandbox.UID),
		Endpoint: &fastpathv2.ResolvedEndpoint{
			ComponentName: componentName, Protocol: protocol, Port: port,
		},
		ProxyEndpoint:   baseURL + path,
		RequiredHeaders: map[string]string{dataplane.HeaderRouteCredential: credential},
		RouteGeneration: claims.RouteGeneration, ExpiresAtUnixSeconds: claims.ExpiresAt,
	}, nil
}

func (s *Server) GetPool(ctx context.Context, request *fastpathv2.GetPoolRequest) (*fastpathv2.PoolInfo, error) {
	if request == nil || request.PoolName == "" {
		return nil, status.Error(codes.InvalidArgument, "pool_name is required")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	pool, err := s.getPool(ctx, namespace, request.PoolName)
	if err != nil {
		return nil, err
	}
	return poolInfo(pool), nil
}

func (s *Server) ListPools(ctx context.Context, request *fastpathv2.ListPoolsRequest) (*fastpathv2.ListPoolsResponse, error) {
	namespace := s.defaultNamespace()
	if request != nil && request.Namespace != "" {
		namespace = request.Namespace
	}
	var pools apiv1alpha2.SandboxPoolList
	if err := s.K8sClient.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return nil, grpcKubernetesError(err)
	}
	sort.Slice(pools.Items, func(i, j int) bool { return pools.Items[i].Name < pools.Items[j].Name })
	response := &fastpathv2.ListPoolsResponse{Items: make([]*fastpathv2.PoolInfo, 0, len(pools.Items))}
	for index := range pools.Items {
		response.Items = append(response.Items, poolInfo(&pools.Items[index]))
	}
	return response, nil
}

func poolInfo(pool *apiv1alpha2.SandboxPool) *fastpathv2.PoolInfo {
	info := &fastpathv2.PoolInfo{
		Namespace: pool.Namespace, Name: pool.Name, Runtime: string(pool.Spec.Runtime),
		SandboxCpu: pool.Spec.SandboxResources.CPU.String(), SandboxMemory: pool.Spec.SandboxResources.Memory.String(),
		SandboxPids: pool.Spec.SandboxResources.PIDs, MaxSandboxesPerPod: pool.Spec.MaxSandboxesPerPod,
		TotalFastlets: pool.Status.CurrentPods, ReadyFastlets: pool.Status.ReadyPods, IdleFastlets: pool.Status.IdleFastlets,
		RuntimeRevision: pool.Status.RuntimeRevision, InfraRevision: pool.Status.InfraRevision,
		FastletRevision: pool.Status.FastletRevision, PreparedFastlets: pool.Status.PreparedFastlets,
	}
	for _, component := range pool.Spec.InfraComponents {
		healthKind := "TCP"
		if component.Process.HealthCheck.HTTPGet != nil {
			healthKind = "HTTP"
		}
		info.Components = append(info.Components, &fastpathv2.ComponentCapability{
			Name: component.Name, Protocol: component.Endpoint.Protocol, Port: uint32(component.Endpoint.Port), HealthKind: healthKind,
		})
	}
	for _, image := range pool.Status.WarmImages {
		info.WarmImages = append(info.WarmImages, &fastpathv2.WarmImageInfo{
			Image: image.Image, DesiredFastlets: image.DesiredFastlets, CachedFastlets: image.CachedFastlets,
			PullingFastlets: image.PullingFastlets, FailedFastlets: image.FailedFastlets,
			ObservedGeneration: image.ObservedGeneration, LastError: image.LastError,
		})
	}
	return info
}

func (s *Server) GetSandboxDiagnostics(ctx context.Context, request *fastpathv2.SandboxDiagnosticsRequest) (*fastpathv2.SandboxDiagnosticsResponse, error) {
	if request == nil || request.SandboxName == "" || request.Limit < 0 || request.Limit > 128 {
		return nil, status.Error(codes.InvalidArgument, "sandbox_name is required and limit must be between 0 and 128")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	var sandbox apiv1alpha2.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: request.SandboxName, Namespace: namespace}, &sandbox); err != nil {
		return nil, grpcKubernetesError(err)
	}
	response := &fastpathv2.SandboxDiagnosticsResponse{}
	envelope, annotationErr := assignment.AssignmentFromAnnotation(&sandbox)
	if annotationErr != nil {
		response.AssignmentState, response.FastletError = "invalid-annotation", annotationErr.Error()
		return response, nil
	}
	if envelope == nil {
		response.AssignmentState, response.FastletError = "unassigned", "Sandbox has no durable assignment"
		return response, nil
	}
	response.AssignmentState = "annotation-authoritative"
	if _, projectionErr := assignment.EffectiveAssignment(&sandbox); projectionErr != nil {
		response.AssignmentState, response.FastletError = "status-projection-conflict", projectionErr.Error()
	} else if sandbox.Status.Placement.FastletName == "" {
		response.AssignmentState = "status-projection-pending"
	} else {
		response.AssignmentState = "synchronized"
	}
	response.RuntimeInstanceId, response.AssignmentAttempt = envelope.RuntimeInstanceID, envelope.Attempt
	if s.Orchestrator == nil || s.Orchestrator.Registry == nil || s.DiagnosticsClient == nil {
		response.FastletError = appendDiagnosticError(response.FastletError, "Fastlet diagnostics client is not configured")
		return response, nil
	}
	fastlet, found := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !found || fastlet.PodUID != envelope.FastletPodUID {
		response.FastletError = appendDiagnosticError(response.FastletError, "assigned Fastlet is unavailable or replaced")
		return response, nil
	}
	fastletResponse, err := s.DiagnosticsClient.SandboxDiagnostics(ctx, fastlet.PodIP, &fastletapi.SandboxDiagnosticsRequest{
		Identity: internalIdentity(&sandbox, envelope), Limit: int(request.Limit),
	})
	if err != nil {
		response.FastletError = appendDiagnosticError(response.FastletError, err.Error())
		return response, nil
	}
	response.FastletReachable = true
	if fastletResponse.Sandbox != nil {
		response.Sandbox = sandboxInfoFromFastlet(&sandbox, fastletResponse.Sandbox)
	}
	for _, event := range fastletResponse.Events {
		response.Events = append(response.Events, &fastpathv2.SandboxDiagnosticEvent{
			TimestampUnixNano: event.Timestamp.UnixNano(), Level: event.Level, Source: event.Source, Phase: event.Phase, Message: event.Message,
		})
	}
	return response, nil
}

func (s *Server) inspectAssignedSandbox(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (*fastpathv2.SandboxInfo, *assignment.AssignmentEnvelope, placement.FastletInfo, error) {
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil || envelope == nil {
		return nil, nil, placement.FastletInfo{}, status.Error(codes.Unavailable, "Sandbox assignment is not ready")
	}
	if s.Orchestrator == nil || s.Orchestrator.Registry == nil || s.Orchestrator.FastletClient == nil {
		return nil, nil, placement.FastletInfo{}, status.Error(codes.FailedPrecondition, "Fastlet inspect client is not configured")
	}
	fastlet, found := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !found || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" {
		return nil, nil, placement.FastletInfo{}, status.Error(codes.Unavailable, "assigned Fastlet is unavailable")
	}
	response, err := s.Orchestrator.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &fastletapi.InspectSandboxRequest{Identity: internalIdentity(sandbox, envelope)})
	if err != nil {
		return nil, nil, placement.FastletInfo{}, status.Errorf(codes.Unavailable, "inspect assigned Fastlet: %v", err)
	}
	if response == nil || response.Sandbox == nil {
		return nil, nil, placement.FastletInfo{}, status.Error(codes.Unavailable, "assigned Fastlet returned no Sandbox state")
	}
	return sandboxInfoFromFastlet(sandbox, response.Sandbox), envelope, fastlet, nil
}

func sandboxInfoFromFastlet(sandbox *apiv1alpha2.Sandbox, observed *fastletapi.SandboxStatus) *fastpathv2.SandboxInfo {
	info := &fastpathv2.SandboxInfo{
		Identity: protoIdentity(sandbox), AppliedGeneration: observed.AppliedGeneration,
		Runtime:   &fastpathv2.RuntimeInfo{State: protoRuntimeState(observed.Runtime.State)},
		DataPlane: &fastpathv2.DataPlaneInfo{State: protoDataPlaneState(observed.DataPlane.State)},
	}
	for _, diagnostic := range observed.InfraComponents {
		state := fastpathv2.InfraComponentState_INFRA_COMPONENT_STATE_STARTING
		switch diagnostic.State {
		case "Ready":
			state = fastpathv2.InfraComponentState_INFRA_COMPONENT_STATE_READY
		case "Failed":
			state = fastpathv2.InfraComponentState_INFRA_COMPONENT_STATE_FAILED
		}
		info.InfraComponents = append(info.InfraComponents, &fastpathv2.InfraComponentInfo{Name: diagnostic.Component, State: state, Message: diagnostic.Message})
	}
	for _, binding := range observed.ActionBindings {
		state := fastpathv2.ActionState_ACTION_STATE_PENDING
		switch binding.State {
		case "Applying":
			state = fastpathv2.ActionState_ACTION_STATE_APPLYING
		case "Ready":
			state = fastpathv2.ActionState_ACTION_STATE_READY
		case "Failed":
			state = fastpathv2.ActionState_ACTION_STATE_FAILED
		}
		transition := int64(0)
		if !binding.LastTransitionTime.IsZero() {
			transition = binding.LastTransitionTime.Unix()
		}
		info.ActionBindings = append(info.ActionBindings, &fastpathv2.ActionBindingInfo{
			Handler: binding.Handler, State: state, LastTransitionUnixSeconds: transition, Message: binding.Message,
		})
	}
	info.Ready = liveOverallReady(info, sandbox)
	return info
}

func liveOverallReady(info *fastpathv2.SandboxInfo, sandbox *apiv1alpha2.Sandbox) bool {
	if info == nil || sandbox == nil || info.AppliedGeneration != sandbox.Generation || info.Runtime == nil || info.Runtime.State != fastpathv2.RuntimeState_RUNTIME_STATE_READY ||
		info.DataPlane == nil || info.DataPlane.State != fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY {
		return false
	}
	if len(info.ActionBindings) != len(sandbox.Spec.ActionBindings) {
		return false
	}
	for _, component := range info.InfraComponents {
		if component.State != fastpathv2.InfraComponentState_INFRA_COMPONENT_STATE_READY {
			return false
		}
	}
	for index, binding := range info.ActionBindings {
		if binding.Handler != sandbox.Spec.ActionBindings[index].Handler || binding.State != fastpathv2.ActionState_ACTION_STATE_READY {
			return false
		}
	}
	return true
}

func protoRuntimeState(state fastletapi.RuntimeState) fastpathv2.RuntimeState {
	switch state {
	case fastletapi.RuntimeStatePending:
		return fastpathv2.RuntimeState_RUNTIME_STATE_PENDING
	case fastletapi.RuntimeStateCreating:
		return fastpathv2.RuntimeState_RUNTIME_STATE_CREATING
	case fastletapi.RuntimeStateReady:
		return fastpathv2.RuntimeState_RUNTIME_STATE_READY
	case fastletapi.RuntimeStateStopping:
		return fastpathv2.RuntimeState_RUNTIME_STATE_STOPPING
	case fastletapi.RuntimeStateStopped:
		return fastpathv2.RuntimeState_RUNTIME_STATE_STOPPED
	case fastletapi.RuntimeStateFailed:
		return fastpathv2.RuntimeState_RUNTIME_STATE_FAILED
	case fastletapi.RuntimeStateUnavailable:
		return fastpathv2.RuntimeState_RUNTIME_STATE_UNAVAILABLE
	default:
		return fastpathv2.RuntimeState_RUNTIME_STATE_UNKNOWN
	}
}

func protoDataPlaneState(state fastletapi.DataPlaneState) fastpathv2.DataPlaneState {
	switch state {
	case fastletapi.DataPlaneStatePending:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_PENDING
	case fastletapi.DataPlaneStatePublishing:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_PUBLISHING
	case fastletapi.DataPlaneStateReady:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY
	case fastletapi.DataPlaneStateDraining:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_DRAINING
	case fastletapi.DataPlaneStateFailed:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_FAILED
	case fastletapi.DataPlaneStateUnavailable:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_UNAVAILABLE
	default:
		return fastpathv2.DataPlaneState_DATA_PLANE_STATE_UNKNOWN
	}
}

func (s *Server) sandboxFromReference(ctx context.Context, reference *fastpathv2.SandboxReference) (*apiv1alpha2.Sandbox, error) {
	return s.sandboxFromReferenceAtGeneration(ctx, reference, 0)
}

func (s *Server) sandboxFromReferenceAtGeneration(ctx context.Context, reference *fastpathv2.SandboxReference, expectedGeneration int64) (*apiv1alpha2.Sandbox, error) {
	if reference == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	if expectedGeneration < 0 {
		return nil, status.Error(codes.InvalidArgument, "expected_generation cannot be negative")
	}
	if reference.NamespacedName == nil || reference.NamespacedName.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "namespaced_name.name is required")
	}
	namespace := reference.NamespacedName.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	sandbox, err := s.findSandboxByName(ctx, client.ObjectKey{Namespace: namespace, Name: reference.NamespacedName.Name}, expectedGeneration)
	if err != nil {
		return nil, err
	}
	if err := checkReferenceUID(sandbox, reference.ExpectedUid); err != nil {
		return nil, err
	}
	return sandbox, nil
}

func (s *Server) findSandboxByName(ctx context.Context, key client.ObjectKey, expectedGeneration int64) (*apiv1alpha2.Sandbox, error) {
	if s.RouteCache != nil {
		var cached apiv1alpha2.Sandbox
		err := s.RouteCache.Get(ctx, key, &cached)
		if err == nil && cached.Generation >= expectedGeneration {
			return &cached, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, grpcKubernetesError(err)
		}
	}
	var sandbox apiv1alpha2.Sandbox
	if err := s.K8sClient.Get(ctx, key, &sandbox); err != nil {
		return nil, grpcKubernetesError(err)
	}
	return &sandbox, nil
}

func (s *Server) resolveEndpointTarget(ctx context.Context, sandbox *apiv1alpha2.Sandbox, target *fastpathv2.EndpointTarget) (uint32, string, string, error) {
	pool, err := s.getPool(ctx, sandbox.Namespace, sandbox.Spec.PoolRef)
	if err != nil {
		return 0, "", "", err
	}
	switch value := target.Target.(type) {
	case *fastpathv2.EndpointTarget_ComponentName:
		for _, component := range pool.Spec.InfraComponents {
			if component.Name == value.ComponentName {
				return uint32(component.Endpoint.Port), component.Name, component.Endpoint.Protocol, nil
			}
		}
		return 0, "", "", status.Errorf(codes.NotFound, "Infra Component %q is not declared", value.ComponentName)
	case *fastpathv2.EndpointTarget_Port:
		if value.Port == 0 || value.Port > 65535 {
			return 0, "", "", status.Error(codes.InvalidArgument, "port must be between 1 and 65535")
		}
		for _, component := range pool.Spec.InfraComponents {
			if uint32(component.Endpoint.Port) == value.Port {
				return 0, "", "", status.Errorf(codes.FailedPrecondition, "port %d belongs to Infra Component %s; resolve by name", value.Port, component.Name)
			}
		}
		return value.Port, "", "HTTP", nil
	default:
		return 0, "", "", status.Error(codes.InvalidArgument, "unknown endpoint target")
	}
}

func (s *Server) issueRouteCredential(ctx context.Context, sandbox *apiv1alpha2.Sandbox, targetPort uint32, componentName, protocol string) (string, routeauth.Claims, error) {
	if s.CredentialIssuer == nil {
		return "", routeauth.Claims{}, status.Error(codes.FailedPrecondition, "route credential issuer is not configured")
	}
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil || envelope == nil {
		return "", routeauth.Claims{}, status.Error(codes.Unavailable, "Sandbox assignment is not ready")
	}
	claims := routeauth.Claims{
		Namespace: sandbox.Namespace, SandboxUID: string(sandbox.UID), TargetPort: targetPort,
		TargetKind: routeauth.TargetKindPort, Protocol: protocol, FastletPodUID: envelope.FastletPodUID,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration,
	}
	if componentName != "" {
		claims.TargetKind, claims.ComponentName = routeauth.TargetKindComponent, componentName
	}
	credential, claims, err := s.CredentialIssuer.Issue(claims)
	if err != nil {
		return "", routeauth.Claims{}, status.Errorf(codes.Internal, "issue route credential: %v", err)
	}
	return credential, claims, nil
}

func sandboxFromCreateRequest(request *fastpathv2.CreateSandboxRequest, bindings []apiv1alpha2.ActionBinding, createSpecHash string) *apiv1alpha2.Sandbox {
	environment := make([]corev1.EnvVar, 0, len(request.Envs))
	for name, value := range request.Envs {
		environment = append(environment, corev1.EnvVar{Name: name, Value: value})
	}
	sort.Slice(environment, func(i, j int) bool { return environment[i].Name < environment[j].Name })
	labels := map[string]string{assignment.LabelCreatedBy: "fastpath"}
	for name, value := range request.Metadata {
		labels[metadataLabelKey(name)] = value
	}
	var expiresAt *metav1.Time
	if request.ExpiresAtUnixSeconds > 0 {
		value := metav1.NewTime(time.Unix(request.ExpiresAtUnixSeconds, 0).UTC())
		expiresAt = &value
	}
	recoveryTimeout := request.RecoveryTimeoutSeconds
	if recoveryTimeout == 0 {
		recoveryTimeout = 60
	}
	return &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.RequestId, Namespace: request.Namespace, Labels: labels,
			Annotations: map[string]string{assignment.AnnotationRequestID: request.RequestId, assignment.AnnotationCreateSpecHash: createSpecHash},
		},
		Spec: apiv1alpha2.SandboxSpec{
			Image: request.Image, PoolRef: request.PoolRef, Command: request.Command, Args: request.Args,
			Envs: environment, WorkingDir: request.WorkingDir, ExpireTime: expiresAt,
			FailurePolicy: toFailurePolicy(request.FailurePolicy), RecoveryTimeoutSeconds: recoveryTimeout,
			ActionBindings: bindings,
		},
	}
}

func apiActionBindings(bindings []*fastpathv2.ActionBinding) ([]apiv1alpha2.ActionBinding, error) {
	if len(bindings) > 16 {
		return nil, errors.New("at most 16 Action Bindings are allowed")
	}
	seen := make(map[string]struct{}, len(bindings))
	result := make([]apiv1alpha2.ActionBinding, 0, len(bindings))
	total := 0
	for _, binding := range bindings {
		if binding == nil || binding.Handler == "" {
			return nil, errors.New("Action Binding Handler is required")
		}
		if problems := kvalidation.IsDNS1123Label(binding.Handler); len(problems) > 0 {
			return nil, fmt.Errorf("Action Binding Handler %q is invalid: %s", binding.Handler, problems[0])
		}
		if _, found := seen[binding.Handler]; found {
			return nil, fmt.Errorf("duplicate Action Binding Handler %q", binding.Handler)
		}
		seen[binding.Handler] = struct{}{}
		if len(binding.Input) > apiv1alpha2.MaxActionBindingInputBytes {
			return nil, fmt.Errorf("Action Binding %s input exceeds %d bytes", binding.Handler, apiv1alpha2.MaxActionBindingInputBytes)
		}
		total += len(binding.Input)
		if total > apiv1alpha2.MaxSandboxActionBindingInputBytes {
			return nil, fmt.Errorf("Action Binding inputs exceed %d bytes", apiv1alpha2.MaxSandboxActionBindingInputBytes)
		}
		result = append(result, apiv1alpha2.ActionBinding{Handler: binding.Handler, Input: binding.Input})
	}
	return result, nil
}

func validateCreateOptions(request *fastpathv2.CreateSandboxRequest, now time.Time) error {
	if request.ExpiresAtUnixSeconds < 0 || request.ExpiresAtUnixSeconds > 0 && !time.Unix(request.ExpiresAtUnixSeconds, 0).After(now) {
		return errors.New("expires_at_unix_seconds must be zero or in the future")
	}
	if request.RecoveryTimeoutSeconds < 0 || request.RecoveryTimeoutSeconds > 86400 {
		return errors.New("recovery_timeout_seconds must be between 0 and 86400")
	}
	if request.FailurePolicy != fastpathv2.FailurePolicy_MANUAL && request.FailurePolicy != fastpathv2.FailurePolicy_AUTO_RECREATE {
		return errors.New("failure_policy is invalid")
	}
	if _, err := apiActionBindings(request.ActionBindings); err != nil {
		return err
	}
	return validateMetadata(request.Metadata)
}

func (s *Server) getPool(ctx context.Context, namespace, name string) (*apiv1alpha2.SandboxPool, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if s.RouteCache != nil {
		var cached apiv1alpha2.SandboxPool
		if err := s.RouteCache.Get(ctx, key, &cached); err == nil {
			return &cached, nil
		} else if !apierrors.IsNotFound(err) {
			return nil, grpcKubernetesError(err)
		}
	}
	var pool apiv1alpha2.SandboxPool
	if err := s.K8sClient.Get(ctx, key, &pool); err != nil {
		return nil, grpcKubernetesError(err)
	}
	return &pool, nil
}

func validateMetadata(metadata map[string]string) error {
	for name, value := range metadata {
		if problems := kvalidation.IsDNS1123Label(name); len(problems) > 0 {
			return fmt.Errorf("metadata key %q must be a DNS label: %s", name, problems[0])
		}
		if problems := kvalidation.IsValidLabelValue(value); len(problems) > 0 {
			return fmt.Errorf("metadata value for %q is invalid: %s", name, problems[0])
		}
	}
	return nil
}

func checkExpectedGeneration(sandbox *apiv1alpha2.Sandbox, expected int64) error {
	if expected < 0 {
		return status.Error(codes.InvalidArgument, "expected_generation cannot be negative")
	}
	if expected > 0 && sandbox.Generation != expected {
		return status.Errorf(codes.Aborted, "Sandbox generation changed: expected %d, current %d", expected, sandbox.Generation)
	}
	return nil
}

func checkReferenceUID(sandbox *apiv1alpha2.Sandbox, expectedUID string) error {
	if expectedUID != "" && string(sandbox.UID) != expectedUID {
		return status.Errorf(codes.Aborted, "Sandbox UID changed: expected %s, current %s", expectedUID, sandbox.UID)
	}
	return nil
}

func checkGenerationFloor(sandbox *apiv1alpha2.Sandbox, expected int64) error {
	if expected < 0 {
		return status.Error(codes.InvalidArgument, "expected_generation cannot be negative")
	}
	if expected > 0 && sandbox.Generation < expected {
		return status.Errorf(codes.Aborted, "Sandbox generation has not reached the expected floor: expected at least %d, current %d", expected, sandbox.Generation)
	}
	return nil
}

func compatibleCandidates(candidates []placement.FastletInfo, revision string) []placement.FastletInfo {
	if revision == "" {
		return candidates
	}
	result := make([]placement.FastletInfo, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.FastletRevision == revision {
			result = append(result, candidate)
		}
	}
	return result
}

func componentReady(components []*fastpathv2.InfraComponentInfo, name string) bool {
	for _, component := range components {
		if component.Name == name {
			return component.State == fastpathv2.InfraComponentState_INFRA_COMPONENT_STATE_READY
		}
	}
	return false
}

func protoIdentity(sandbox *apiv1alpha2.Sandbox) *fastpathv2.SandboxIdentity {
	return &fastpathv2.SandboxIdentity{Uid: string(sandbox.UID), Name: sandbox.Name, Namespace: sandbox.Namespace}
}

func internalIdentity(sandbox *apiv1alpha2.Sandbox, envelope *assignment.AssignmentEnvelope) fastletapi.SandboxIdentity {
	return fastletapi.SandboxIdentity{
		SandboxUID: string(sandbox.UID), Namespace: sandbox.Namespace, Name: sandbox.Name,
		FastletPodUID: envelope.FastletPodUID, InstanceGeneration: envelope.InstanceGeneration,
		RuntimeInstanceID: envelope.RuntimeInstanceID, AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration,
	}
}

func identityFromSandbox(sandbox *apiv1alpha2.Sandbox, targetPort uint32) observability.Identity {
	identity := observability.Identity{TargetPort: targetPort}
	if sandbox == nil {
		return identity
	}
	identity.RequestID = sandbox.Annotations[assignment.AnnotationRequestID]
	identity.Namespace, identity.SandboxName, identity.SandboxUID = sandbox.Namespace, sandbox.Name, string(sandbox.UID)
	identity.InstanceGeneration, identity.RouteGeneration = sandbox.Status.Runtime.Generation, sandbox.Status.DataPlane.RouteGeneration
	identity.FastletPodUID, identity.AssignmentAttempt = string(sandbox.Status.Placement.FastletPodUID), sandbox.Status.Placement.Attempt
	return identity
}

func metadataLabelKey(name string) string { return metadataLabelPrefix + name }

func parseCreateCompletion(value fastpathv2.CreateCompletion) (createCompletion, error) {
	switch value {
	case fastpathv2.CreateCompletion_CREATE_COMPLETION_UNSPECIFIED, fastpathv2.CreateCompletion_CREATE_COMPLETION_READY:
		return createCompletion{api: fastpathv2.CreateCompletion_CREATE_COMPLETION_READY, fastlet: fastletapi.CreateCompletionReady}, nil
	case fastpathv2.CreateCompletion_CREATE_COMPLETION_RUNTIME_READY:
		return createCompletion{api: value, fastlet: fastletapi.CreateCompletionRuntimeReady}, nil
	default:
		return createCompletion{}, status.Errorf(codes.InvalidArgument, "completion %d is invalid", value)
	}
}

func toFailurePolicy(policy fastpathv2.FailurePolicy) apiv1alpha2.FailurePolicy {
	if policy == fastpathv2.FailurePolicy_AUTO_RECREATE {
		return apiv1alpha2.FailurePolicyAutoRecreate
	}
	return apiv1alpha2.FailurePolicyManual
}

func appendDiagnosticError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func grpcKubernetesError(err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return status.Error(codes.NotFound, err.Error())
	case apierrors.IsAlreadyExists(err):
		return status.Error(codes.AlreadyExists, err.Error())
	case apierrors.IsConflict(err):
		return status.Error(codes.Aborted, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *Server) orchestrator() (*orchestration.Orchestrator, error) {
	if s.Orchestrator == nil {
		return nil, errors.New("Sandbox orchestrator is not configured")
	}
	return s.Orchestrator, nil
}

func (s *Server) defaultNamespace() string {
	if s.DefaultNamespace != "" {
		return s.DefaultNamespace
	}
	return "fast-sandbox"
}
