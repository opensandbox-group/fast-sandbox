package fastpath

import (
	"context"
	"crypto/sha256"
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
	"fast-sandbox/internal/routeauth"
	"fast-sandbox/internal/runtimecatalog"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	fastpathv1.UnimplementedFastPathServiceServer
	K8sClient           client.Client
	RouteCache          client.Client
	Orchestrator        *sandboxorchestrator.Orchestrator
	CredentialIssuer    *routeauth.Issuer
	SandboxProxyBaseURL string

	// Deprecated construction fields retained for source compatibility while
	// deployments migrate to Orchestrator. Fast/Strong no longer select a path.
	Registry               fastletpool.FastletRegistry
	FastletClient          *api.FastletClient
	DefaultConsistencyMode api.ConsistencyMode
	Catalog                *runtimecatalog.Catalog
}

var _ fastpathv1.FastPathServiceServer = &Server{}

func (s *Server) ResolveEndpoint(ctx context.Context, request *fastpathv1.ResolveEndpointRequest) (*fastpathv1.ResolveEndpointResponse, error) {
	if request == nil || request.SandboxUid == "" || request.TargetPort == 0 || request.TargetPort > 65535 {
		return nil, status.Error(codes.InvalidArgument, "sandbox_uid and target_port between 1 and 65535 are required")
	}
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
	defer func() {
		success := "true"
		if resultErr != nil {
			success = "false"
		}
		createSandboxDuration.WithLabelValues("v2", success).Observe(time.Since(started).Seconds())
	}()

	if request == nil || request.Image == "" || request.PoolRef == "" {
		return nil, status.Error(codes.InvalidArgument, "image and pool_ref are required")
	}
	if request.Namespace == "" {
		request.Namespace = "default"
	}
	if request.RequestId == "" {
		generated, err := idgen.GenerateRequestID()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "generate request_id: %v", err)
		}
		request.RequestId = generated
	}
	if err := ValidateRequestID(request.RequestId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	createSpecHash, err := CreateSpecHash(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hash create request: %v", err)
	}
	orchestrator, err := s.orchestrator()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	existing, err := s.findSandboxByRequestID(ctx, request.Namespace, request.RequestId)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Annotations[common.AnnotationCreateSpecHash] != createSpecHash {
			return nil, status.Errorf(codes.AlreadyExists, "request_id %q is bound to a different create spec", request.RequestId)
		}
		if err := s.ensureExisting(ctx, orchestrator, existing); err != nil {
			return nil, err
		}
		if err := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
			return nil, err
		}
		return createResponseFromSandbox(existing), nil
	}

	sandbox := sandboxFromCreateRequest(request, createSpecHash)
	reservation, err := orchestrator.ReserveForCreate(ctx, sandbox, request.RequestId, createSpecHash)
	if err != nil {
		if errors.Is(err, sandboxorchestrator.ErrNoCandidate) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	ownedReservation := reservation
	defer func() {
		cancelContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := orchestrator.CancelReservation(cancelContext, ownedReservation); err != nil {
			klog.ErrorS(err, "Cancel Fastlet reservation", "requestID", request.RequestId, "fastlet", ownedReservation.Fastlet.ID)
		}
	}()

	if err := s.K8sClient.Create(ctx, sandbox); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		var collided apiv1alpha1.Sandbox
		if getErr := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), &collided); getErr != nil {
			return nil, errors.Join(err, getErr)
		}
		if collided.Annotations[common.AnnotationRequestID] != request.RequestId || collided.Annotations[common.AnnotationCreateSpecHash] != createSpecHash {
			return nil, status.Errorf(codes.AlreadyExists, "Sandbox name %q belongs to another request", sandbox.Name)
		}
		sandbox = collided.DeepCopy()
	}

	assigned, won, err := orchestrator.EnsureAssignment(ctx, sandbox, reservation.Fastlet)
	if err != nil {
		return nil, err
	}
	runtimeReservation := reservation
	if !won && assigned.Status.Assignment.FastletPodUID != reservation.Fastlet.PodUID {
		// Another active FastPath replica won the CRD CAS. Its durable assignment
		// is authoritative for the first attempt. Keep our reservation alive until
		// that winner either succeeds or explicitly rejects admission.
		runtimeReservation = nil
	}
	ensureErr := orchestrator.EnsureRuntime(ctx, assigned, runtimeReservation)
	if ensureErr != nil && runtimeReservation == nil && isAdmissionReschedule(ensureErr) {
		// Controller can race the FastPath between CRD Create and assignment CAS.
		// If it picked a stale/full Fastlet while this request already owns a valid
		// reservation elsewhere, move only the rejected assignment and consume the
		// reservation instead of leaking a retryable capacity error to the caller.
		cleared, clearErr := orchestrator.ClearAssignment(ctx, assigned, false)
		if clearErr != nil {
			return nil, errors.Join(ensureErr, clearErr)
		}
		assigned, _, clearErr = orchestrator.EnsureAssignment(ctx, cleared, ownedReservation.Fastlet)
		if clearErr != nil {
			return nil, errors.Join(ensureErr, clearErr)
		}
		if assigned.Status.Assignment != nil && assigned.Status.Assignment.FastletPodUID == ownedReservation.Fastlet.PodUID {
			runtimeReservation = ownedReservation
		}
		ensureErr = orchestrator.EnsureRuntime(ctx, assigned, runtimeReservation)
	}
	if ensureErr != nil {
		err := ensureErr
		if !errors.Is(err, sandboxorchestrator.ErrRuntimeInProgress) && !errors.Is(err, sandboxorchestrator.ErrUnknownFastletOutcome) {
			return nil, err
		}
		if err := waitForRuntime(ctx, orchestrator, assigned); err != nil {
			return nil, err
		}
	}
	if err := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(assigned), assigned); err != nil {
		return nil, err
	}
	return createResponseFromSandbox(assigned), nil
}

