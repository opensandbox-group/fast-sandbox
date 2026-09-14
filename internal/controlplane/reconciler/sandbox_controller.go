package reconciler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	FinalizerName           = "sandbox.fast.io/cleanup"
	DefaultRequeueInterval  = 5 * time.Second
	ReadyRequeueInterval    = 30 * time.Second
	DeletionPollInterval    = time.Second
	ObservationPollInterval = time.Second
)

type SandboxReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Orchestrator *orchestration.Orchestrator
	Now          func() time.Time
	// MaxConcurrentReconciles is the controller-runtime worker count for
	// Sandbox reconciles. Different Sandboxes are disjoint objects, and
	// the workqueue guarantees a single in-flight reconcile per key, so
	// raising it past 1 only parallelizes independent sandboxes (the
	// per-sandbox control-plane cycle dominates batch ready latency).
	// 0 keeps controller-runtime's default (1).
	MaxConcurrentReconciles int
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, request ctrl.Request) (_ ctrl.Result, resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: request.Namespace, SandboxName: request.Name})
	ctx, span := observability.Start(ctx, "controller.reconcile Sandbox")
	defer func() { observability.End(span, resultErr) }()
	var sandbox apiv1alpha2.Sandbox
	if err := r.Get(ctx, request.NamespacedName, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = observability.WithIdentity(ctx, sandboxObservabilityIdentity(&sandbox))
	if r.Orchestrator == nil {
		return ctrl.Result{}, errors.New("Sandbox orchestrator is not configured")
	}
	orchestrator := r.Orchestrator

	if sandbox.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, orchestrator, &sandbox)
	}
	if !controllerutil.ContainsFinalizer(&sandbox, FinalizerName) {
		controllerutil.AddFinalizer(&sandbox, FinalizerName)
		if err := r.Update(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if resetPending(&sandbox) {
		return r.reconcileReset(ctx, orchestrator, &sandbox)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if expirationPending(&sandbox, now) {
		return r.reconcileExpiration(ctx, orchestrator, &sandbox)
	}
	if sandbox.Spec.State == apiv1alpha2.SandboxStatePaused {
		return r.reconcilePause(ctx, orchestrator, &sandbox)
	}
	return r.reconcileEnsure(ctx, orchestrator, &sandbox)
}

func sandboxObservabilityIdentity(sandbox *apiv1alpha2.Sandbox) observability.Identity {
	identity := observability.Identity{
		RequestID: sandbox.Annotations[assignment.AnnotationRequestID], Namespace: sandbox.Namespace, SandboxName: sandbox.Name,
		SandboxUID: string(sandbox.UID), InstanceGeneration: sandbox.Status.Runtime.Generation, RouteGeneration: sandbox.Status.DataPlane.RouteGeneration,
	}
	if sandbox.Status.Placement.FastletName != "" {
		identity.FastletPodUID = string(sandbox.Status.Placement.FastletPodUID)
		identity.AssignmentAttempt = sandbox.Status.Placement.Attempt
	}
	return identity
}

func (r *SandboxReconciler) reconcileEnsure(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	if sandbox.Status.Placement.FastletName != "" {
		lost, err := r.assignedPodLost(ctx, sandbox)
		if err != nil {
			return ctrl.Result{}, err
		}
		if lost {
			return r.reconcilePodLost(ctx, orchestrator, sandbox)
		}
	}
	assigned, newlyAssigned, err := orchestrator.AssignDeclarative(ctx, sandbox, string(sandbox.UID))
	if err != nil {
		if errors.Is(err, orchestration.ErrNoCandidate) {
			_ = r.markPending(ctx, sandbox, "NoCandidate", "No Ready Fastlet currently accepts this Pool/profile")
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		if errors.Is(err, orchestration.ErrAssignedFastletUnavailable) {
			if statusErr := r.markAssignedFastletUnavailable(ctx, sandbox); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		return ctrl.Result{}, err
	}

	var observed *fastletapi.SandboxStatus
	if newlyAssigned {
		observed, err = orchestrator.EnsureRuntime(ctx, assigned)
	} else {
		observed, err = orchestrator.ObserveRuntime(ctx, assigned)
		if orchestration.IsNotFound(err) {
			observed, err = orchestrator.EnsureRuntime(ctx, assigned)
		}
	}
	if err != nil {
		if errors.Is(err, orchestration.ErrAssignedFastletUnavailable) {
			if statusErr := r.markAssignedFastletUnavailable(ctx, assigned); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		if explicitReschedule(err) {
			// A CRD-first assignment is a durable creation fact. Move directly
			// from the rejected identity to another eligible candidate with one
			// CAS; if none exists, preserve the current identity until capacity
			// or a new Fastlet becomes visible.
			_, moved, reassignErr := orchestrator.ReassignDeclarativeAfterRejection(ctx, assigned, string(assigned.UID))
			if reassignErr != nil {
				return ctrl.Result{}, reassignErr
			}
			if moved {
				// Status still projects the previous annotation here. Requeue
				// immediately so the normal annotation-to-status projection runs
				// before the new identity is sent to Fastlet.
				return ctrl.Result{Requeue: true}, nil
			}
			_ = r.markPending(ctx, assigned, "FastletRejected", err.Error())
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		var failure *fastletapi.FastletError
		if errors.As(err, &failure) && failure.Code == fastletapi.ErrorInProgress {
			_ = r.markCreating(ctx, assigned, failure.Message)
			return ctrl.Result{RequeueAfter: ObservationPollInterval}, nil
		}
		if errors.Is(err, orchestration.ErrUnknownFastletOutcome) {
			// The durable assignment is retained. A later local observation or
			// idempotent Ensure resolves whether the runtime was created.
			return ctrl.Result{RequeueAfter: ObservationPollInterval}, nil
		}
		return ctrl.Result{RequeueAfter: ObservationPollInterval}, err
	}
	if observed == nil {
		return ctrl.Result{RequeueAfter: ObservationPollInterval}, orchestration.ErrUnknownFastletOutcome
	}

	// Initial Create already registers Bindings. Rehydrate desired state only
	// when Inspect proves that this Fastlet has not accepted the current Spec.
	if observed.Runtime.State == fastletapi.RuntimeStateReady && observed.AcceptedGeneration < assigned.Generation {
		observed, err = orchestrator.ReconcileBindings(ctx, assigned)
		if err != nil {
			if errors.Is(err, orchestration.ErrInvalidActionDesiredState) {
				if statusErr := r.markActionDesiredStateInvalid(ctx, assigned, err.Error()); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
			}
			return ctrl.Result{RequeueAfter: ObservationPollInterval}, err
		}
	}
	if err := r.patchStatus(ctx, assigned, func(status *apiv1alpha2.SandboxStatus) {
		orchestration.ProjectObservedStatus(status, assigned, observed)
		// A resume in flight is a pause-FSM state: while the restore has not
		// reached Ready the Sandbox reads Resuming, and a completed resume
		// clears the one-shot checkpoint.
		projectResumeFromCheckpoint(status, assigned, observed)
	}); err != nil {
		return ctrl.Result{}, err
	}
	if newlyAssigned {
		klog.FromContext(ctx).Info("Sandbox assigned and runtime ensured", "sandbox", sandbox.Name, "fastlet", assigned.Status.Placement.FastletName)
	}
	if observationReady(assigned, observed) {
		return ctrl.Result{RequeueAfter: ReadyRequeueInterval}, nil
	}
	return ctrl.Result{RequeueAfter: ObservationPollInterval}, nil
}

func (r *SandboxReconciler) assignedPodLost(ctx context.Context, sandbox *apiv1alpha2.Sandbox) (bool, error) {
	if sandbox == nil || sandbox.Status.Placement.FastletName == "" {
		return false, nil
	}
	placement := sandbox.Status.Placement
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: placement.FastletName}, &pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return pod.UID != placement.FastletPodUID || pod.DeletionTimestamp != nil, nil
}

func (r *SandboxReconciler) markAssignedFastletUnavailable(ctx context.Context, sandbox *apiv1alpha2.Sandbox) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		if status.Runtime.Checkpoint != nil {
			// A resume whose target Fastlet is temporarily unavailable stays
			// visibly a resume (the checkpoint lineage is preserved).
			setControllerStates(status, apiv1alpha2.RuntimeResuming, apiv1alpha2.DataPlanePending, "The assigned Fastlet Pod still exists, but its local registry endpoint is temporarily unavailable")
			setSandboxReadyCondition(status, sandbox.Generation, "FastletRegistryPending", "The assigned Fastlet Pod still exists, but its local registry endpoint is temporarily unavailable")
			setSandboxSuspendedCondition(status, sandbox.Generation, false, "Resuming", "Sandbox resume is waiting for the assigned Fastlet")
			return
		}
		setControllerStates(status, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.DataPlaneUnavailable, "The assigned Fastlet Pod still exists, but its local registry endpoint is temporarily unavailable")
		setSandboxReadyCondition(status, sandbox.Generation, "FastletRegistryPending", "The assigned Fastlet Pod still exists, but its local registry endpoint is temporarily unavailable")
	})
}

func (r *SandboxReconciler) markPending(ctx context.Context, sandbox *apiv1alpha2.Sandbox, reason, message string) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		if status.Runtime.Checkpoint != nil {
			// A resume waiting for placement must not read as a fresh
			// create: keep the checkpoint lineage visible.
			setControllerStates(status, apiv1alpha2.RuntimeResuming, apiv1alpha2.DataPlanePending, message)
			setSandboxReadyCondition(status, sandbox.Generation, reason, message)
			setSandboxSuspendedCondition(status, sandbox.Generation, false, "Resuming", message)
			return
		}
		setControllerStates(status, apiv1alpha2.RuntimePending, apiv1alpha2.DataPlanePending, message)
		setSandboxReadyCondition(status, sandbox.Generation, reason, message)
	})
}

