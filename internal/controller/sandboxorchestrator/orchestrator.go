package sandboxorchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/runtimecatalog"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrNoCandidate                = errors.New("no eligible Fastlet for the Sandbox request")
	ErrRuntimeInProgress          = errors.New("Sandbox runtime creation is in progress")
	ErrAssignedFastletUnavailable = errors.New("assigned Fastlet is unavailable or was replaced")
	ErrUnknownFastletOutcome      = errors.New("Fastlet operation outcome is unknown")
)

const (
	ConditionRuntimeReady   = apiv1alpha1.SandboxConditionRuntimeReady
	ConditionDataPlaneReady = apiv1alpha1.SandboxConditionDataPlaneReady
	ReasonFastletPodLost    = "FastletPodLost"
	ReasonExpired           = "Expired"
)

type FastletClient interface {
	CreateSandbox(context.Context, string, *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error)
	InspectSandbox(context.Context, string, *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error)
	DeleteSandboxV2(context.Context, string, *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error)
}

type Registry interface {
	TopK(fastletpool.CandidateRequest, int) []fastletpool.FastletInfo
	GetFastletByID(fastletpool.FastletID) (fastletpool.FastletInfo, bool)
	RecordFeedback(fastletpool.FastletID, fastletpool.LocalFeedback)
}

type Orchestrator struct {
	Client        client.Client
	Registry      Registry
	FastletClient FastletClient
	Catalog       *runtimecatalog.Catalog
	InfraCatalog  *infracatalog.Catalog
	TopK          int
	Now           func() time.Time
}

// RuntimeParameters are used only by the declarative Controller to validate a
// Pool against the watched Fastlet profile. Fastlet remains authoritative for
// the concrete CPU/memory/PID values it injects into the runtime request.
type RuntimeParameters struct {
	RuntimeName         apiv1alpha1.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraProfile        string
	InfraProfileHash    string
}

func (o *Orchestrator) ResolveRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (RuntimeParameters, error) {
	if sandbox == nil {
		return RuntimeParameters{}, errors.New("Sandbox is required")
	}
	var pool apiv1alpha1.SandboxPool
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return RuntimeParameters{}, fmt.Errorf("get SandboxPool %s: %w", sandbox.Spec.PoolRef, err)
	}
	if err := pool.Spec.ValidateRuntime(); err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve Pool runtime: %w", err)
	}
	catalog := o.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	profile, err := catalog.Resolve(pool.Spec.Runtime)
	if err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve runtime profile: %w", err)
	}
	if err := apiv1alpha1.ValidateSandboxResourceProfile(pool.Spec.SandboxResources); err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve Sandbox resources: %w", err)
	}
	infraCatalog := o.InfraCatalog
	if infraCatalog == nil {
		infraCatalog = infracatalog.Builtin()
	}
	infraPlan, err := infraCatalog.Compile(pool.Spec.InfraProfile, profile)
	if err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve InfraProfile: %w", err)
	}
	return RuntimeParameters{
		RuntimeName: pool.Spec.Runtime, RuntimeProfileHash: profile.ProfileHash,
		ResourceProfileHash: pool.Spec.SandboxResources.Hash(),
		InfraProfile:        infraPlan.ProfileName, InfraProfileHash: infraPlan.ProfileHash,
	}, nil
}

func (o *Orchestrator) Candidates(ctx context.Context, sandbox *apiv1alpha1.Sandbox, stableKey string) ([]fastletpool.FastletInfo, RuntimeParameters, error) {
	parameters, err := o.ResolveRuntime(ctx, sandbox)
	if err != nil {
		return nil, RuntimeParameters{}, err
	}
	candidates := o.topK(fastletpool.CandidateRequest{
		Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef,
		RuntimeName: parameters.RuntimeName, RuntimeProfileHash: parameters.RuntimeProfileHash,
		ResourceProfileHash: parameters.ResourceProfileHash, InfraProfileHash: parameters.InfraProfileHash,
		Image: sandbox.Spec.Image, StableKey: stableKey,
	})
	if len(candidates) == 0 {
		return nil, parameters, ErrNoCandidate
	}
	return candidates, parameters, nil
}

