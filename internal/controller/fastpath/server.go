package fastpath

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/controller/sandboxorchestrator"
	"fast-sandbox/internal/fastletproxy"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/routeauth"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	fastpathv1.UnimplementedFastPathServiceServer
	K8sClient         client.Client
	RouteCache        client.Client
	Orchestrator      *sandboxorchestrator.Orchestrator
	DiagnosticsClient interface {
		SandboxDiagnostics(context.Context, string, *api.SandboxDiagnosticsRequest) (*api.SandboxDiagnosticsResponse, error)
	}
	CredentialIssuer    *routeauth.Issuer
	SandboxProxyBaseURL string
}

var _ fastpathv1.FastPathServiceServer = &Server{}

func (s *Server) ResolveEndpoint(ctx context.Context, request *fastpathv1.ResolveEndpointRequest) (*fastpathv1.ResolveEndpointResponse, error) {
	if request == nil || request.SandboxUid == "" || request.TargetPort == 0 || request.TargetPort > 65535 {
		return nil, status.Error(codes.InvalidArgument, "sandbox_uid and target_port between 1 and 65535 are required")
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxUID: request.SandboxUid, TargetPort: request.TargetPort})
	protocol := strings.ToLower(request.Protocol)
	if protocol == "" {
		protocol = "http"
	}
	if protocol != "http" {
		return nil, status.Error(codes.Unimplemented, "the initial transparent proxy supports HTTP/SSE/WebSocket over HTTP only")
	}
	credential, claims, err := s.issueRouteCredential(ctx, request.SandboxUid, request.TargetPort)
	if err != nil {
		return nil, err
	}
	if s.SandboxProxyBaseURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox Proxy base URL is not configured")
	}
	return &fastpathv1.ResolveEndpointResponse{
		SandboxUid: request.SandboxUid, TargetPort: request.TargetPort,
		ProxyEndpoint:   strings.TrimRight(s.SandboxProxyBaseURL, "/") + fastletproxy.RoutePath(request.SandboxUid, request.TargetPort),
		RequiredHeaders: map[string]string{"Authorization": "Bearer " + credential},
		RouteGeneration: claims.RouteGeneration, ExpiresAtUnixSeconds: claims.ExpiresAt,
	}, nil
}

func (s *Server) IssueRouteCredential(ctx context.Context, request *fastpathv1.IssueRouteCredentialRequest) (*fastpathv1.IssueRouteCredentialResponse, error) {
	if request == nil || request.SandboxUid == "" || request.TargetPort == 0 || request.TargetPort > 65535 {
		return nil, status.Error(codes.InvalidArgument, "sandbox_uid and target_port between 1 and 65535 are required")
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxUID: request.SandboxUid, TargetPort: request.TargetPort})
	credential, claims, err := s.issueRouteCredential(ctx, request.SandboxUid, request.TargetPort)
	if err != nil {
		return nil, err
	}
	return &fastpathv1.IssueRouteCredentialResponse{
		Credential: credential, ExpiresAtUnixSeconds: claims.ExpiresAt, RouteGeneration: claims.RouteGeneration,
	}, nil
}

func (s *Server) issueRouteCredential(ctx context.Context, sandboxUID string, targetPort uint32) (string, routeauth.Claims, error) {
	if s.CredentialIssuer == nil {
		return "", routeauth.Claims{}, status.Error(codes.FailedPrecondition, "route credential issuer is not configured")
	}
	sandbox, err := s.findSandboxByUID(ctx, sandboxUID)
	if err != nil {
		return "", routeauth.Claims{}, err
	}
	if sandbox.Status.Assignment == nil || sandbox.Status.RuntimeState != apiv1alpha1.ObservedStateReady ||
		sandbox.Status.DataPlaneState != apiv1alpha1.ObservedStateReady {
		return "", routeauth.Claims{}, status.Error(codes.Unavailable, "Sandbox data plane is not ready")
	}
	ctx = observability.WithIdentity(ctx, identityFromSandbox(sandbox, targetPort))
	routeGeneration := sandbox.Status.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	credential, claims, err := s.CredentialIssuer.Issue(routeauth.Claims{
		Namespace: sandbox.Namespace, SandboxUID: string(sandbox.UID), TargetPort: targetPort,
		FastletPodUID: sandbox.Status.Assignment.FastletPodUID, AssignmentAttempt: sandbox.Status.Assignment.Attempt,
		RouteGeneration: routeGeneration,
	})
	if err != nil {
		return "", routeauth.Claims{}, status.Errorf(codes.Internal, "issue route credential: %v", err)
	}
	return credential, claims, nil
}