func (r *SandboxReconciler) markCreating(ctx context.Context, sandbox *apiv1alpha2.Sandbox, message string) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		setControllerStates(status, apiv1alpha2.RuntimeCreating, apiv1alpha2.DataPlanePending, message)
		setSandboxReadyCondition(status, sandbox.Generation, "RuntimeCreating", message)
	})
}

func (r *SandboxReconciler) markActionDesiredStateInvalid(ctx context.Context, sandbox *apiv1alpha2.Sandbox, message string) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		now := metav1.Now()
		status.ActionBindings = make([]apiv1alpha2.ActionBindingStatus, 0, len(sandbox.Spec.ActionBindings))
		for _, binding := range sandbox.Spec.ActionBindings {
			transition := now
			status.ActionBindings = append(status.ActionBindings, apiv1alpha2.ActionBindingStatus{
				Handler: binding.Handler, State: apiv1alpha2.ActionFailed, LastTransitionTime: &transition, Message: message,
			})
		}
		setSandboxReadyCondition(status, sandbox.Generation, "ActionBindingInvalid", message)
	})
}

func observationReady(sandbox *apiv1alpha2.Sandbox, observed *fastletapi.SandboxStatus) bool {
	if sandbox == nil || observed == nil || observed.Runtime.State != fastletapi.RuntimeStateReady ||
		observed.DataPlane.State != fastletapi.DataPlaneStateReady || observed.AppliedGeneration != sandbox.Generation ||
		len(observed.ActionBindings) != len(sandbox.Spec.ActionBindings) {
		return false
	}
	for _, component := range observed.InfraComponents {
		if component.State != string(apiv1alpha2.InfraComponentReady) {
			return false
		}
	}
	for index, binding := range sandbox.Spec.ActionBindings {
		if observed.ActionBindings[index].Handler != binding.Handler || observed.ActionBindings[index].State != string(apiv1alpha2.ActionReady) {
			return false
		}
	}
	return true
}

