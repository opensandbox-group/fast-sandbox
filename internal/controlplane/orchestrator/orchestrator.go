package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
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
	ErrInvalidActionDesiredState  = errors.New("Sandbox Action desired state is invalid")
	ErrAssignedFastletUnavailable = errors.New("assigned Fastlet is unavailable or was replaced")
	ErrUnknownFastletOutcome      = errors.New("Fastlet operation outcome is unknown")
)

const (
	ConditionReady       = apiv1alpha2.SandboxConditionReady
	ReasonFastletPodLost = "FastletPodLost"
	ReasonExpired        = "Expired"
)

type FastletClient interface {
	CreateSandbox(context.Context, string, *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error)
	InspectSandbox(context.Context, string, *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error)
	DeleteSandbox(context.Context, string, *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error)
	CreateSnapshot(context.Context, string, *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error)
	InspectSnapshot(context.Context, string, *fastletapi.InspectSnapshotRequest) (*fastletapi.InspectSnapshotResponse, error)
	DeleteSnapshot(context.Context, string, *fastletapi.DeleteSnapshotRequest) (*fastletapi.DeleteSnapshotResponse, error)
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
	TopK          int
	Now           func() time.Time
}

// RuntimeParameters are used only by the declarative Controller to validate a
// Pool against the watched Fastlet profile. Fastlet remains authoritative for
// the concrete CPU/memory/PID values it injects into the runtime request.
type RuntimeParameters struct {
	RuntimeName         apiv1alpha2.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraRevision       string
	FastletRevision     string
}

func (o *Orchestrator) ResolveRuntime(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (RuntimeParameters, error) {
	if sandbox == nil {
		return RuntimeParameters{}, errors.New("Sandbox is required")
	}
	var pool apiv1alpha2.SandboxPool
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return RuntimeParameters{}, fmt.Errorf("get SandboxPool %s: %w", sandbox.Spec.PoolRef, err)
	}
	if err := pool.Spec.ValidateRuntime(); err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve Pool runtime: %w", err)
	}
	if err := pool.Spec.ValidateActionHandlers(); err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve Pool Action Handlers: %w", err)
	}
	if err := sandbox.Spec.ValidateActionBindings(pool.Spec.ActionHandlers); err != nil {
		return RuntimeParameters{}, err
	}
	catalog := o.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	profile, err := catalog.Resolve(pool.Spec.Runtime)
	if err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve runtime profile: %w", err)
	}
	if err := apiv1alpha2.ValidateSandboxResourceProfile(pool.Spec.SandboxResources); err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve Sandbox resources: %w", err)
	}
	infraPlan, err := infracatalog.Compile(pool.Spec.InfraComponents, profile)
	if err != nil {
		return RuntimeParameters{}, fmt.Errorf("compile Infra Components: %w", err)
	}
	runtimeProfileHash := strings.TrimPrefix(pool.Status.RuntimeRevision, "sha256:")
	if runtimeProfileHash == "" {
		// The Pool controller normally publishes the resolved environment
		// revision before a Fastlet can register. Retain the definition hash as
		// a compatibility fallback for unit tests and pre-v1alpha2 objects.
		runtimeProfileHash = profile.ProfileHash
	}
	return RuntimeParameters{
		RuntimeName: pool.Spec.Runtime, RuntimeProfileHash: runtimeProfileHash,
		ResourceProfileHash: pool.Spec.SandboxResources.Hash(),
		InfraRevision:       infraPlan.Revision,
		FastletRevision:     pool.Status.FastletRevision,
	}, nil
}

func (o *Orchestrator) Candidates(ctx context.Context, sandbox *apiv1alpha2.Sandbox, stableKey string) ([]placement.FastletInfo, RuntimeParameters, error) {
	parameters, err := o.ResolveRuntime(ctx, sandbox)
	if err != nil {
		return nil, RuntimeParameters{}, err
	}
	candidates := o.topK(placement.CandidateRequest{
		Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef,
		RuntimeName: parameters.RuntimeName, RuntimeProfileHash: parameters.RuntimeProfileHash,
		ResourceProfileHash: parameters.ResourceProfileHash, InfraRevision: parameters.InfraRevision,
		FastletRevision: parameters.FastletRevision,
		Image:           sandbox.Spec.Image, StableKey: stableKey,
	})
	if len(candidates) == 0 {
		return nil, parameters, ErrNoCandidate
	}
	return candidates, parameters, nil
}