func (s *Server) findSandboxByUID(ctx context.Context, sandboxUID string) (*apiv1alpha1.Sandbox, error) {
	if s.RouteCache != nil {
		var cached apiv1alpha1.SandboxList
		if err := s.RouteCache.List(ctx, &cached, client.MatchingFields{SandboxUIDIndexField: sandboxUID}); err != nil {
			return nil, status.Errorf(codes.Internal, "read Sandbox UID index: %v", err)
		}
		if len(cached.Items) == 1 {
			return cached.Items[0].DeepCopy(), nil
		}
		if len(cached.Items) > 1 {
			return nil, status.Error(codes.Internal, "Sandbox UID index returned duplicate objects")
		}
	}
	var list apiv1alpha1.SandboxList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, status.Errorf(codes.Internal, "list Sandboxes: %v", err)
	}
	for index := range list.Items {
		if string(list.Items[index].UID) == sandboxUID {
			return list.Items[index].DeepCopy(), nil
		}
	}
	return nil, status.Error(codes.NotFound, "Sandbox UID not found")
}

const SandboxUIDIndexField = "metadata.uid"

func (s *Server) CreateSandbox(ctx context.Context, request *fastpathv1.CreateRequest) (_ *fastpathv1.CreateResponse, resultErr error) {
	started := time.Now()
	acceptedObserved := false
	defer func() {
		success := "true"
		if resultErr != nil {
			success = "false"
		}
		createSandboxDuration.WithLabelValues("v2", success).Observe(time.Since(started).Seconds())
		createDataPlaneReadyLatency.WithLabelValues(grpcMetricResult(resultErr)).Observe(time.Since(started).Seconds())
		if !acceptedObserved {
			observeCreateAccepted("rejected", started, resultErr)
		}
	}()

	if request == nil || request.Image == "" || request.PoolRef == "" {
		return nil, status.Error(codes.InvalidArgument, "image and pool_ref are required")
	}
	if request.Namespace == "" {
		request.Namespace = "default"
	}
	if request.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required")
	}
	if err := ValidateRequestID(request.RequestId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if request.Name != "" && request.Name != request.RequestId {
		return nil, status.Error(codes.InvalidArgument, "name and request_id must be identical")
	}
	request.Name = request.RequestId
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: request.RequestId, Namespace: request.Namespace, SandboxName: request.Name,
	})
	createSpecHash, err := CreateSpecHash(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hash create request: %v", err)
	}
	orchestrator, err := s.orchestrator()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	sandbox := sandboxFromCreateRequest(request, createSpecHash)
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxName: sandbox.Name})
	candidates, err := orchestrator.FastPathCandidates(sandbox, request.RequestId)
	if err != nil {
		if errors.Is(err, sandboxorchestrator.ErrNoCandidate) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	runtimeInstanceID, err := idgen.GenerateRequestID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate runtime instance ID: %v", err)
	}
	envelope, err := sandboxorchestrator.AssignmentForCandidate(candidates[0], 1, apiv1alpha1.InitialInstanceGeneration, 1, runtimeInstanceID)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid Fastlet candidate: %v", err)
	}
	if err := common.SetAssignmentAnnotation(sandbox, envelope); err != nil {
		return nil, status.Errorf(codes.Internal, "encode assignment: %v", err)
	}

	// IO 1: CRD Create. The happy path does not preflight with a Get/List.
	if createErr := s.K8sClient.Create(ctx, sandbox); createErr != nil {
		var existing apiv1alpha1.Sandbox
		getErr := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), &existing)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) && !apierrors.IsAlreadyExists(createErr) {
				return nil, createErr
			}
			return nil, errors.Join(createErr, getErr)
		}
		if existing.Annotations[common.AnnotationRequestID] != request.RequestId || existing.Annotations[common.AnnotationCreateSpecHash] != createSpecHash {
			return nil, status.Errorf(codes.AlreadyExists, "Sandbox name %q belongs to another create intent", sandbox.Name)
		}
		sandbox = existing.DeepCopy()
		existingEnvelope, envelopeErr := common.AssignmentFromAnnotation(sandbox)
		if envelopeErr != nil || existingEnvelope == nil {
			return nil, status.Errorf(codes.Unavailable, "existing Sandbox assignment is not ready: %v", envelopeErr)
		}
		envelope = *existingEnvelope
		selected, ok := orchestrator.Registry.GetFastletByID(fastletpool.FastletID(envelope.FastletName))
		if !ok {
			return nil, status.Error(codes.Unavailable, sandboxorchestrator.ErrAssignedFastletUnavailable.Error())
		}
		candidates = []fastletpool.FastletInfo{selected}
		observeCreateAccepted("idempotent", started, nil)
	} else {
		observeCreateAccepted("crd", started, nil)
	}
	acceptedObserved = true
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: request.RequestId, Namespace: sandbox.Namespace, SandboxName: sandbox.Name, SandboxUID: string(sandbox.UID),
		FastletPodUID: envelope.FastletPodUID, InstanceGeneration: envelope.InstanceGeneration,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration,
	})

	for index, candidate := range candidates {
		if index > 0 {
			sandboxorchestrator.RecordTopKRetry("attempt")
			runtimeInstanceID, err = idgen.GenerateRequestID()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "generate runtime instance ID: %v", err)
			}
			next, nextErr := sandboxorchestrator.AssignmentForCandidate(candidate, envelope.Attempt+1, envelope.InstanceGeneration, envelope.RouteGeneration+1, runtimeInstanceID)
			if nextErr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "invalid Fastlet candidate: %v", nextErr)
			}
			sandbox, err = common.CASAssignmentAnnotation(ctx, s.K8sClient, client.ObjectKeyFromObject(sandbox), envelope, next)
			if err != nil {
				return nil, status.Errorf(codes.Aborted, "assignment changed concurrently: %v", err)
			}
			envelope = next
		}

		// IO 2 on the happy path: one atomic Fastlet admission/create call.
		_, createErr := orchestrator.CreateRuntimeOnCandidate(ctx, sandbox, candidate, envelope)
		if createErr == nil {
			if index > 0 {
				sandboxorchestrator.RecordTopKRetry("accepted")
			}
			return createResponseFromSandbox(sandbox, &envelope), nil
		}
		if sandboxorchestrator.IsCandidateRejection(createErr) && index+1 < len(candidates) {
			sandboxorchestrator.RecordTopKRetry("candidate_rejected")
			orchestrator.RecordCandidateFeedback(candidate.ID, createErr)
			continue
		}
		if sandboxorchestrator.IsCandidateRejection(createErr) {
			return nil, status.Errorf(codes.ResourceExhausted, "all Fastlet candidates rejected admission: %v", createErr)
		}
		return nil, status.Errorf(codes.Unavailable, "Sandbox intent is persisted and Controller will retry the same runtime identity: %v", createErr)
	}
	return nil, status.Error(codes.ResourceExhausted, sandboxorchestrator.ErrNoCandidate.Error())
}