// FastPathCandidates is intentionally registry-only. Calling it cannot issue
// a Kubernetes API request, which keeps the first-create happy path at two IOs.
func (o *Orchestrator) FastPathCandidates(sandbox *apiv1alpha1.Sandbox, stableKey string) ([]fastletpool.FastletInfo, error) {
	if sandbox == nil {
		return nil, errors.New("Sandbox is required")
	}
	candidates := o.topK(fastletpool.CandidateRequest{
		Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef,
		Image: sandbox.Spec.Image, StableKey: stableKey,
	})
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}
	return candidates, nil
}

func (o *Orchestrator) topK(request fastletpool.CandidateRequest) []fastletpool.FastletInfo {
	if o.Registry == nil {
		return nil
	}
	if request.Now.IsZero() {
		request.Now = time.Now()
		if o.Now != nil {
			request.Now = o.Now()
		}
	}
	k := o.TopK
	if k <= 0 {
		k = 3
	}
	return o.Registry.TopK(request, k)
}

func AssignmentForCandidate(candidate fastletpool.FastletInfo, attempt, instanceGeneration, routeGeneration int64, runtimeInstanceID string) (common.AssignmentEnvelope, error) {
	envelope := common.AssignmentEnvelope{
		Version:     common.AssignmentEnvelopeVersion,
		FastletName: candidate.PodName, FastletPodUID: candidate.PodUID, NodeName: candidate.NodeName,
		Attempt: attempt, InstanceGeneration: instanceGeneration, RouteGeneration: routeGeneration,
		RuntimeInstanceID:  runtimeInstanceID,
		RuntimeProfileHash: candidate.RuntimeProfileHash, ResourceProfileHash: candidate.ResourceProfileHash,
		InfraProfileHash: candidate.InfraProfileHash,
	}
	if err := envelope.Validate(); err != nil {
		return common.AssignmentEnvelope{}, err
	}
	if candidate.PodIP == "" || candidate.InfraProfile == "" {
		return common.AssignmentEnvelope{}, errors.New("candidate endpoint and InfraProfile are required")
	}
	return envelope, nil
}

// AssignDeclarative preserves the standalone Controller deployment mode. It
// first honors any FastPath-written annotation, then performs Pool validation
// and registry selection only when no durable assignment exists.
func (o *Orchestrator) AssignDeclarative(ctx context.Context, sandbox *apiv1alpha1.Sandbox, stableKey string) (*apiv1alpha1.Sandbox, bool, error) {
	if sandbox == nil || sandbox.UID == "" {
		return nil, false, errors.New("persisted Sandbox UID is required")
	}
	envelope, err := common.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, false, err
	}
	if envelope != nil {
		projected, err := common.ProjectAssignmentToStatus(ctx, o.Client, client.ObjectKeyFromObject(sandbox))
		return projected, false, err
	}

	// Upgrade bridge for objects created by the previous status-only model.
	if sandbox.Status.Assignment != nil {
		candidate, ok := o.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.Assignment.FastletName))
		if !ok || candidate.PodUID != sandbox.Status.Assignment.FastletPodUID {
			return nil, false, ErrAssignedFastletUnavailable
		}
		generation := max(sandbox.Status.InstanceGeneration, apiv1alpha1.InitialInstanceGeneration)
		routeGeneration := max(sandbox.Status.RouteGeneration, int64(1))
		legacyEnvelope, err := AssignmentForCandidate(candidate, sandbox.Status.Assignment.Attempt, generation, routeGeneration, "legacy-"+string(sandbox.UID))
		if err != nil {
			return nil, false, err
		}
		if _, _, err := common.InitializeAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), legacyEnvelope); err != nil {
			return nil, false, err
		}
		projected, err := common.ProjectAssignmentToStatus(ctx, o.Client, client.ObjectKeyFromObject(sandbox))
		return projected, false, err
	}

	candidates, _, err := o.Candidates(ctx, sandbox, stableKey)
	if err != nil {
		return nil, false, err
	}
	runtimeInstanceID, err := idgen.GenerateRequestID()
	if err != nil {
		return nil, false, fmt.Errorf("generate runtime instance ID: %w", err)
	}
	attempt := sandbox.Status.AssignmentAttempt + 1
	generation := max(sandbox.Status.InstanceGeneration, apiv1alpha1.InitialInstanceGeneration)
	routeGeneration := max(sandbox.Status.RouteGeneration, int64(1))
	desired, err := AssignmentForCandidate(candidates[0], attempt, generation, routeGeneration, runtimeInstanceID)
	if err != nil {
		return nil, false, err
	}
	_, won, err := common.InitializeAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), desired)
	if err != nil {
		return nil, false, err
	}
	projected, err := common.ProjectAssignmentToStatus(ctx, o.Client, client.ObjectKeyFromObject(sandbox))
	return projected, won, err
}