// FastPathCandidates is intentionally registry-only. Calling it cannot issue
// a Kubernetes API request, which keeps the first-create happy path at two IOs.
func (o *Orchestrator) FastPathCandidates(sandbox *apiv1alpha2.Sandbox, stableKey string) ([]placement.FastletInfo, error) {
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
		InfraRevision: candidate.InfraRevision,
	}
	if err := envelope.Validate(); err != nil {
		return assignment.AssignmentEnvelope{}, err
	}
	if candidate.PodIP == "" || candidate.InfraRevision == "" {
		return assignment.AssignmentEnvelope{}, errors.New("candidate endpoint and Infra revision are required")
	}
	return envelope, nil
}

// AssignDeclarative preserves the standalone Controller deployment mode. It
// first honors any FastPath-written annotation, then performs Pool validation
// and registry selection only when no durable assignment exists.
func (o *Orchestrator) AssignDeclarative(ctx context.Context, sandbox *apiv1alpha2.Sandbox, stableKey string) (*apiv1alpha2.Sandbox, bool, error) {
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

	candidates, _, err := o.Candidates(ctx, sandbox, stableKey)
	if err != nil {
		return nil, false, err
	}
	runtimeInstanceID, err := idgen.GenerateRequestID()
	if err != nil {
		return nil, false, fmt.Errorf("generate runtime instance ID: %w", err)
	}
	attempt := sandbox.Status.Placement.Attempt + 1
	generation := max(sandbox.Status.Runtime.Generation, apiv1alpha2.InitialInstanceGeneration)
	routeGeneration := max(sandbox.Status.DataPlane.RouteGeneration, int64(1))
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
func (o *Orchestrator) ReassignDeclarativeAfterRejection(ctx context.Context, sandbox *apiv1alpha2.Sandbox, stableKey string) (*apiv1alpha2.Sandbox, bool, error) {
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
func (o *Orchestrator) CreateRuntime(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (*fastletapi.CreateSandboxResponse, error) {
	fastlet, envelope, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return nil, err
	}
	return o.createRuntimeOnTarget(ctx, sandbox, fastlet, envelope, identity, "")
}

// CreateRuntimeOnCandidate is used immediately after FastPath wins an
// annotation Create/CAS. The annotation is revalidated, while a concurrently
// stale status projection is deliberately ignored.
func (o *Orchestrator) CreateRuntimeOnCandidate(ctx context.Context, sandbox *apiv1alpha2.Sandbox, fastlet placement.FastletInfo, envelope assignment.AssignmentEnvelope) (*fastletapi.CreateSandboxResponse, error) {
	return o.CreateRuntimeOnCandidateWithCompletion(ctx, sandbox, fastlet, envelope, "")
}

func (o *Orchestrator) CreateRuntimeOnCandidateWithCompletion(ctx context.Context, sandbox *apiv1alpha2.Sandbox, fastlet placement.FastletInfo, envelope assignment.AssignmentEnvelope, completion fastletapi.CreateCompletion) (*fastletapi.CreateSandboxResponse, error) {
	current, err := assignment.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if current == nil || *current != envelope {
		return nil, assignment.ErrAssignmentAnnotationChanged
	}
	if fastlet.PodName != envelope.FastletName || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" ||
		fastlet.RuntimeProfileHash != envelope.RuntimeProfileHash || fastlet.ResourceProfileHash != envelope.ResourceProfileHash ||
		fastlet.InfraRevision != envelope.InfraRevision {
		return nil, ErrAssignedFastletUnavailable
	}
	identity := fastletapi.SandboxIdentity{
		SandboxUID: string(sandbox.UID), Namespace: sandbox.Namespace, Name: sandbox.Name,
		InstanceGeneration: envelope.InstanceGeneration, RuntimeInstanceID: envelope.RuntimeInstanceID,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration, FastletPodUID: envelope.FastletPodUID,
	}
	return o.createRuntimeOnTarget(ctx, sandbox, fastlet, envelope, identity, completion)
}

func (o *Orchestrator) createRuntimeOnTarget(ctx context.Context, sandbox *apiv1alpha2.Sandbox, fastlet placement.FastletInfo, envelope assignment.AssignmentEnvelope, identity fastletapi.SandboxIdentity, completion fastletapi.CreateCompletion) (*fastletapi.CreateSandboxResponse, error) {
	bindings, err := compileActionBindings(sandbox.Spec.ActionBindings)
	if err != nil {
		return nil, err
	}
	request := &fastletapi.CreateSandboxRequest{
		RequestID: sandbox.Annotations[assignment.AnnotationRequestID], Identity: identity,
		SpecGeneration: sandbox.Generation, ActionBindings: bindings, Completion: completion,
		Sandbox: fastletapi.SandboxSpec{
			Image:              sandbox.Spec.Image,
			RuntimeProfileHash: envelope.RuntimeProfileHash, ResourceProfileHash: envelope.ResourceProfileHash,
			InfraRevision: envelope.InfraRevision,
			Command:       sandbox.Spec.Command, Args: sandbox.Spec.Args, Env: envMap(sandbox.Spec.Envs), WorkingDir: sandbox.Spec.WorkingDir,
		},
	}
	response, createErr := o.FastletClient.CreateSandbox(ctx, fastlet.PodIP, request)
	// A Created observation may be runtime-Creating: the Fastlet parks cold
	// Sandboxes (asynchronous image delivery) and returns immediately. The
	// Controller observes the Sandbox until it converges, so this is an
	// accepted create, not a failure.
	if createErr == nil && response != nil &&
		(response.Disposition == fastletapi.CreateDispositionCreated || response.Disposition == fastletapi.CreateDispositionExisting) &&
		response.Sandbox != nil &&
		(response.Sandbox.Runtime.State == fastletapi.RuntimeStateReady || response.Sandbox.Runtime.State == fastletapi.RuntimeStateCreating) {
		return response, nil
	}
	if createErr == nil {
		createErr = ErrUnknownFastletOutcome
	} else {
		var failure *fastletapi.FastletError
		if !errors.As(createErr, &failure) {
			createErr = fmt.Errorf("%w: %v", ErrUnknownFastletOutcome, createErr)
		}
	}
	o.recordFeedback(fastlet.ID, createErr)
	return response, createErr
}

// EnsureRuntime performs the side-effecting create/ensure operation. A
// structured observation is returned whenever Fastlet can identify the local
// Sandbox, including normal Pending or unavailable states.
func (o *Orchestrator) EnsureRuntime(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (*fastletapi.SandboxStatus, error) {
	response, err := o.CreateRuntime(ctx, sandbox)
	if response != nil && response.Sandbox != nil {
		return response.Sandbox, nil
	}
	if err == nil {
		return nil, ErrUnknownFastletOutcome
	}
	return nil, err
}

// ReconcileBindings sends CRD-persisted desired input to the assigned Fastlet.
// Fastlet remains the only lifecycle Hook dispatcher.
func (o *Orchestrator) ReconcileBindings(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (*fastletapi.SandboxStatus, error) {
	var pool apiv1alpha2.SandboxPool
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return nil, fmt.Errorf("get SandboxPool for Actions: %w", err)
	}
	if err := pool.Spec.ValidateActionHandlers(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidActionDesiredState, err)
	}
	if err := sandbox.Spec.ValidateActionBindings(pool.Spec.ActionHandlers); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidActionDesiredState, err)
	}
	inputs, err := compileActionBindings(sandbox.Spec.ActionBindings)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidActionDesiredState, err)
	}
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return nil, err
	}
	client, ok := o.FastletClient.(interface {
		ReconcileBindings(context.Context, string, *fastletapi.ReconcileBindingsRequest) (*fastletapi.ReconcileBindingsResponse, error)
	})
	if !ok {
		return nil, errors.New("Fastlet client does not support Sandbox Actions")
	}
	response, callErr := client.ReconcileBindings(ctx, fastlet.PodIP, &fastletapi.ReconcileBindingsRequest{
		Identity: identity, SpecGeneration: sandbox.Generation, ActionBindings: inputs,
	})
	if response != nil && response.Sandbox != nil {
		return response.Sandbox, nil
	}
	if callErr != nil {
		return nil, callErr
	}
	return nil, ErrUnknownFastletOutcome
}

