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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// SandboxSnapshotFinalizerName gates object deletion on best-effort
	// cleanup of node-local snapshot artifacts.
	SandboxSnapshotFinalizerName = "sandbox.fast.io/snapshot-cleanup"
	// SnapshotObservationPollInterval drives the observe loop of a running
	// snapshot task.
	SnapshotObservationPollInterval = time.Second
	// SnapshotRetryInterval backs off placement problems (unavailable
	// Fastlet, not-Ready target Sandbox).
	SnapshotRetryInterval = 5 * time.Second
	// SnapshotPendingDeadline bounds how long a never-admitted intent may
	// hold its reentrancy fence (same Sandbox, same template name) while
	// the target Sandbox or the assigned Fastlet stays unavailable. Past
	// the deadline the object terminates Failed and the fence is released;
	// clients retry with a new request_id.
	SnapshotPendingDeadline = 10 * time.Minute
	// SnapshotActiveDeadline bounds an admitted snapshot the Controller can
	// no longer observe (Fastlet lost with its in-memory task). It safely
	// exceeds the Fastlet worker's own dump/publish timeout, so a live task
	// always terminates itself before the Controller gives up on it.
	SnapshotActiveDeadline = 45 * time.Minute
)

// SandboxSnapshotReconciler converges one SandboxSnapshot: it resolves the
// target Sandbox's durable assignment, triggers the snapshot on the single
// assigned Fastlet, observes the task until a terminal phase, and gates
// deletion on artifact cleanup. Snapshots are one-shot and non-reentrant;
// terminal objects stop reconciling.
type SandboxSnapshotReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Orchestrator *orchestration.Orchestrator
	Now          func() time.Time
	// MaxConcurrentReconciles parallelizes independent snapshots; the
	// workqueue still guarantees a single in-flight reconcile per key.
	MaxConcurrentReconciles int
}

