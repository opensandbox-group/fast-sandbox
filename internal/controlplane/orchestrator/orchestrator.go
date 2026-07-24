package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
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
	ErrDataPlaneInProgress        = errors.New("Sandbox data plane initialization is in progress")
	ErrDataPlaneUnavailable       = errors.New("Sandbox data plane is unavailable and retrying")
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
	CreateSandbox(context.Context, string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error)
	InspectSandbox(context.Context, string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error)
	DeleteSandboxV2(context.Context, string, *fastletapi.DeleteSandboxV2Request) (*fastletapi.DeleteSandboxV2Response, error)
}

type Registry interface {
	TopK(placement.CandidateRequest, int) []placement.FastletInfo
	GetFastletByID(placement.FastletID) (placement.FastletInfo, bool)
	RecordFeedback(placement.FastletID, placement.LocalFeedback)
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

func (o *Orchestrator) Candidates(ctx context.Context, sandbox *apiv1alpha1.Sandbox, stableKey string) ([]placement.FastletInfo, RuntimeParameters, error) {
	parameters, err := o.ResolveRuntime(ctx, sandbox)
	if err != nil {
		return nil, RuntimeParameters{}, err
	}
	candidates := o.topK(placement.CandidateRequest{
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
func (o *Orchestrator) FastPathCandidates(sandbox *apiv1alpha1.Sandbox, stableKey string) ([]placement.FastletInfo, error) {
	if sandbox == nil {
		return nil, errors.New("Sandbox is required")
	}
	candidates := o.topK(placement.CandidateRequest{
		Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef,
		Image: sandbox.Spec.Image, StableKey: stableKey,
	})
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}
	return candidates, nil
}

func (o *Orchestrator) topK(request placement.CandidateRequest) []placement.FastletInfo {
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

func AssignmentForCandidate(candidate placement.FastletInfo, attempt, instanceGeneration, routeGeneration int64, runtimeInstanceID string) (assignment.AssignmentEnvelope, error) {
	envelope := assignment.AssignmentEnvelope{
		Version:     assignment.AssignmentEnvelopeVersion,
		FastletName: candidate.PodName, FastletPodUID: candidate.PodUID, NodeName: candidate.NodeName,
		Attempt: attempt, InstanceGeneration: instanceGeneration, RouteGeneration: routeGeneration,
		RuntimeInstanceID:  runtimeInstanceID,
		RuntimeProfileHash: candidate.RuntimeProfileHash, ResourceProfileHash: candidate.ResourceProfileHash,
		InfraProfileHash: candidate.InfraProfileHash,
	}
	if err := envelope.Validate(); err != nil {
		return assignment.AssignmentEnvelope{}, err
	}
	if candidate.PodIP == "" || candidate.InfraProfile == "" {
		return assignment.AssignmentEnvelope{}, errors.New("candidate endpoint and InfraProfile are required")
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
	envelope, err := assignment.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, false, err
	}
	if envelope != nil {
		projected, err := assignment.ProjectAssignmentToStatus(ctx, o.Client, client.ObjectKeyFromObject(sandbox))
		return projected, false, err
	}

	// Upgrade bridge for objects created by the previous status-only model.
	if sandbox.Status.Assignment != nil {
		candidate, ok := o.Registry.GetFastletByID(placement.FastletID(sandbox.Status.Assignment.FastletName))
		if !ok || candidate.PodUID != sandbox.Status.Assignment.FastletPodUID {
			return nil, false, ErrAssignedFastletUnavailable
		}
		generation := max(sandbox.Status.InstanceGeneration, apiv1alpha1.InitialInstanceGeneration)
		routeGeneration := max(sandbox.Status.RouteGeneration, int64(1))
		legacyEnvelope, err := AssignmentForCandidate(candidate, sandbox.Status.Assignment.Attempt, generation, routeGeneration, "legacy-"+string(sandbox.UID))
		if err != nil {
			return nil, false, err
		}
		if _, _, err := assignment.InitializeAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), legacyEnvelope); err != nil {
			return nil, false, err
		}
		projected, err := assignment.ProjectAssignmentToStatus(ctx, o.Client, client.ObjectKeyFromObject(sandbox))
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
	_, won, err := assignment.InitializeAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), desired)
	if err != nil {
		return nil, false, err
	}
	projected, err := assignment.ProjectAssignmentToStatus(ctx, o.Client, client.ObjectKeyFromObject(sandbox))
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
	current, err := assignment.AssignmentFromAnnotation(sandbox)
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
		updated, err := assignment.CASAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), *current, next)
		if err != nil {
			return nil, false, err
		}
		return updated, true, nil
	}
	return sandbox.DeepCopy(), false, nil
}