func (r *SandboxReconciler) reconcilePodLost(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	timeout := time.Duration(sandbox.Spec.RecoveryTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if sandbox.Status.Placement.Recovery == nil {
		deadline := now.Add(timeout)
		err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
			setControllerStates(status, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.DataPlaneUnavailable, "The assigned Fastlet Pod is lost; recovery delay is active")
			status.Placement.Recovery = &apiv1alpha2.RecoveryStatus{
				DetectedAt: metav1.NewTime(now), Deadline: metav1.NewTime(deadline),
			}
			setSandboxReadyCondition(status, sandbox.Generation, orchestration.ReasonFastletPodLost, "The assigned Fastlet Pod is lost; recovery delay is active")
		})
		return ctrl.Result{RequeueAfter: timeout}, err
	}
	if now.Before(sandbox.Status.Placement.Recovery.Deadline.Time) {
		return ctrl.Result{RequeueAfter: sandbox.Status.Placement.Recovery.Deadline.Sub(now)}, nil
	}
	if sandbox.Spec.FailurePolicy == apiv1alpha2.FailurePolicyAutoRecreate {
		if sandbox.Status.Placement.FastletName != "" {
			if _, err := orchestrator.ClearAssignment(ctx, sandbox, true); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true}, nil
	}
	err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		setControllerStates(status, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.DataPlaneUnavailable, "The assigned Fastlet Pod is lost and Manual failure policy requires operator action")
		setSandboxReadyCondition(status, sandbox.Generation, orchestration.ReasonFastletPodLost, "The assigned Fastlet Pod is lost and Manual failure policy requires operator action")
	})
	return ctrl.Result{}, err
}