func isAdmissionReschedule(err error) bool {
	var failure *api.FastletError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case api.ErrorCapacityRejected, api.ErrorDraining, api.ErrorRuntimeUnavailable, api.ErrorNetworkUnavailable, api.ErrorInfraUnavailable:
		return true
	default:
		return false
	}
}

func (s *Server) ensureExisting(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) error {
	if sandbox.Status.RuntimeState == apiv1alpha1.ObservedStateReady && sandbox.Status.DataPlaneState == apiv1alpha1.ObservedStateReady {
		return nil
	}
	if sandbox.Status.Assignment == nil {
		assigned, _, err := orchestrator.AssignDeclarative(ctx, sandbox, sandbox.Annotations[common.AnnotationRequestID])
		if err != nil {
			if errors.Is(err, sandboxorchestrator.ErrNoCandidate) {
				return status.Error(codes.ResourceExhausted, err.Error())
			}
			return err
		}
		sandbox = assigned
	}
	if err := orchestrator.EnsureRuntime(ctx, sandbox, nil); err != nil {
		if !errors.Is(err, sandboxorchestrator.ErrRuntimeInProgress) && !errors.Is(err, sandboxorchestrator.ErrUnknownFastletOutcome) {
			return err
		}
		return waitForRuntime(ctx, orchestrator, sandbox)
	}
	return nil
}