// CreateRuntime performs exactly one Fastlet call. It never reads a Pool and
// never writes Kubernetes status, so FastPath can use it as IO 2.
func (o *Orchestrator) CreateRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (*fastletapi.CreateSandboxResponse, error) {
	fastlet, envelope, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return nil, err
	}
	return o.createRuntimeOnTarget(ctx, sandbox, fastlet, envelope, identity)
}

// CreateRuntimeOnCandidate is used immediately after FastPath wins an
// annotation Create/CAS. The annotation is revalidated, while a concurrently
// stale status projection is deliberately ignored.
func (o *Orchestrator) CreateRuntimeOnCandidate(ctx context.Context, sandbox *apiv1alpha1.Sandbox, fastlet placement.FastletInfo, envelope assignment.AssignmentEnvelope) (*fastletapi.CreateSandboxResponse, error) {
	current, err := assignment.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if current == nil || *current != envelope {
		return nil, assignment.ErrAssignmentAnnotationChanged
	}
	if fastlet.PodName != envelope.FastletName || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" ||
		fastlet.RuntimeProfileHash != envelope.RuntimeProfileHash || fastlet.ResourceProfileHash != envelope.ResourceProfileHash ||
		fastlet.InfraProfileHash != envelope.InfraProfileHash {
		return nil, ErrAssignedFastletUnavailable
	}
	identity := fastletapi.SandboxIdentity{
		RequestID: sandbox.Annotations[assignment.AnnotationRequestID], SandboxUID: string(sandbox.UID),
		InstanceGeneration: envelope.InstanceGeneration, RuntimeInstanceID: envelope.RuntimeInstanceID,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration, FastletPodUID: envelope.FastletPodUID,
	}
	return o.createRuntimeOnTarget(ctx, sandbox, fastlet, envelope, identity)
}

func (o *Orchestrator) createRuntimeOnTarget(ctx context.Context, sandbox *apiv1alpha1.Sandbox, fastlet placement.FastletInfo, envelope assignment.AssignmentEnvelope, identity fastletapi.SandboxIdentity) (*fastletapi.CreateSandboxResponse, error) {
	request := &fastletapi.CreateSandboxRequest{
		Identity: identity,
		Sandbox: fastletapi.SandboxSpec{
			SandboxID: string(sandbox.UID), RequestID: sandbox.Annotations[assignment.AnnotationRequestID], ClaimUID: string(sandbox.UID),
			ClaimNamespace: sandbox.Namespace, ClaimName: sandbox.Name, Image: sandbox.Spec.Image,
			RuntimeProfileHash: envelope.RuntimeProfileHash, ResourceProfileHash: envelope.ResourceProfileHash,
			InfraProfile: fastlet.InfraProfile, InfraProfileHash: envelope.InfraProfileHash,
			Command: sandbox.Spec.Command, Args: sandbox.Spec.Args, Env: envMap(sandbox.Spec.Envs), WorkingDir: sandbox.Spec.WorkingDir,
		},
	}
	response, createErr := o.FastletClient.CreateSandbox(ctx, fastlet.PodIP, request)
	if createErr == nil && response != nil && response.Accepted && response.Sandbox != nil {
		switch response.Sandbox.Phase {
		case "running", "infra-pending", "initializing-infra", "infra-unavailable", "route-pending", "publishing-route", "route-unavailable":
			return response, nil
		}
	}
	if createErr == nil {
		createErr = ErrUnknownFastletOutcome
	}
	o.recordFeedback(fastlet.ID, createErr)
	return response, createErr
}

