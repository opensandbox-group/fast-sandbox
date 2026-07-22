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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrNoCandidate                = errors.New("no Fastlet accepted the Sandbox request")
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
	ReserveSandbox(context.Context, string, *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error)
	CancelReservation(context.Context, string, *api.CancelReservationRequest) (*api.CancelReservationResponse, error)
	EnsureSandbox(context.Context, string, *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error)
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

type RuntimeParameters struct {
	RuntimeName         apiv1alpha1.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraProfile        string
	InfraProfileHash    string
	CPU                 string
	Memory              string
	PIDs                int64
}

type Reservation struct {
	Fastlet        fastletpool.FastletInfo
	RequestID      string
	CreateSpecHash string
	Token          string
	ExpiresAt      time.Time
	Parameters     RuntimeParameters
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
	runtimeName := pool.Spec.Runtime
	catalog := o.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	profile, err := catalog.Resolve(runtimeName)
	if err != nil {
		return RuntimeParameters{}, fmt.Errorf("resolve runtime profile: %w", err)
	}
	resources := pool.Spec.SandboxResources
	if err := apiv1alpha1.ValidateSandboxResourceProfile(resources); err != nil {
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
		RuntimeName: runtimeName, RuntimeProfileHash: profile.ProfileHash,
		ResourceProfileHash: resources.Hash(), CPU: resources.CPU.String(),
		Memory: resources.Memory.String(), PIDs: resources.PIDs,
		InfraProfile: infraPlan.ProfileName, InfraProfileHash: infraPlan.ProfileHash,
	}, nil
}