// ReassignDeclarativeAfterRejection atomically moves a durable assignment to
// a different eligible Fastlet. When no alternative exists it deliberately
// preserves the current annotation, so CRD-first never exposes an unassigned
// window between rejection and a later Pool scale-up or heartbeat refresh.
func (o *Orchestrator) ReassignDeclarativeAfterRejection(ctx context.Context, sandbox *apiv1alpha1.Sandbox, stableKey string) (*apiv1alpha1.Sandbox, bool, error) {
	if sandbox == nil || sandbox.UID == "" {
		return nil, false, errors.New("persisted Sandbox UID is required")
	}
	current, err := common.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, false, err
	}
	if current == nil {
		return sandbox.DeepCopy(), false, nil
	}
	candidates, _, err := o.Candidates(ctx, sandbox, stableKey)
	if errors.Is(err, ErrNoCandidate) {
		return sandbox.DeepCopy(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, candidate := range candidates {
		if candidate.PodName == current.FastletName && candidate.PodUID == current.FastletPodUID {
			continue
		}
		runtimeInstanceID, err := idgen.GenerateRequestID()
		if err != nil {
			return nil, false, fmt.Errorf("generate runtime instance ID: %w", err)
		}
		next, err := AssignmentForCandidate(candidate, current.Attempt+1, current.InstanceGeneration, current.RouteGeneration+1, runtimeInstanceID)
		if err != nil {
			return nil, false, err
		}
		updated, err := common.CASAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), *current, next)
		if err != nil {
			return nil, false, err
		}
		return updated, true, nil
	}
	return sandbox.DeepCopy(), false, nil
}

// CreateRuntime performs exactly one Fastlet call. It never reads a Pool and
// never writes Kubernetes status, so FastPath can use it as IO 2.
func (o *Orchestrator) CreateRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (*api.CreateSandboxResponse, error) {
	fastlet, envelope, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return nil, err
	}
	return o.createRuntimeOnTarget(ctx, sandbox, fastlet, envelope, identity)
}

// CreateRuntimeOnCandidate is used immediately after FastPath wins an
// annotation Create/CAS. The annotation is revalidated, while a concurrently
// stale status projection is deliberately ignored.
func (o *Orchestrator) CreateRuntimeOnCandidate(ctx context.Context, sandbox *apiv1alpha1.Sandbox, fastlet fastletpool.FastletInfo, envelope common.AssignmentEnvelope) (*api.CreateSandboxResponse, error) {
	current, err := common.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if current == nil || *current != envelope {
		return nil, common.ErrAssignmentAnnotationChanged
	}
	if fastlet.PodName != envelope.FastletName || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" ||
		fastlet.RuntimeProfileHash != envelope.RuntimeProfileHash || fastlet.ResourceProfileHash != envelope.ResourceProfileHash ||
		fastlet.InfraProfileHash != envelope.InfraProfileHash {
		return nil, ErrAssignedFastletUnavailable
	}
	identity := api.SandboxIdentity{
		RequestID: sandbox.Annotations[common.AnnotationRequestID], SandboxUID: string(sandbox.UID),
		InstanceGeneration: envelope.InstanceGeneration, RuntimeInstanceID: envelope.RuntimeInstanceID,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration, FastletPodUID: envelope.FastletPodUID,
	}
	return o.createRuntimeOnTarget(ctx, sandbox, fastlet, envelope, identity)
}