// ReconcileRuntime is the declarative wrapper around CreateRuntime. A
// successful Create proves the runtime is ready; Infra readiness and route
// publication are projected independently as the data plane converges.
func (o *Orchestrator) ReconcileRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	response, err := o.CreateRuntime(ctx, sandbox)
	if err == nil {
		return o.projectObservedPhase(ctx, sandbox, response.Sandbox.Phase)
	}
	var failure *fastletapi.FastletError
	if errors.As(err, &failure) && failure.Code == fastletapi.ErrorInProgress {
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
	response, inspectErr := o.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &fastletapi.InspectSandboxRequest{Identity: identity})
	if inspectErr != nil {
		return inspectErr
	}
	if response == nil || response.Sandbox == nil {
		return ErrUnknownFastletOutcome
	}
	switch response.Sandbox.Phase {
	case "running":
		return o.MarkReady(ctx, sandbox)
	case "creating":
		_ = o.MarkCreating(ctx, sandbox, "Fastlet is still creating the runtime")
		return ErrRuntimeInProgress
	default:
		return o.projectObservedPhase(ctx, sandbox, response.Sandbox.Phase)
	}
}

func (o *Orchestrator) projectObservedPhase(ctx context.Context, sandbox *apiv1alpha1.Sandbox, phase string) error {
	switch phase {
	case "running":
		return o.MarkReady(ctx, sandbox)
	case "infra-pending", "initializing-infra", "route-pending", "publishing-route":
		if err := o.MarkRuntimeReadyDataPlaneCreating(ctx, sandbox, phase); err != nil {
			return err
		}
		return ErrDataPlaneInProgress
	case "infra-unavailable", "route-unavailable":
		if err := o.MarkRuntimeReadyDataPlaneUnavailable(ctx, sandbox, phase); err != nil {
			return err
		}
		return ErrDataPlaneUnavailable
	default:
		return fmt.Errorf("runtime is %s", phase)
	}
}

func (o *Orchestrator) DeleteRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return err
	}
	_, err = o.FastletClient.DeleteSandboxV2(ctx, fastlet.PodIP, &fastletapi.DeleteSandboxV2Request{Identity: identity})
	return err
}