func sandboxFromCreateRequest(request *fastpathv1.CreateRequest, createSpecHash string) *apiv1alpha1.Sandbox {
	environment := make([]corev1.EnvVar, 0, len(request.Envs))
	for name, value := range request.Envs {
		environment = append(environment, corev1.EnvVar{Name: name, Value: value})
	}
	return &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.RequestId, Namespace: request.Namespace,
			Annotations: map[string]string{
				common.AnnotationRequestID: request.RequestId, common.AnnotationCreateSpecHash: createSpecHash,
			},
			Labels: map[string]string{common.LabelCreatedBy: "fastpath"},
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image: request.Image, PoolRef: request.PoolRef,
			Command: request.Command, Args: request.Args, Envs: environment, WorkingDir: request.WorkingDir,
		},
	}
}

func (s *Server) orchestrator() (*sandboxorchestrator.Orchestrator, error) {
	if s.Orchestrator == nil {
		return nil, errors.New("Sandbox orchestrator is not configured")
	}
	return s.Orchestrator, nil
}

func createResponseFromSandbox(sandbox *apiv1alpha1.Sandbox, envelope *common.AssignmentEnvelope) *fastpathv1.CreateResponse {
	fastletName := ""
	if envelope != nil {
		fastletName = envelope.FastletName
	} else if sandbox.Status.Assignment != nil {
		fastletName = sandbox.Status.Assignment.FastletName
	}
	return &fastpathv1.CreateResponse{
		SandboxUid: string(sandbox.UID), SandboxName: sandbox.Name, FastletPod: fastletName,
	}
}