func (o *Orchestrator) createRuntimeOnTarget(ctx context.Context, sandbox *apiv1alpha1.Sandbox, fastlet fastletpool.FastletInfo, envelope common.AssignmentEnvelope, identity api.SandboxIdentity) (*api.CreateSandboxResponse, error) {
	request := &api.CreateSandboxRequest{
		Identity: identity,
		Sandbox: api.SandboxSpec{
			SandboxID: string(sandbox.UID), RequestID: sandbox.Annotations[common.AnnotationRequestID], ClaimUID: string(sandbox.UID),
			ClaimNamespace: sandbox.Namespace, ClaimName: sandbox.Name, Image: sandbox.Spec.Image,
			RuntimeProfileHash: envelope.RuntimeProfileHash, ResourceProfileHash: envelope.ResourceProfileHash,
			InfraProfile: fastlet.InfraProfile, InfraProfileHash: envelope.InfraProfileHash,
			Command: sandbox.Spec.Command, Args: sandbox.Spec.Args, Env: envMap(sandbox.Spec.Envs), WorkingDir: sandbox.Spec.WorkingDir,
		},
	}
	response, createErr := o.FastletClient.CreateSandbox(ctx, fastlet.PodIP, request)
	if createErr == nil && response != nil && response.Accepted && response.Sandbox != nil && response.Sandbox.Phase == "running" {
		return response, nil
	}
	if createErr == nil {
		createErr = ErrUnknownFastletOutcome
	}
	o.recordFeedback(fastlet.ID, createErr)
	return response, createErr
}

// ReconcileRuntime is the declarative wrapper around CreateRuntime. Status
// convergence is intentionally outside the FastPath request.
func (o *Orchestrator) ReconcileRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	_, err := o.CreateRuntime(ctx, sandbox)
	if err == nil {
		return o.MarkReady(ctx, sandbox)
	}
	var failure *api.FastletError
	if errors.As(err, &failure) && failure.Code == api.ErrorInProgress {
		_ = o.MarkCreating(ctx, sandbox, failure.Message)
		return ErrRuntimeInProgress
	}
	if errors.As(err, &failure) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrUnknownFastletOutcome, err)
}

func (o *Orchestrator) ObserveRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return err
	}
	response, inspectErr := o.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &api.InspectSandboxRequest{Identity: identity})
	if inspectErr != nil {
		return inspectErr
	}
	if response == nil || response.Sandbox == nil {
		return ErrUnknownFastletOutcome
	}
	switch response.Sandbox.Phase {
	case "running":
		return o.MarkReady(ctx, sandbox)
	case "creating", "infra-pending", "initializing-infra", "route-pending", "publishing-route":
		_ = o.MarkCreating(ctx, sandbox, "Fastlet is still creating the runtime")
		return ErrRuntimeInProgress
	default:
		return fmt.Errorf("runtime is %s", response.Sandbox.Phase)
	}
}

func (o *Orchestrator) DeleteRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return err
	}
	_, err = o.FastletClient.DeleteSandboxV2(ctx, fastlet.PodIP, &api.DeleteSandboxV2Request{Identity: identity})
	return err
}

func (o *Orchestrator) RuntimeGone(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (bool, error) {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return errors.Is(err, ErrAssignedFastletUnavailable), err
	}
	response, err := o.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &api.InspectSandboxRequest{Identity: identity})
	if err != nil {
		var failure *api.FastletError
		if errors.As(err, &failure) && failure.Code == api.ErrorNotFound {
			return true, nil
		}
		return false, err
	}
	return response == nil || response.Sandbox == nil, nil
}

func (o *Orchestrator) ClearAssignment(ctx context.Context, sandbox *apiv1alpha1.Sandbox, advanceInstance bool) (*apiv1alpha1.Sandbox, error) {
	if sandbox == nil {
		return nil, errors.New("Sandbox is required")
	}
	envelope, err := common.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if envelope == nil {
		if sandbox.Status.Assignment == nil {
			return sandbox.DeepCopy(), nil
		}
		return o.clearAssignmentProjection(ctx, sandbox, nil, advanceInstance)
	}
	updated, _, err := common.RemoveAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), *envelope)
	if err != nil {
		return nil, err
	}
	return o.clearAssignmentProjection(ctx, updated, envelope, advanceInstance)
}