func (r *SandboxReconciler) reconcileDeletion(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, FinalizerName) {
		return ctrl.Result{}, nil
	}
	done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
	if err != nil {
		return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
	}
	if !done {
		_ = r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
			setControllerStates(status, apiv1alpha2.RuntimeStopping, apiv1alpha2.DataPlaneDraining, "Sandbox deletion is in progress")
			setSandboxReadyCondition(status, sandbox.Generation, "Deleting", "Sandbox deletion is in progress")
		})
		return ctrl.Result{RequeueAfter: DeletionPollInterval}, nil
	}
	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha2.Sandbox
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		controllerutil.RemoveFinalizer(&current, FinalizerName)
		return r.Update(ctx, &current)
	})
}

func (r *SandboxReconciler) reconcileExpiration(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
	}
	if sandbox.Status.Placement.FastletName != "" {
		cleared, err := orchestrator.ClearAssignment(ctx, sandbox, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		sandbox = cleared
	}
	err = r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		// An expired Sandbox is terminal: a recorded checkpoint cannot be
		// resumed any more. Store objects stay for store-lifecycle GC.
		status.Runtime.Checkpoint = nil
		status.Runtime.PauseAttempt = 0
		setControllerStates(status, apiv1alpha2.RuntimeStopped, apiv1alpha2.DataPlaneUnavailable, "Sandbox desired lifetime expired")
		setSandboxReadyCondition(status, sandbox.Generation, orchestration.ReasonExpired, "Sandbox desired lifetime expired")
		setSandboxSuspendedCondition(status, sandbox.Generation, false, orchestration.ReasonExpired, "Sandbox desired lifetime expired")
	})
	return ctrl.Result{}, err
}