func (o *Orchestrator) Candidates(ctx context.Context, sandbox *apiv1alpha1.Sandbox, stableKey string) ([]fastletpool.FastletInfo, RuntimeParameters, error) {
	parameters, err := o.ResolveRuntime(ctx, sandbox)
	if err != nil {
		return nil, RuntimeParameters{}, err
	}
	k := o.TopK
	if k <= 0 {
		k = 3
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	candidates := o.Registry.TopK(fastletpool.CandidateRequest{
		Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef,
		RuntimeName: parameters.RuntimeName, RuntimeProfileHash: parameters.RuntimeProfileHash,
		ResourceProfileHash: parameters.ResourceProfileHash, Image: sandbox.Spec.Image,
		InfraProfileHash: parameters.InfraProfileHash,
		StableKey:        stableKey, Now: now,
	}, k)
	if len(candidates) == 0 {
		return nil, parameters, ErrNoCandidate
	}
	return candidates, parameters, nil
}

// ReserveForCreate is the RPC-only admission gate. It never writes a CRD.
func (o *Orchestrator) ReserveForCreate(ctx context.Context, sandbox *apiv1alpha1.Sandbox, requestID, createSpecHash string) (*Reservation, error) {
	candidates, parameters, err := o.Candidates(ctx, sandbox, requestID)
	if err != nil {
		return nil, err
	}
	for index, candidate := range candidates {
		if index > 0 {
			recordTopKRetry("attempt")
		}
		response, reserveErr := o.FastletClient.ReserveSandbox(ctx, candidate.PodIP, &api.ReserveSandboxRequest{
			RequestID: requestID, CreateSpecHash: createSpecHash,
			ClaimNamespace: sandbox.Namespace, ClaimName: sandbox.Name, FastletPodUID: candidate.PodUID,
			RuntimeProfileHash: parameters.RuntimeProfileHash, ResourceProfileHash: parameters.ResourceProfileHash,
			InfraProfileHash: parameters.InfraProfileHash,
		})
		if reserveErr == nil && response != nil && response.ReservationToken != "" && response.FastletPodUID == candidate.PodUID {
			if index > 0 {
				recordTopKRetry("accepted")
			}
			return &Reservation{
				Fastlet: candidate, RequestID: requestID, CreateSpecHash: createSpecHash,
				Token: response.ReservationToken, ExpiresAt: response.ExpiresAt, Parameters: parameters,
			}, nil
		}
		if reserveErr == nil {
			reserveErr = ErrUnknownFastletOutcome
		}
		if isCandidateRejection(reserveErr) {
			recordTopKRetry("candidate_rejected")
			o.recordFeedback(candidate.ID, reserveErr)
			continue
		}
		return nil, fmt.Errorf("reserve Fastlet %s: %w", candidate.ID, reserveErr)
	}
	return nil, ErrNoCandidate
}

func (o *Orchestrator) CancelReservation(ctx context.Context, reservation *Reservation) error {
	if reservation == nil || reservation.Token == "" {
		return nil
	}
	_, err := o.FastletClient.CancelReservation(ctx, reservation.Fastlet.PodIP, &api.CancelReservationRequest{
		RequestID: reservation.RequestID, ReservationToken: reservation.Token,
	})
	return err
}

// EnsureAssignment uses status CAS. If another active replica wins with a
// different candidate, that durable winner is returned and must be used.
func (o *Orchestrator) EnsureAssignment(ctx context.Context, sandbox *apiv1alpha1.Sandbox, candidate fastletpool.FastletInfo) (*apiv1alpha1.Sandbox, bool, error) {
	if sandbox.Status.Assignment != nil {
		return sandbox.DeepCopy(), false, nil
	}
	desired := apiv1alpha1.SandboxAssignment{
		FastletName: candidate.PodName, FastletPodUID: candidate.PodUID, NodeName: candidate.NodeName,
	}
	assigned, err := common.EnsureSandboxAssignment(ctx, o.Client, client.ObjectKeyFromObject(sandbox), desired)
	if err == nil {
		return assigned, true, nil
	}
	if !errors.Is(err, common.ErrAssignmentConflict) {
		return nil, false, err
	}
	var winner apiv1alpha1.Sandbox
	if getErr := o.Client.Get(ctx, client.ObjectKeyFromObject(sandbox), &winner); getErr != nil {
		return nil, false, errors.Join(err, getErr)
	}
	if winner.Status.Assignment == nil {
		return nil, false, err
	}
	return winner.DeepCopy(), false, nil
}

func (o *Orchestrator) AssignDeclarative(ctx context.Context, sandbox *apiv1alpha1.Sandbox, stableKey string) (*apiv1alpha1.Sandbox, bool, error) {
	if sandbox.Status.Assignment != nil {
		return sandbox.DeepCopy(), false, nil
	}
	candidates, _, err := o.Candidates(ctx, sandbox, stableKey)
	if err != nil {
		return nil, false, err
	}
	return o.EnsureAssignment(ctx, sandbox, candidates[0])
}

func (o *Orchestrator) EnsureRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox, reservation *Reservation) error {
	if sandbox == nil || sandbox.Status.Assignment == nil || sandbox.UID == "" {
		return errors.New("persisted Sandbox UID and assignment are required")
	}
	assignment := *sandbox.Status.Assignment
	fastlet, ok := o.Registry.GetFastletByID(fastletpool.FastletID(assignment.FastletName))
	if !ok || fastlet.PodUID != assignment.FastletPodUID || fastlet.PodIP == "" {
		return ErrAssignedFastletUnavailable
	}
	parameters, err := o.ResolveRuntime(ctx, sandbox)
	if err != nil {
		return err
	}
	requestID := sandbox.Annotations[common.AnnotationRequestID]
	createSpecHash := sandbox.Annotations[common.AnnotationCreateSpecHash]
	identity := identityFor(sandbox)
	request := &api.EnsureSandboxRequest{
		Identity: identity, CreateSpecHash: createSpecHash,
		Sandbox: api.SandboxSpec{
			SandboxID: string(sandbox.UID), RequestID: requestID, ClaimUID: string(sandbox.UID),
			ClaimNamespace: sandbox.Namespace, ClaimName: sandbox.Name,
			RouteGeneration: identity.RouteGeneration,
			Image:           sandbox.Spec.Image, CPU: parameters.CPU, Memory: parameters.Memory, PIDs: parameters.PIDs,
			RuntimeProfileHash: parameters.RuntimeProfileHash, ResourceProfileHash: parameters.ResourceProfileHash,
			InfraProfile: parameters.InfraProfile, InfraProfileHash: parameters.InfraProfileHash,
			Command: sandbox.Spec.Command, Args: sandbox.Spec.Args, Env: envMap(sandbox.Spec.Envs), WorkingDir: sandbox.Spec.WorkingDir,
		},
	}
	if reservation != nil {
		if reservation.Fastlet.PodUID != assignment.FastletPodUID {
			return errors.New("reservation does not match durable assignment")
		}
		request.ReservationToken = reservation.Token
		request.CreateSpecHash = reservation.CreateSpecHash
	}
	response, ensureErr := o.FastletClient.EnsureSandbox(ctx, fastlet.PodIP, request)
	if ensureErr == nil && response != nil && response.Accepted && response.Sandbox != nil && response.Sandbox.Phase == "running" {
		return o.MarkReady(ctx, sandbox)
	}
	if ensureErr == nil {
		ensureErr = ErrUnknownFastletOutcome
	}
	var fastletErr *api.FastletError
	if errors.As(ensureErr, &fastletErr) {
		o.recordFeedback(fastlet.ID, ensureErr)
		if fastletErr.Code == api.ErrorInProgress {
			_ = o.MarkCreating(ctx, sandbox, fastletErr.Message)
			return ErrRuntimeInProgress
		}
		return ensureErr
	}
	// A lost response is never a reason to choose a second Fastlet. Inspect the
	// durable assignment once and let the caller retry the same identity.
	if observeErr := o.ObserveRuntime(ctx, sandbox); observeErr == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrUnknownFastletOutcome, ensureErr)
}