func (o *Orchestrator) clearAssignmentProjection(ctx context.Context, sandbox *apiv1alpha1.Sandbox, envelope *common.AssignmentEnvelope, advanceInstance bool) (*apiv1alpha1.Sandbox, error) {
	key := client.ObjectKeyFromObject(sandbox)
	var result *apiv1alpha1.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha1.Sandbox
		if err := o.Client.Get(ctx, key, &current); err != nil {
			return err
		}
		active, err := common.AssignmentFromAnnotation(&current)
		if err != nil {
			return err
		}
		if active != nil {
			return common.ErrAssignmentAnnotationChanged
		}
		if current.Status.Assignment == nil {
			result = current.DeepCopy()
			return nil
		}
		generation := max(current.Status.InstanceGeneration, apiv1alpha1.InitialInstanceGeneration)
		routeGeneration := max(current.Status.RouteGeneration, int64(1)) + 1
		if envelope != nil {
			generation = max(generation, envelope.InstanceGeneration)
			routeGeneration = max(routeGeneration, envelope.RouteGeneration+1)
		}
		if advanceInstance {
			generation = apiv1alpha1.NextInstanceGeneration(generation)
		}
		before := current.DeepCopy()
		current.Status.Assignment = nil
		current.Status.InstanceGeneration = generation
		current.Status.RouteGeneration = routeGeneration
		current.Status.RuntimeState = apiv1alpha1.ObservedStatePending
		current.Status.DataPlaneState = apiv1alpha1.ObservedStatePending
		if err := o.Client.Status().Patch(ctx, &current, client.MergeFrom(before)); err != nil {
			return err
		}
		result = current.DeepCopy()
		return nil
	})
	return result, err
}

func (o *Orchestrator) MarkPending(ctx context.Context, sandbox *apiv1alpha1.Sandbox, reason, message string) error {
	return o.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStatePending
		status.DataPlaneState = apiv1alpha1.ObservedStatePending
		setCondition(status, ConditionRuntimeReady, metav1.ConditionFalse, reason, message)
		setCondition(status, ConditionDataPlaneReady, metav1.ConditionFalse, reason, message)
	})
}

func (o *Orchestrator) MarkCreating(ctx context.Context, sandbox *apiv1alpha1.Sandbox, message string) error {
	return o.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateCreating
		status.DataPlaneState = apiv1alpha1.ObservedStatePending
		setCondition(status, ConditionRuntimeReady, metav1.ConditionFalse, "Creating", message)
	})
}

func (o *Orchestrator) MarkReady(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	return o.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateReady
		status.DataPlaneState = apiv1alpha1.ObservedStateReady
		setCondition(status, ConditionRuntimeReady, metav1.ConditionTrue, "RuntimeRunning", "Fastlet reports the runtime running")
		setCondition(status, ConditionDataPlaneReady, metav1.ConditionTrue, "FastletRouteReady", "Fastlet reports the instance-fenced local proxy route published")
	})
}

func (o *Orchestrator) patchStatus(ctx context.Context, sandbox *apiv1alpha1.Sandbox, mutate func(*apiv1alpha1.SandboxStatus)) error {
	key := client.ObjectKeyFromObject(sandbox)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current apiv1alpha1.Sandbox
		if err := o.Client.Get(ctx, key, &current); err != nil {
			return err
		}
		if _, err := common.EffectiveAssignment(&current); err != nil {
			return err
		}
		before := current.DeepCopy().Status
		mutate(&current.Status)
		if reflect.DeepEqual(before, current.Status) {
			return nil
		}
		return o.Client.Status().Update(ctx, &current)
	})
}