func (s *Server) ListSandboxes(ctx context.Context, request *fastpathv1.ListRequest) (*fastpathv1.ListResponse, error) {
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace})
	var list apiv1alpha1.SandboxList
	if err := s.K8sClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	response := &fastpathv1.ListResponse{Items: make([]*fastpathv1.SandboxInfo, 0, len(list.Items))}
	for index := range list.Items {
		response.Items = append(response.Items, sandboxInfo(&list.Items[index]))
	}
	return response, nil
}

func (s *Server) GetSandbox(ctx context.Context, request *fastpathv1.GetRequest) (*fastpathv1.SandboxInfo, error) {
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	var sandbox apiv1alpha1.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: request.SandboxName, Namespace: namespace}, &sandbox); err != nil {
		return nil, err
	}
	return sandboxInfo(&sandbox), nil
}

func (s *Server) GetSandboxDiagnostics(ctx context.Context, request *fastpathv1.SandboxDiagnosticsRequest) (*fastpathv1.SandboxDiagnosticsResponse, error) {
	if request == nil || request.SandboxName == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_name is required")
	}
	if request.Limit < 0 || request.Limit > 128 {
		return nil, status.Error(codes.InvalidArgument, "limit must be between 0 and 128")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	var sandbox apiv1alpha1.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: request.SandboxName, Namespace: namespace}, &sandbox); err != nil {
		return nil, err
	}
	response := &fastpathv1.SandboxDiagnosticsResponse{Sandbox: sandboxInfo(&sandbox)}
	envelope, annotationErr := common.AssignmentFromAnnotation(&sandbox)
	if annotationErr != nil {
		response.AssignmentState = "invalid-annotation"
		response.FastletError = annotationErr.Error()
		return response, nil
	}
	if envelope == nil {
		response.AssignmentState = "unassigned"
		response.FastletError = "Sandbox has no durable assignment annotation"
		return response, nil
	}
	response.AssignmentState = "annotation-authoritative"
	if _, projectionErr := common.EffectiveAssignment(&sandbox); projectionErr != nil {
		response.AssignmentState = "status-projection-conflict"
		response.FastletError = projectionErr.Error()
	} else if sandbox.Status.Assignment == nil {
		response.AssignmentState = "status-projection-pending"
	} else {
		response.AssignmentState = "synchronized"
	}
	response.RuntimeInstanceId = envelope.RuntimeInstanceID
	response.AssignmentAttempt = envelope.Attempt

	if s.Orchestrator == nil || s.Orchestrator.Registry == nil || s.DiagnosticsClient == nil {
		response.FastletError = appendDiagnosticError(response.FastletError, "Fastlet diagnostics client is not configured")
		return response, nil
	}
	fastlet, found := s.Orchestrator.Registry.GetFastletByID(fastletpool.FastletID(envelope.FastletName))
	if !found {
		response.FastletError = appendDiagnosticError(response.FastletError, "assigned Fastlet is absent from the local registry")
		return response, nil
	}
	if fastlet.PodUID != envelope.FastletPodUID {
		response.FastletError = appendDiagnosticError(response.FastletError, "assigned Fastlet Pod UID was replaced")
		return response, nil
	}
	identity := api.SandboxIdentity{
		RequestID: sandbox.Annotations[common.AnnotationRequestID], SandboxUID: string(sandbox.UID),
		FastletPodUID: envelope.FastletPodUID, InstanceGeneration: envelope.InstanceGeneration,
		RuntimeInstanceID: envelope.RuntimeInstanceID, AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration,
	}
	fastletResponse, err := s.DiagnosticsClient.SandboxDiagnostics(ctx, fastlet.PodIP, &api.SandboxDiagnosticsRequest{
		Identity: identity, Limit: int(request.Limit),
	})
	if err != nil {
		response.FastletError = appendDiagnosticError(response.FastletError, err.Error())
		return response, nil
	}
	response.FastletReachable = true
	for _, event := range fastletResponse.Events {
		response.Events = append(response.Events, &fastpathv1.SandboxDiagnosticEvent{
			TimestampUnixNano: event.Timestamp.UnixNano(), Level: event.Level,
			Source: event.Source, Phase: event.Phase, Message: event.Message,
		})
	}
	return response, nil
}