func (r *SandboxSnapshotReconciler) Reconcile(ctx context.Context, request ctrl.Request) (_ ctrl.Result, resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: request.Namespace, SandboxName: request.Name})
	ctx, span := observability.Start(ctx, "controller.reconcile SandboxSnapshot")
	defer func() { observability.End(span, resultErr) }()
	var snapshot apiv1alpha2.SandboxSnapshot
	if err := r.Get(ctx, request.NamespacedName, &snapshot); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Orchestrator == nil {
		return ctrl.Result{}, errors.New("Sandbox orchestrator is not configured")
	}
	if snapshot.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, &snapshot)
	}
	if !controllerutil.ContainsFinalizer(&snapshot, SandboxSnapshotFinalizerName) {
		controllerutil.AddFinalizer(&snapshot, SandboxSnapshotFinalizerName)
		if err := r.Update(ctx, &snapshot); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if snapshot.Status.Phase.Terminal() {
		return ctrl.Result{}, nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if exceeded, elapsed := snapshotDeadlineExceeded(&snapshot, now); exceeded {
		return ctrl.Result{}, r.markSnapshotFailed(ctx, &snapshot, "SnapshotDeadlineExceeded",
			fmt.Sprintf("snapshot did not reach a terminal phase within %s; reentrancy fence released, retry with a new request", elapsed.Round(time.Second)))
	}

	sandbox, result, err := r.resolveTarget(ctx, &snapshot)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result != nil {
		return *result, nil
	}
	if snapshot.Status.SnapshotID == "" {
		return r.reconcileTrigger(ctx, &snapshot, sandbox)
	}
	return r.reconcileObserve(ctx, &snapshot, sandbox)
}

// snapshotDeadlineExceeded reports whether a non-terminal snapshot has held
// its fence past the pending (never admitted) or active (admitted but no
// longer observable) deadline. The remaining time is returned for callers
// that want to schedule a wakeup.
func snapshotDeadlineExceeded(snapshot *apiv1alpha2.SandboxSnapshot, now time.Time) (bool, time.Duration) {
	if snapshot.Status.Phase.Terminal() || snapshot.CreationTimestamp.IsZero() {
		return false, 0
	}
	deadline := SnapshotActiveDeadline
	if snapshot.Status.SnapshotID == "" {
		deadline = SnapshotPendingDeadline
	}
	elapsed := now.Sub(snapshot.CreationTimestamp.Time)
	if elapsed > deadline {
		return true, elapsed
	}
	return false, deadline - elapsed
}

// resolveTarget loads and validates the target Sandbox. A nil result means
// the caller may continue with the returned sandbox.
func (r *SandboxSnapshotReconciler) resolveTarget(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot) (*apiv1alpha2.Sandbox, *ctrl.Result, error) {
	var sandbox apiv1alpha2.Sandbox
	key := types.NamespacedName{Namespace: snapshot.Spec.SandboxRef.Namespace, Name: snapshot.Spec.SandboxRef.Name}
	if err := r.Get(ctx, key, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &ctrl.Result{}, r.markSnapshotFailed(ctx, snapshot, "SandboxNotFound", "target Sandbox does not exist")
		}
		return nil, nil, err
	}
	if snapshot.Spec.SandboxRef.UID != "" && string(sandbox.UID) != string(snapshot.Spec.SandboxRef.UID) {
		return nil, &ctrl.Result{}, r.markSnapshotFailed(ctx, snapshot, "SandboxUIDMismatch", "target Sandbox was recreated under the same name")
	}
	if sandbox.DeletionTimestamp != nil {
		return nil, &ctrl.Result{}, r.markSnapshotFailed(ctx, snapshot, "SandboxDeleting", "target Sandbox is being deleted")
	}
	if sandbox.Status.Runtime.State != apiv1alpha2.RuntimeReady {
		if err := r.markSnapshotPending(ctx, snapshot, "SandboxRuntimeNotReady", "target Sandbox runtime is "+string(sandbox.Status.Runtime.State)); err != nil {
			return nil, nil, err
		}
		return nil, &ctrl.Result{RequeueAfter: SnapshotRetryInterval}, nil
	}
	return &sandbox, nil, nil
}

func (r *SandboxSnapshotReconciler) reconcileTrigger(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	observed, err := r.Orchestrator.CreateSnapshot(ctx, snapshot, sandbox)
	if err != nil {
		return r.handleSnapshotCallError(ctx, snapshot, sandbox, err)
	}
	if observed == nil {
		return ctrl.Result{RequeueAfter: SnapshotObservationPollInterval}, orchestration.ErrUnknownFastletOutcome
	}
	if err := r.projectObservation(ctx, snapshot, sandbox, observed); err != nil {
		return ctrl.Result{}, err
	}
	if fastletapi.SnapshotPhaseTerminal(observed.Phase) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: SnapshotObservationPollInterval}, nil
}

func (r *SandboxSnapshotReconciler) reconcileObserve(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	observed, err := r.Orchestrator.ObserveSnapshot(ctx, snapshot, sandbox)
	if err != nil {
		var failure *fastletapi.FastletError
		if errors.As(err, &failure) && failure.Code == fastletapi.ErrorNotFound {
			// The Fastlet no longer knows the task: it restarted (in-memory
			// tasks are lost) or was replaced. Either way the attempt is
			// gone and must terminate.
			return ctrl.Result{}, r.markSnapshotFailed(ctx, snapshot, "SnapshotLost", "Fastlet no longer reports the snapshot task")
		}
		return r.handleSnapshotCallError(ctx, snapshot, sandbox, err)
	}
	if observed == nil {
		return ctrl.Result{RequeueAfter: SnapshotObservationPollInterval}, orchestration.ErrUnknownFastletOutcome
	}
	if err := r.projectObservation(ctx, snapshot, sandbox, observed); err != nil {
		return ctrl.Result{}, err
	}
	if fastletapi.SnapshotPhaseTerminal(observed.Phase) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: SnapshotObservationPollInterval}, nil
}