func (o *Orchestrator) assignedTarget(sandbox *apiv1alpha1.Sandbox) (fastletpool.FastletInfo, common.AssignmentEnvelope, api.SandboxIdentity, error) {
	if sandbox == nil || sandbox.UID == "" {
		return fastletpool.FastletInfo{}, common.AssignmentEnvelope{}, api.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	envelope, err := common.EffectiveAssignment(sandbox)
	if err != nil {
		if !errors.Is(err, common.ErrAssignmentAnnotationMissing) || sandbox.Status.Assignment == nil {
			return fastletpool.FastletInfo{}, common.AssignmentEnvelope{}, api.SandboxIdentity{}, err
		}
		// Status-only objects are an upgrade bridge. Existing runtimes recover the
		// same deterministic legacy identity; active reconcile persists it later.
		legacyFastlet, ok := o.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.Assignment.FastletName))
		if !ok || legacyFastlet.PodUID != sandbox.Status.Assignment.FastletPodUID || legacyFastlet.PodIP == "" {
			return fastletpool.FastletInfo{}, common.AssignmentEnvelope{}, api.SandboxIdentity{}, ErrAssignedFastletUnavailable
		}
		legacy, legacyErr := AssignmentForCandidate(
			legacyFastlet, sandbox.Status.Assignment.Attempt,
			max(sandbox.Status.InstanceGeneration, apiv1alpha1.InitialInstanceGeneration),
			max(sandbox.Status.RouteGeneration, int64(1)), "legacy-"+string(sandbox.UID),
		)
		if legacyErr != nil {
			return fastletpool.FastletInfo{}, common.AssignmentEnvelope{}, api.SandboxIdentity{}, legacyErr
		}
		identity := api.SandboxIdentity{
			RequestID: sandbox.Annotations[common.AnnotationRequestID], SandboxUID: string(sandbox.UID),
			InstanceGeneration: legacy.InstanceGeneration, RuntimeInstanceID: legacy.RuntimeInstanceID,
			AssignmentAttempt: legacy.Attempt, RouteGeneration: legacy.RouteGeneration, FastletPodUID: legacy.FastletPodUID,
		}
		return legacyFastlet, legacy, identity, nil
	}
	if envelope == nil {
		return fastletpool.FastletInfo{}, common.AssignmentEnvelope{}, api.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	fastlet, ok := o.Registry.GetFastletByID(fastletpool.FastletID(envelope.FastletName))
	if !ok || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" ||
		fastlet.RuntimeProfileHash != envelope.RuntimeProfileHash || fastlet.ResourceProfileHash != envelope.ResourceProfileHash ||
		fastlet.InfraProfileHash != envelope.InfraProfileHash {
		return fastletpool.FastletInfo{}, common.AssignmentEnvelope{}, api.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	identity := api.SandboxIdentity{
		RequestID: sandbox.Annotations[common.AnnotationRequestID], SandboxUID: string(sandbox.UID),
		InstanceGeneration: envelope.InstanceGeneration, RuntimeInstanceID: envelope.RuntimeInstanceID,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration, FastletPodUID: envelope.FastletPodUID,
	}
	return fastlet, *envelope, identity, nil
}

func envMap(values []corev1.EnvVar) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.Name] = value.Value
	}
	return result
}

func IsCandidateRejection(err error) bool {
	var failure *api.FastletError
	if !errors.As(err, &failure) || failure.Outcome != api.OutcomeRejectedBeforeSideEffects {
		return false
	}
	switch failure.Code {
	case api.ErrorCapacityRejected, api.ErrorDraining, api.ErrorRuntimeUnavailable, api.ErrorNetworkUnavailable, api.ErrorInfraUnavailable, api.ErrorProfileMismatch:
		return true
	default:
		return false
	}
}

func (o *Orchestrator) RecordCandidateFeedback(id fastletpool.FastletID, err error) {
	o.recordFeedback(id, err)
}

func (o *Orchestrator) recordFeedback(id fastletpool.FastletID, err error) {
	var failure *api.FastletError
	if !errors.As(err, &failure) {
		return
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	o.Registry.RecordFeedback(id, fastletpool.LocalFeedback{Code: failure.Code, ObservedAt: now})
}

func setCondition(status *apiv1alpha1.SandboxStatus, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, Reason: reason, Message: message,
		ObservedGeneration: 0, LastTransitionTime: metav1.Now(),
	})
}

func IsNotFound(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	var fastletErr *api.FastletError
	return errors.As(err, &fastletErr) && fastletErr.Code == api.ErrorNotFound
}