func appendDiagnosticError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func sandboxInfo(sandbox *apiv1alpha1.Sandbox) *fastpathv1.SandboxInfo {
	fastletName := ""
	if sandbox.Status.Assignment != nil {
		fastletName = sandbox.Status.Assignment.FastletName
	}
	return &fastpathv1.SandboxInfo{
		SandboxUid: string(sandbox.UID), SandboxName: sandbox.Name,
		RuntimeState: string(sandbox.Status.RuntimeState), DataPlaneState: string(sandbox.Status.DataPlaneState),
		UserProcessState: string(sandbox.Status.UserProcessState), FastletPod: fastletName,
		Image: sandbox.Spec.Image, PoolRef: sandbox.Spec.PoolRef,
		CreatedAt: sandbox.CreationTimestamp.Unix(),
	}
}

func identityFromSandbox(sandbox *apiv1alpha1.Sandbox, targetPort uint32) observability.Identity {
	if sandbox == nil {
		return observability.Identity{TargetPort: targetPort}
	}
	identity := observability.Identity{
		RequestID: sandbox.Annotations[common.AnnotationRequestID], Namespace: sandbox.Namespace, SandboxName: sandbox.Name,
		SandboxUID: string(sandbox.UID), InstanceGeneration: sandbox.Status.InstanceGeneration,
		RouteGeneration: sandbox.Status.RouteGeneration, TargetPort: targetPort,
	}
	if sandbox.Status.Assignment != nil {
		identity.FastletPodUID = sandbox.Status.Assignment.FastletPodUID
		identity.AssignmentAttempt = sandbox.Status.Assignment.Attempt
	}
	return identity
}

// Delete only submits desired state. Finalizer reconciliation owns runtime cleanup.
func (s *Server) DeleteSandbox(ctx context.Context, request *fastpathv1.DeleteRequest) (*fastpathv1.DeleteResponse, error) {
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	sandbox := &apiv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: request.SandboxName, Namespace: namespace}}
	if err := s.K8sClient.Delete(ctx, sandbox); err != nil && !apierrors.IsNotFound(err) {
		return &fastpathv1.DeleteResponse{Success: false}, err
	}
	return &fastpathv1.DeleteResponse{Success: true}, nil
}

// Update only commits declarative intent; the Controller observes and reconciles it.
func (s *Server) UpdateSandbox(ctx context.Context, request *fastpathv1.UpdateRequest) (*fastpathv1.UpdateResponse, error) {
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	key := client.ObjectKey{Name: request.SandboxName, Namespace: namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var sandbox apiv1alpha1.Sandbox
		if err := s.K8sClient.Get(ctx, key, &sandbox); err != nil {
			return err
		}
		switch value := request.Update.(type) {
		case *fastpathv1.UpdateRequest_ExpireTimeSeconds:
			if value.ExpireTimeSeconds == 0 {
				sandbox.Spec.ExpireTime = nil
			} else {
				expiresAt := metav1.NewTime(time.Unix(value.ExpireTimeSeconds, 0))
				sandbox.Spec.ExpireTime = &expiresAt
			}
		case *fastpathv1.UpdateRequest_ResetRevision:
			parsed, err := time.Parse(time.RFC3339Nano, value.ResetRevision)
			if err != nil {
				return fmt.Errorf("invalid reset_revision: %w", err)
			}
			sandbox.Spec.ResetRevision = &metav1.Time{Time: parsed}
		case *fastpathv1.UpdateRequest_FailurePolicy:
			sandbox.Spec.FailurePolicy = toFailurePolicy(value.FailurePolicy)
		case *fastpathv1.UpdateRequest_RecoveryTimeoutSeconds:
			sandbox.Spec.RecoveryTimeoutSeconds = value.RecoveryTimeoutSeconds
		}
		if sandbox.Labels == nil {
			sandbox.Labels = make(map[string]string)
		}
		for name, value := range request.Labels {
			sandbox.Labels[name] = value
		}
		return s.K8sClient.Update(ctx, &sandbox)
	})
	if err != nil {
		return &fastpathv1.UpdateResponse{Success: false, Message: err.Error()}, nil
	}
	var updated apiv1alpha1.Sandbox
	if err := s.K8sClient.Get(ctx, key, &updated); err != nil {
		return nil, err
	}
	return &fastpathv1.UpdateResponse{Success: true, Message: "desired state committed", Sandbox: sandboxInfo(&updated)}, nil
}

func toFailurePolicy(policy fastpathv1.FailurePolicy) apiv1alpha1.FailurePolicy {
	if policy == fastpathv1.FailurePolicy_AUTO_RECREATE {
		return apiv1alpha1.FailurePolicyAutoRecreate
	}
	return apiv1alpha1.FailurePolicyManual
}