// handleSnapshotCallError maps trigger/observe failures onto the one-shot
// snapshot semantics: deterministic rejections terminate the object, ambient
// unavailability keeps it Pending for a later retry.
func (r *SandboxSnapshotReconciler) handleSnapshotCallError(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox, err error) (ctrl.Result, error) {
	if errors.Is(err, orchestration.ErrAssignedFastletUnavailable) {
		if statusErr := r.markSnapshotPending(ctx, snapshot, "FastletUnavailable", "assigned Fastlet is unavailable; retrying"); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: SnapshotRetryInterval}, nil
	}
	var failure *fastletapi.FastletError
	if errors.As(err, &failure) {
		switch failure.Code {
		case fastletapi.ErrorSnapshotInProgress, fastletapi.ErrorSnapshotUnsupported, fastletapi.ErrorNotFound,
			fastletapi.ErrorConflict, fastletapi.ErrorStaleAssignment, fastletapi.ErrorStaleGeneration, fastletapi.ErrorGenerationFenced:
			return ctrl.Result{}, r.markSnapshotFailed(ctx, snapshot, string(failure.Code), failure.Message)
		case fastletapi.ErrorDraining, fastletapi.ErrorInProgress:
			// Draining and in-progress states are transient (rollout,
			// restart): keep the intent Pending instead of terminating a
			// one-shot snapshot over an ambient condition.
			if statusErr := r.markSnapshotPending(ctx, snapshot, string(failure.Code), failure.Message); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: SnapshotRetryInterval}, nil
		}
	}
	return ctrl.Result{RequeueAfter: SnapshotObservationPollInterval}, err
}

func (r *SandboxSnapshotReconciler) projectObservation(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox, observed *fastletapi.SnapshotStatus) error {
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil {
		return err
	}
	return r.patchStatus(ctx, snapshot, func(status *apiv1alpha2.SandboxSnapshotStatus) {
		if envelope != nil {
			status.FastletName = envelope.FastletName
			status.FastletPodUID = types.UID(envelope.FastletPodUID)
			// Pin the trigger placement once: later observations resolve
			// against it even if the Sandbox is reassigned mid-flight.
			if status.Triggered == nil {
				status.Triggered = &apiv1alpha2.SnapshotTrigger{
					FastletName: envelope.FastletName, FastletPodUID: envelope.FastletPodUID,
					RuntimeInstanceID:  envelope.RuntimeInstanceID,
					InstanceGeneration: envelope.InstanceGeneration, AssignmentAttempt: envelope.Attempt,
				}
			}
		}
		status.SandboxUID = sandbox.UID
		orchestration.ProjectSnapshotStatus(status, observed)
	})
}

func (r *SandboxSnapshotReconciler) reconcileDeletion(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizerName) {
		return ctrl.Result{}, nil
	}
	if !snapshot.Status.Phase.Terminal() {
		done, err := r.waitSnapshotTerminal(ctx, snapshot)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: SnapshotObservationPollInterval}, err
		}
	}
	// Best-effort artifact cleanup: stored objects are never unpublished and
	// node-side leftovers are bounded by driver-local garbage collection.
	sandbox, result, resolveErr := r.resolveTarget(ctx, snapshot)
	if resolveErr == nil && result == nil && sandbox != nil {
		if err := r.Orchestrator.DeleteSnapshot(ctx, snapshot, sandbox); err != nil && !orchestration.IsNotFound(err) {
			var failure *fastletapi.FastletError
			if errors.As(err, &failure) && failure.Code == fastletapi.ErrorRuntimeUnavailable {
				// The artifacts may still exist but the runtime is down;
				// proceed so object deletion is not wedged on cleanup.
			} else if !errors.Is(err, orchestration.ErrAssignedFastletUnavailable) {
				return ctrl.Result{RequeueAfter: SnapshotRetryInterval}, err
			}
		}
	}
	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha2.SandboxSnapshot
		if err := r.Get(ctx, client.ObjectKeyFromObject(snapshot), &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		controllerutil.RemoveFinalizer(&current, SandboxSnapshotFinalizerName)
		return r.Update(ctx, &current)
	})
}