func (o *Orchestrator) ObserveRuntime(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (*fastletapi.SandboxStatus, error) {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return nil, err
	}
	response, inspectErr := o.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &fastletapi.InspectSandboxRequest{Identity: identity})
	if inspectErr != nil {
		return nil, inspectErr
	}
	if response == nil || response.Sandbox == nil {
		return nil, ErrUnknownFastletOutcome
	}
	return response.Sandbox, nil
}

func (o *Orchestrator) DeleteRuntime(ctx context.Context, sandbox *apiv1alpha2.Sandbox) error {
	fastlet, _, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return err
	}
	_, err = o.FastletClient.DeleteSandbox(ctx, fastlet.PodIP, &fastletapi.DeleteSandboxRequest{Identity: identity})
	return err
}

func (o *Orchestrator) RuntimeGone(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (bool, error) {
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

func (o *Orchestrator) ClearAssignment(ctx context.Context, sandbox *apiv1alpha2.Sandbox, advanceInstance bool) (*apiv1alpha2.Sandbox, error) {
	if sandbox == nil {
		return nil, errors.New("Sandbox is required")
	}
	envelope, err := assignment.AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if envelope == nil {
		if sandbox.Status.Placement.FastletName == "" {
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

func (o *Orchestrator) clearAssignmentProjection(ctx context.Context, sandbox *apiv1alpha2.Sandbox, envelope *assignment.AssignmentEnvelope, advanceInstance bool) (*apiv1alpha2.Sandbox, error) {
	key := client.ObjectKeyFromObject(sandbox)
	var result *apiv1alpha2.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha2.Sandbox
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
		if current.Status.Placement.FastletName == "" {
			result = current.DeepCopy()
			return nil
		}
		generation := max(current.Status.Runtime.Generation, apiv1alpha2.InitialInstanceGeneration)
		routeGeneration := max(current.Status.DataPlane.RouteGeneration, int64(1)) + 1
		if envelope != nil {
			generation = max(generation, envelope.InstanceGeneration)
			routeGeneration = max(routeGeneration, envelope.RouteGeneration+1)
		}
		if advanceInstance {
			generation = apiv1alpha2.NextInstanceGeneration(generation)
		}
		before := current.DeepCopy()
		attempt := current.Status.Placement.Attempt
		if envelope != nil {
			attempt = max(attempt, envelope.Attempt)
		}
		current.Status.Placement = apiv1alpha2.PlacementStatus{Attempt: attempt}
		current.Status.Runtime.Generation = generation
		current.Status.DataPlane.RouteGeneration = routeGeneration
		// This projection is deliberately fence-only. SandboxReconciler owns
		// Runtime/DataPlane/Ready business state and will publish the outcome of
		// expiration, reset, or recovery in its own status mutation.
		if err := o.Client.Status().Patch(ctx, &current, client.MergeFrom(before)); err != nil {
			return err
		}
		result = current.DeepCopy()
		return nil
	})
	return result, err
}

// ProjectObservedStatus is a pure projection used by SandboxReconciler while
// it owns the single business-status write for one reconcile pass.
func ProjectObservedStatus(status *apiv1alpha2.SandboxStatus, sandbox *apiv1alpha2.Sandbox, observed *fastletapi.SandboxStatus) {
	if status == nil || observed == nil {
		return
	}
	now := metav1.Now()
	setRuntimeStatus(&status.Runtime, apiv1alpha2.RuntimeState(observed.Runtime.State), observed.Runtime.Message, now)
	setDataPlaneStatus(&status.DataPlane, apiv1alpha2.DataPlaneState(observed.DataPlane.State), observed.DataPlane.Message, now)
	// ObservedGeneration follows the Kubernetes convention: the Controller has
	// processed this Spec generation, regardless of whether applying it is
	// Pending, Ready, or Failed. Fastlet's applied generation remains a live-only
	// convergence fence and is deliberately not duplicated into CRD status.
	status.ObservedGeneration = sandbox.Generation
	if observed.Runtime.State == fastletapi.RuntimeStateReady {
		status.Placement.Recovery = nil
	}

	previous := make(map[string]apiv1alpha2.InfraComponentStatus, len(status.InfraComponents))
	for _, component := range status.InfraComponents {
		previous[component.Name] = component
	}
	components := make([]apiv1alpha2.InfraComponentStatus, 0, len(observed.InfraComponents))
	for _, diagnostic := range observed.InfraComponents {
		state := apiv1alpha2.InfraComponentStarting
		switch diagnostic.State {
		case "Ready":
			state = apiv1alpha2.InfraComponentReady
		case "Failed":
			state = apiv1alpha2.InfraComponentFailed
		}
		component := apiv1alpha2.InfraComponentStatus{Name: diagnostic.Component, State: state, Message: diagnostic.Message}
		if old, found := previous[component.Name]; found && old.State == component.State && old.Message == component.Message {
			component.LastTransitionTime = old.LastTransitionTime
		} else {
			component.LastTransitionTime = &now
		}
		components = append(components, component)
	}
	status.InfraComponents = components
	previousActions := make(map[string]apiv1alpha2.ActionBindingStatus, len(status.ActionBindings))
	for _, action := range status.ActionBindings {
		previousActions[action.Handler] = action
	}
	actions := make([]apiv1alpha2.ActionBindingStatus, 0, len(observed.ActionBindings))
	for _, observedAction := range observed.ActionBindings {
		action := apiv1alpha2.ActionBindingStatus{
			Handler: observedAction.Handler, State: apiv1alpha2.ActionState(observedAction.State), Message: observedAction.Message,
		}
		currentGeneration := observedAction.ObservedSpecGeneration == sandbox.Generation ||
			(observedAction.ObservedSpecGeneration == 0 && observed.AppliedGeneration == sandbox.Generation)
		if !currentGeneration {
			action.State = apiv1alpha2.ActionPending
			action.Message = "Action Binding has not applied the current Sandbox generation"
		}
		old, found := previousActions[action.Handler]
		samePublicState := found && old.State == action.State && old.Message == action.Message
		// Fastlet owns the Binding lifecycle and can observe Ready -> Applying ->
		// Ready between Controller polls. Prefer its newer transition time even
		// when the final public state and message are unchanged. An observation
		// from an older Spec generation must not advance or roll back the current
		// Binding timestamp.
		if currentGeneration && !observedAction.LastTransitionTime.IsZero() {
			transition := metav1.NewTime(observedAction.LastTransitionTime)
			if !found || old.LastTransitionTime == nil || transition.After(old.LastTransitionTime.Time) {
				action.LastTransitionTime = &transition
			}
		}
		if action.LastTransitionTime == nil {
			if samePublicState {
				action.LastTransitionTime = old.LastTransitionTime
			} else {
				action.LastTransitionTime = &now
			}
		}
		actions = append(actions, action)
	}
	status.ActionBindings = actions
	ready, reason, message := overallReady(sandbox, status)
	if ready && observed.AppliedGeneration != sandbox.Generation {
		ready, reason, message = false, "GenerationNotApplied", "Fastlet has not applied the current Sandbox generation"
	}
	setReadyCondition(status, sandbox.Generation, ready, reason, message)
}

func setRuntimeStatus(status *apiv1alpha2.RuntimeStatus, state apiv1alpha2.RuntimeState, message string, now metav1.Time) {
	if status.State != state || status.Message != message || status.LastTransitionTime == nil {
		status.LastTransitionTime = &now
	}
	status.State, status.Message = state, message
}

func setDataPlaneStatus(status *apiv1alpha2.DataPlaneStatus, state apiv1alpha2.DataPlaneState, message string, now metav1.Time) {
	if status.State != state || status.Message != message || status.LastTransitionTime == nil {
		status.LastTransitionTime = &now
	}
	status.State, status.Message = state, message
}

func overallReady(sandbox *apiv1alpha2.Sandbox, status *apiv1alpha2.SandboxStatus) (bool, string, string) {
	if status.Runtime.State != apiv1alpha2.RuntimeReady {
		return false, "RuntimeNotReady", status.Runtime.Message
	}
	if status.DataPlane.State != apiv1alpha2.DataPlaneReady {
		return false, "DataPlaneNotReady", status.DataPlane.Message
	}
	for _, component := range status.InfraComponents {
		if component.State != apiv1alpha2.InfraComponentReady {
			return false, "InfraComponentNotReady", component.Name + " is " + string(component.State)
		}
	}
	if len(status.ActionBindings) != len(sandbox.Spec.ActionBindings) {
		return false, "ActionBindingNotReady", "Action Binding status has not converged"
	}
	for index, binding := range sandbox.Spec.ActionBindings {
		observed := status.ActionBindings[index]
		if observed.Handler != binding.Handler || observed.State != apiv1alpha2.ActionReady {
			return false, "ActionBindingNotReady", binding.Handler + " is not Ready"
		}
	}
	if status.ObservedGeneration != sandbox.Generation {
		return false, "GenerationNotObserved", "Fastlet has not applied the current Sandbox generation"
	}
	return true, "Ready", "Runtime, data plane, Infra Components, and Action Bindings are Ready"
}

func (o *Orchestrator) assignedTarget(sandbox *apiv1alpha2.Sandbox) (placement.FastletInfo, assignment.AssignmentEnvelope, fastletapi.SandboxIdentity, error) {
	if sandbox == nil || sandbox.UID == "" {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, err
	}
	if envelope == nil {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	fastlet, ok := o.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !ok || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" ||
		fastlet.RuntimeProfileHash != envelope.RuntimeProfileHash || fastlet.ResourceProfileHash != envelope.ResourceProfileHash ||
		fastlet.InfraRevision != envelope.InfraRevision {
		return placement.FastletInfo{}, assignment.AssignmentEnvelope{}, fastletapi.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	identity := fastletapi.SandboxIdentity{
		SandboxUID: string(sandbox.UID), Namespace: sandbox.Namespace, Name: sandbox.Name,
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
	if !errors.As(err, &failure) || fastletapi.CreateDispositionFromError(err) != fastletapi.CreateDispositionRejectedBeforeSideEffects {
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

func setReadyCondition(status *apiv1alpha2.SandboxStatus, generation int64, ready bool, reason, message string) {
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: conditionStatus, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.Now(),
	})
}

func compileActionBindings(bindings []apiv1alpha2.ActionBinding) ([]fastletapi.ActionBindingInput, error) {
	result := make([]fastletapi.ActionBindingInput, 0, len(bindings))
	total := 0
	for _, binding := range bindings {
		total += len(binding.Input)
		if len(binding.Input) > apiv1alpha2.MaxActionBindingInputBytes || total > apiv1alpha2.MaxSandboxActionBindingInputBytes {
			return nil, fmt.Errorf("Action Binding inputs exceed configured size limits")
		}
		result = append(result, fastletapi.ActionBindingInput{Handler: binding.Handler, Input: binding.Input})
	}
	return result, nil
}

func IsNotFound(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	var fastletErr *fastletapi.FastletError
	return errors.As(err, &fastletErr) && fastletErr.Code == fastletapi.ErrorNotFound
}