func (o *Orchestrator) ObserveRuntime(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	fastlet, identity, err := o.assignedTarget(sandbox)
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
	fastlet, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return err
	}
	_, err = o.FastletClient.DeleteSandboxV2(ctx, fastlet.PodIP, &api.DeleteSandboxV2Request{Identity: identity})
	return err
}

func (o *Orchestrator) RuntimeGone(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (bool, error) {
	fastlet, identity, err := o.assignedTarget(sandbox)
	if err != nil {
		return errors.Is(err, ErrAssignedFastletUnavailable), err
	}
	response, err := o.FastletClient.InspectSandbox(ctx, fastlet.PodIP, &api.InspectSandboxRequest{Identity: identity})
	if err != nil {
		var fastletErr *api.FastletError
		if errors.As(err, &fastletErr) && fastletErr.Code == api.ErrorNotFound {
			return true, nil
		}
		return false, err
	}
	return response == nil || response.Sandbox == nil, nil
}

func (o *Orchestrator) ClearAssignment(ctx context.Context, sandbox *apiv1alpha1.Sandbox, advanceInstance bool) (*apiv1alpha1.Sandbox, error) {
	if sandbox.Status.Assignment == nil {
		return sandbox.DeepCopy(), nil
	}
	return common.ClearSandboxAssignment(ctx, o.Client, client.ObjectKeyFromObject(sandbox), *sandbox.Status.Assignment, advanceInstance)
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
		before := current.DeepCopy().Status
		mutate(&current.Status)
		if reflect.DeepEqual(before, current.Status) {
			return nil
		}
		return o.Client.Status().Update(ctx, &current)
	})
}

func (o *Orchestrator) assignedTarget(sandbox *apiv1alpha1.Sandbox) (fastletpool.FastletInfo, api.SandboxIdentity, error) {
	if sandbox == nil || sandbox.Status.Assignment == nil || sandbox.UID == "" {
		return fastletpool.FastletInfo{}, api.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	assignment := sandbox.Status.Assignment
	fastlet, ok := o.Registry.GetFastletByID(fastletpool.FastletID(assignment.FastletName))
	if !ok || fastlet.PodUID != assignment.FastletPodUID || fastlet.PodIP == "" {
		return fastletpool.FastletInfo{}, api.SandboxIdentity{}, ErrAssignedFastletUnavailable
	}
	return fastlet, identityFor(sandbox), nil
}

func identityFor(sandbox *apiv1alpha1.Sandbox) api.SandboxIdentity {
	generation := sandbox.Status.InstanceGeneration
	if generation < apiv1alpha1.InitialInstanceGeneration {
		generation = apiv1alpha1.InitialInstanceGeneration
	}
	return api.SandboxIdentity{
		RequestID: sandbox.Annotations[common.AnnotationRequestID], SandboxUID: string(sandbox.UID),
		InstanceGeneration: generation, AssignmentAttempt: sandbox.Status.Assignment.Attempt,
		RouteGeneration: routeGenerationFor(sandbox),
		FastletPodUID:   sandbox.Status.Assignment.FastletPodUID,
	}
}

func routeGenerationFor(sandbox *apiv1alpha1.Sandbox) int64 {
	if sandbox.Status.RouteGeneration > 0 {
		return sandbox.Status.RouteGeneration
	}
	return 1
}

func envMap(values []corev1.EnvVar) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.Name] = value.Value
	}
	return result
}

func isCandidateRejection(err error) bool {
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