func (o *Orchestrator) RuntimeGone(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (bool, error) {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return errors.Is(err, ErrAssignedFastletUnavailable), err
	}
	response, err := o.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &fastletapi.InspectSandboxRequest{Identity: identity})
	if err != nil {
		var failure *fastletapi.FastletError
		if errors.As(err, &failure) && failure.Code == fastletapi.ErrorNotFound {
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
	envelope, err := assignment.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if envelope == nil {
		if sandbox.Status.Assignment == nil {
			return sandbox.DeepCopy(), nil
		}
		return o.clearAssignmentProjection(ctx, sandbox, nil, advanceInstance)
	}
	updated, _, err := assignment.RemoveAssignmentAnnotation(ctx, o.Client, client.ObjectKeyFromObject(sandbox), *envelope)
	if err != nil {
		return nil, err
	}
	return o.clearAssignmentProjection(ctx, updated, envelope, advanceInstance)
}

func (o *Orchestrator) clearAssignmentProjection(ctx context.Context, sandbox *apiv1alpha1.Sandbox, envelope *assignment.AssignmentEnvelope, advanceInstance bool) (*apiv1alpha1.Sandbox, error) {
	key := client.ObjectKeyFromObject(sandbox)
	var result *apiv1alpha1.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha1.Sandbox
		if err := o.Client.Get(ctx, key, &current); err != nil {
			return err
		}
		active, err := assignment.AssignmentFromAnnotation(&current)
		if err != nil {
			return err
		}
		if active != nil {
			return assignment.ErrAssignmentAnnotationChanged
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

func (o *Orchestrator) MarkRuntimeReadyDataPlaneCreating(ctx context.Context, sandbox *apiv1alpha1.Sandbox, phase string) error {
	return o.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateReady
		status.DataPlaneState = apiv1alpha1.ObservedStateCreating
		setCondition(status, ConditionRuntimeReady, metav1.ConditionTrue, "RuntimeRunning", "Fastlet reports the runtime and private network ready")
		setCondition(status, ConditionDataPlaneReady, metav1.ConditionFalse, "DataPlaneInitializing", "Fastlet data plane is "+phase)
	})
}

func (o *Orchestrator) MarkRuntimeReadyDataPlaneUnavailable(ctx context.Context, sandbox *apiv1alpha1.Sandbox, phase string) error {
	return o.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateReady
		status.DataPlaneState = apiv1alpha1.ObservedStateUnavailable
		setCondition(status, ConditionRuntimeReady, metav1.ConditionTrue, "RuntimeRunning", "Fastlet reports the runtime and private network ready")
		setCondition(status, ConditionDataPlaneReady, metav1.ConditionFalse, "DataPlaneUnavailable", "Fastlet data plane is "+phase+" and will be retried")
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
		if _, err := assignment.EffectiveAssignment(&current); err != nil {
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

func (o *Orchestrator) assignedTarget(sandbox *apiv1alpha1.Sandbox) (placement.FastletInfo, assignment.AssignmentEnvelope, fastletapi.SandboxIdentity, error) {
	if sandbox == nil || sandbox.UID == "" {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil {
		if !errors.Is(err, assignment.ErrAssignmentAnnotationMissing) || sandbox.Status.Assignment == nil {
			return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, err
		}
		// Status-only objects are an upgrade bridge. Existing runtimes recover the
		// same deterministic legacy identity; active reconcile persists it later.
		legacyFastlet, ok := o.Registry.GetFastletByID(placement.FastletID(sandbox.Status.Assignment.FastletName))
		if !ok || legacyFastlet.PodUID != sandbox.Status.Assignment.FastletPodUID || legacyFastlet.PodIP == "" {
			return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
		}
		legacy, legacyErr := AssignmentForCandidate(
			legacyFastlet, sandbox.Status.Assignment.Attempt,
			max(sandbox.Status.InstanceGeneration, apiv1alpha1.InitialInstanceGeneration),
			max(sandbox.Status.RouteGeneration, int64(1)), "legacy-"+string(sandbox.UID),
		)
		if legacyErr != nil {
			return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, legacyErr
		}
		identity := fastletapi.SandboxIdentity{
			RequestID: sandbox.Annotations[assignment.AnnotationRequestID], SandboxUID: string(sandbox.UID),
			InstanceGeneration: legacy.InstanceGeneration, RuntimeInstanceID: legacy.RuntimeInstanceID,
			AssignmentAttempt: legacy.Attempt, RouteGeneration: legacy.RouteGeneration, FastletPodUID: legacy.FastletPodUID,
		}
		return legacyFastlet, legacy, identity, nil
	}
	if envelope == nil {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	fastlet, ok := o.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !ok || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" ||
		fastlet.RuntimeProfileHash != envelope.RuntimeProfileHash || fastlet.ResourceProfileHash != envelope.ResourceProfileHash ||
		fastlet.InfraProfileHash != envelope.InfraProfileHash {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	identity := fastletapi.SandboxIdentity{
		RequestID: sandbox.Annotations[assignment.AnnotationRequestID], SandboxUID: string(sandbox.UID),
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
	var failure *fastletapi.FastletError
	if !errors.As(err, &failure) || failure.Outcome != fastletapi.OutcomeRejectedBeforeSideEffects {
		return false
	}
	switch failure.Code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorRuntimeUnavailable, fastletapi.ErrorNetworkUnavailable, fastletapi.ErrorInfraUnavailable, fastletapi.ErrorProfileMismatch:
		return true
	default:
		return false
	}
}

func (o *Orchestrator) RecordCandidateFeedback(id placement.FastletID, err error) {
	o.recordFeedback(id, err)
}

func (o *Orchestrator) recordFeedback(id placement.FastletID, err error) {
	var failure *fastletapi.FastletError
	if !errors.As(err, &failure) {
		return
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	o.Registry.RecordFeedback(id, placement.LocalFeedback{Code: failure.Code, ObservedAt: now})
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
	var fastletErr *fastletapi.FastletError
	return errors.As(err, &fastletErr) && fastletErr.Code == fastletapi.ErrorNotFound
}