func (r *SandboxReconciler) reconcileReset(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	if sandbox.Status.Placement.FastletName != "" {
		done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
		}
		cleared, err := orchestrator.ClearAssignment(ctx, sandbox, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		sandbox = cleared
	}
	if err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		if status.Runtime.Generation < apiv1alpha2.InitialInstanceGeneration {
			status.Runtime.Generation = apiv1alpha2.InitialInstanceGeneration
		}
		// Reset starts a fresh instance: the previous pause lineage and its
		// checkpoint are dropped.
		status.Runtime.Checkpoint = nil
		status.Runtime.PauseAttempt = 0
		status.Runtime.AcceptedResetRevision = sandbox.Spec.ResetRevision.DeepCopy()
		setControllerStates(status, apiv1alpha2.RuntimePending, apiv1alpha2.DataPlanePending, "Sandbox reset is pending")
		setSandboxReadyCondition(status, sandbox.Generation, "ResetRequested", "Sandbox reset is pending")
		setSandboxSuspendedCondition(status, sandbox.Generation, false, "ResetRequested", "Sandbox reset is pending")
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SandboxReconciler) ensureRuntimeDeleted(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (bool, error) {
	if sandbox.Status.Placement.FastletName == "" {
		return true, nil
	}
	gone, inspectErr := orchestrator.RuntimeGone(ctx, sandbox)
	if gone {
		return true, nil
	}
	if inspectErr != nil {
		if errors.Is(inspectErr, orchestration.ErrAssignedFastletUnavailable) {
			lost, podErr := r.assignedPodLost(ctx, sandbox)
			if podErr != nil {
				return false, podErr
			}
			if lost {
				return true, nil
			}
			return false, inspectErr
		}
		if orchestration.IsNotFound(inspectErr) {
			// Pod-bound model: once the Fastlet Pod identity is gone, all of its
			// Sandbox runtimes are considered gone and cannot be taken over.
			return true, nil
		}
		return false, inspectErr
	}
	if err := orchestrator.DeleteRuntime(ctx, sandbox); err != nil && !orchestration.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

func resetPending(sandbox *apiv1alpha2.Sandbox) bool {
	if sandbox.Spec.ResetRevision == nil {
		return false
	}
	return sandbox.Status.Runtime.AcceptedResetRevision == nil || sandbox.Spec.ResetRevision.After(sandbox.Status.Runtime.AcceptedResetRevision.Time)
}

// expirationPending treats a reset revision newer than the absolute expiry as
// an explicit recovery override. A later ExpireTime still takes effect because
// it is newer than the accepted reset revision.
func expirationPending(sandbox *apiv1alpha2.Sandbox, now time.Time) bool {
	if sandbox == nil || sandbox.Spec.ExpireTime == nil || now.Before(sandbox.Spec.ExpireTime.Time) {
		return false
	}
	acceptedReset := sandbox.Status.Runtime.AcceptedResetRevision
	return acceptedReset == nil || !acceptedReset.After(sandbox.Spec.ExpireTime.Time)
}

func explicitReschedule(err error) bool {
	var failure *fastletapi.FastletError
	if !errors.As(err, &failure) || fastletapi.CreateDispositionFromError(err) != fastletapi.CreateDispositionRejectedBeforeSideEffects {
		return false
	}
	switch failure.Code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorRuntimeUnavailable, fastletapi.ErrorNetworkUnavailable, fastletapi.ErrorInfraUnavailable:
		return true
	default:
		return false
	}
}

func (r *SandboxReconciler) patchStatus(ctx context.Context, sandbox *apiv1alpha2.Sandbox, mutate func(*apiv1alpha2.SandboxStatus)) error {
	key := client.ObjectKeyFromObject(sandbox)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current apiv1alpha2.Sandbox
		if err := r.Get(ctx, key, &current); err != nil {
			return err
		}
		before := current.DeepCopy().Status
		mutate(&current.Status)
		current.Status.ObservedGeneration = current.Generation
		for index := range current.Status.Conditions {
			if current.Status.Conditions[index].Type == orchestration.ConditionReady {
				current.Status.Conditions[index].ObservedGeneration = current.Generation
			}
		}
		if reflect.DeepEqual(before, current.Status) {
			return nil
		}
		return r.Status().Update(ctx, &current)
	})
}

func setSandboxReadyCondition(status *apiv1alpha2.SandboxStatus, generation int64, reason, message string) {
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: orchestration.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.Now(),
	})
}

func setControllerStates(status *apiv1alpha2.SandboxStatus, runtimeState apiv1alpha2.RuntimeState, dataPlaneState apiv1alpha2.DataPlaneState, message string) {
	now := metav1.Now()
	if status.Runtime.State != runtimeState || status.Runtime.Message != message || status.Runtime.LastTransitionTime == nil {
		status.Runtime.LastTransitionTime = &now
	}
	status.Runtime.State, status.Runtime.Message = runtimeState, message
	if status.DataPlane.State != dataPlaneState || status.DataPlane.Message != message || status.DataPlane.LastTransitionTime == nil {
		status.DataPlane.LastTransitionTime = &now
	}
	status.DataPlane.State, status.DataPlane.Message = dataPlaneState, message
}

func (r *SandboxReconciler) SetupWithManager(manager ctrl.Manager) error {
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &apiv1alpha2.Sandbox{}, "status.placement.fastletName", func(object client.Object) []string {
		sandbox := object.(*apiv1alpha2.Sandbox)
		if sandbox.Status.Placement.FastletName == "" {
			return nil
		}
		return []string{sandbox.Status.Placement.FastletName}
	}); err != nil {
		return fmt.Errorf("index Sandbox assignment: %w", err)
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&apiv1alpha2.Sandbox{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPodToSandboxes)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *SandboxReconciler) mapPodToSandboxes(ctx context.Context, object client.Object) []ctrl.Request {
	if object.GetLabels()["app"] != "sandbox-fastlet" {
		return nil
	}
	var list apiv1alpha2.SandboxList
	if err := r.List(ctx, &list, client.InNamespace(object.GetNamespace()), client.MatchingFields{"status.placement.fastletName": object.GetName()}); err != nil {
		return nil
	}
	result := make([]ctrl.Request, 0, len(list.Items))
	for index := range list.Items {
		result = append(result, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[index].Namespace, Name: list.Items[index].Name}})
	}
	return result
}