// waitSnapshotTerminal reports whether the in-flight task reached a terminal
// phase on the Fastlet. The Fastlet never aborts a running dump, so deletion
// deliberately waits instead of cancelling — but a fence that outlived its
// deadline (Fastlet lost with the task) is treated as done so object deletion
// and the fence itself are never wedged.
func (r *SandboxSnapshotReconciler) waitSnapshotTerminal(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot) (bool, error) {
	if snapshot.Status.Phase.Terminal() {
		return true, nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if exceeded, _ := snapshotDeadlineExceeded(snapshot, now); exceeded {
		return true, nil
	}
	sandbox, result, err := r.resolveTarget(ctx, snapshot)
	if err != nil || result != nil {
		// The target (or its placement) is gone; the node task cannot be
		// observed or cleaned up any further.
		return true, err
	}
	observed, observeErr := r.Orchestrator.ObserveSnapshot(ctx, snapshot, sandbox)
	if observeErr != nil {
		var failure *fastletapi.FastletError
		if errors.As(observeErr, &failure) && failure.Code == fastletapi.ErrorNotFound {
			return true, nil
		}
		if errors.Is(observeErr, orchestration.ErrAssignedFastletUnavailable) {
			return true, nil
		}
		return false, observeErr
	}
	if observed == nil {
		return false, nil
	}
	if err := r.projectObservation(ctx, snapshot, sandbox, observed); err != nil {
		return false, err
	}
	return fastletapi.SnapshotPhaseTerminal(observed.Phase), nil
}

func (r *SandboxSnapshotReconciler) markSnapshotFailed(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, reason, message string) error {
	return r.patchStatus(ctx, snapshot, func(status *apiv1alpha2.SandboxSnapshotStatus) {
		now := metav1.Now()
		status.Phase = apiv1alpha2.SandboxSnapshotPhaseFailed
		status.Message = message
		if status.CompletedAt == nil {
			status.CompletedAt = &now
		}
		apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: apiv1alpha2.SandboxSnapshotConditionCompleted, Status: metav1.ConditionFalse,
			Reason: reason, Message: message, LastTransitionTime: now,
		})
	})
}

func (r *SandboxSnapshotReconciler) markSnapshotPending(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, reason, message string) error {
	return r.patchStatus(ctx, snapshot, func(status *apiv1alpha2.SandboxSnapshotStatus) {
		if status.Phase != apiv1alpha2.SandboxSnapshotPhaseCreating && status.Phase != apiv1alpha2.SandboxSnapshotPhasePublishing {
			status.Phase = apiv1alpha2.SandboxSnapshotPhasePending
			status.Message = message
		}
	})
}

func (r *SandboxSnapshotReconciler) patchStatus(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, mutate func(*apiv1alpha2.SandboxSnapshotStatus)) error {
	key := client.ObjectKeyFromObject(snapshot)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current apiv1alpha2.SandboxSnapshot
		if err := r.Get(ctx, key, &current); err != nil {
			return err
		}
		before := current.DeepCopy().Status
		// Terminal phases are monotonic: once Succeeded/Failed is visible,
		// neither this controller nor a racing FastPath patch may rewrite
		// the object (a deletion-time target recheck or a stale observation
		// would otherwise roll a terminal object backwards).
		if before.Phase.Terminal() {
			return nil
		}
		mutate(&current.Status)
		current.Status.ObservedGeneration = current.Generation
		if reflect.DeepEqual(before, current.Status) {
			return nil
		}
		return r.Status().Update(ctx, &current)
	})
}

func (r *SandboxSnapshotReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&apiv1alpha2.SandboxSnapshot{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}