func waitForRuntime(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := orchestrator.ObserveRuntime(ctx, sandbox)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sandboxorchestrator.ErrRuntimeInProgress) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func sandboxFromCreateRequest(request *fastpathv1.CreateRequest, createSpecHash string) *apiv1alpha1.Sandbox {
	name := request.Name
	if name == "" {
		digest := sha256.Sum256([]byte(request.RequestId))
		name = fmt.Sprintf("sb-%x", digest[:12])
	}
	environment := make([]corev1.EnvVar, 0, len(request.Envs))
	for name, value := range request.Envs {
		environment = append(environment, corev1.EnvVar{Name: name, Value: value})
	}
	return &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: request.Namespace,
			Annotations: map[string]string{
				common.AnnotationRequestID: request.RequestId, common.AnnotationCreateSpecHash: createSpecHash,
			},
			Labels: map[string]string{
				common.LabelCreatedBy: "fastpath", common.LabelRequestIDHash: requestIDLabelValue(request.RequestId),
			},
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image: request.Image, PoolRef: request.PoolRef, ExposedPorts: request.ExposedPorts,
			Command: request.Command, Args: request.Args, Envs: environment, WorkingDir: request.WorkingDir,
		},
	}
}

func (s *Server) orchestrator() (*sandboxorchestrator.Orchestrator, error) {
	if s.Orchestrator != nil {
		return s.Orchestrator, nil
	}
	registry, ok := s.Registry.(sandboxorchestrator.Registry)
	if !ok || s.FastletClient == nil {
		return nil, errors.New("Sandbox orchestrator is not configured")
	}
	return &sandboxorchestrator.Orchestrator{
		Client: s.K8sClient, Registry: registry, FastletClient: s.FastletClient, Catalog: s.Catalog,
	}, nil
}

func (s *Server) findSandboxByRequestID(ctx context.Context, namespace, requestID string) (*apiv1alpha1.Sandbox, error) {
	var list apiv1alpha1.SandboxList
	if err := s.K8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		common.LabelRequestIDHash: requestIDLabelValue(requestID),
	}); err != nil {
		return nil, err
	}
	matches := make([]apiv1alpha1.Sandbox, 0, len(list.Items))
	for index := range list.Items {
		if list.Items[index].Annotations[common.AnnotationRequestID] == requestID {
			matches = append(matches, list.Items[index])
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("request_id %q is bound to multiple Sandbox objects", requestID)
	}
	if len(matches) == 1 {
		return matches[0].DeepCopy(), nil
	}
	return nil, nil
}

func createResponseFromSandbox(sandbox *apiv1alpha1.Sandbox) *fastpathv1.CreateResponse {
	fastletName := sandbox.Status.AssignedFastlet
	if sandbox.Status.Assignment != nil {
		fastletName = sandbox.Status.Assignment.FastletName
	}
	return &fastpathv1.CreateResponse{
		SandboxId: string(sandbox.UID), SandboxUid: string(sandbox.UID),
		SandboxName: sandbox.Name, FastletPod: fastletName,
	}
}

func (s *Server) ListSandboxes(ctx context.Context, request *fastpathv1.ListRequest) (*fastpathv1.ListResponse, error) {
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
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
	var sandbox apiv1alpha1.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: request.SandboxName, Namespace: namespace}, &sandbox); err != nil {
		return nil, err
	}
	return sandboxInfo(&sandbox), nil
}

func sandboxInfo(sandbox *apiv1alpha1.Sandbox) *fastpathv1.SandboxInfo {
	fastletName := sandbox.Status.AssignedFastlet
	if sandbox.Status.Assignment != nil {
		fastletName = sandbox.Status.Assignment.FastletName
	}
	return &fastpathv1.SandboxInfo{
		SandboxId: string(sandbox.UID), SandboxName: sandbox.Name, Phase: sandbox.Status.Phase,
		FastletPod: fastletName, Image: sandbox.Spec.Image, PoolRef: sandbox.Spec.PoolRef,
		CreatedAt: sandbox.CreationTimestamp.Unix(),
	}
}

// Delete only submits desired state. Finalizer reconciliation owns runtime cleanup.
func (s *Server) DeleteSandbox(ctx context.Context, request *fastpathv1.DeleteRequest) (*fastpathv1.DeleteResponse, error) {
	namespace := request.Namespace
	if namespace == "" {
		namespace = "default"
	}
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
