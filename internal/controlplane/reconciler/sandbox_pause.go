package reconciler

// sandbox_pause.go drives the pause FSM of a Sandbox whose spec.state is
// Paused: trigger a checkpoint-kind snapshot task on the assigned Fastlet,
// persist the durable artifact address in status.runtime.checkpoint, then
// release the runtime and the assignment. The release is resumable from CRD
// state alone: the checkpoint facts are persisted before the runtime is
// touched, so a Controller crash mid-release re-enters the same step instead
// of losing the address. Resume is the ordinary ensure path carrying
// spec.restore (see Orchestrator.createRuntimeOnTarget).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// PauseRetryInterval back off a pause attempt that failed transiently
// (Fastlet unavailable or draining, dump interrupted): the runtime keeps
// running and the Controller retries with a new checkpoint attempt epoch.
const PauseRetryInterval = 5 * time.Second

// pauseCheckpointID derives the deterministic Fastlet task id of one pause
// attempt: (Sandbox UID, spec generation, pause attempt epoch). The epoch is
// persisted in status.runtime.pauseAttempt, so an unknown-outcome retry
// replays the same task while a new attempt after a terminal failure never
// reuses a retired id.
func pauseCheckpointID(sandbox *apiv1alpha2.Sandbox) string {
	attempt := sandbox.Status.Runtime.PauseAttempt
	if attempt < 1 {
		attempt = 1
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", sandbox.UID, sandbox.Generation, attempt)))
	return hex.EncodeToString(digest[:])
}

// bumpPauseAttempt advances the durable retry epoch; the next checkpoint id
// is derived from it.
func bumpPauseAttempt(status *apiv1alpha2.RuntimeStatus) {
	if status.PauseAttempt < 1 {
		status.PauseAttempt = 1
	}
	status.PauseAttempt++
}

// reconcilePause converges a Sandbox whose desired state is Paused.
func (r *SandboxReconciler) reconcilePause(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	switch sandbox.Status.Runtime.State {
	case apiv1alpha2.RuntimePaused:
		// Converged. Repair the release projection if a crash landed between
		// the status write and the annotation removal.
		if sandbox.Status.Placement.FastletName != "" {
			if _, err := orchestrator.ClearAssignment(ctx, sandbox, false); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: ReadyRequeueInterval}, nil
	case apiv1alpha2.RuntimePausing:
		if sandbox.Status.Runtime.Checkpoint != nil {
			// The checkpoint is durable and persisted; only the runtime
			// release remains (resumable from CRD state alone).
			return r.releasePausedRuntime(ctx, orchestrator, sandbox, sandbox.Status.Runtime.Checkpoint.CheckpointID)
		}
		return r.observePause(ctx, orchestrator, sandbox)
	case apiv1alpha2.RuntimeReady:
		return r.triggerPause(ctx, orchestrator, sandbox)
	case apiv1alpha2.RuntimePending, apiv1alpha2.RuntimeCreating, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.RuntimeResuming:
		return ctrl.Result{RequeueAfter: ObservationPollInterval},
			r.markPauseWaiting(ctx, sandbox, "PauseWaitingRuntime", "Sandbox runtime is "+string(sandbox.Status.Runtime.State)+"; pause starts once it is Ready")
	default:
		// Failed/Stopped/Stopping: there is no live runtime to checkpoint.
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval},
			r.markPauseRejected(ctx, sandbox, "PauseUnsupported", "Sandbox runtime is "+string(sandbox.Status.Runtime.State)+"; only a Ready runtime can be paused")
	}
}

// triggerPause registers the checkpoint task. Admission is idempotent per
// derived checkpoint id, so an unknown outcome replays the same task.
func (r *SandboxReconciler) triggerPause(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	checkpointID := pauseCheckpointID(sandbox)
	observed, err := orchestrator.CreateCheckpoint(ctx, sandbox, checkpointID)
	if err != nil {
		return r.handlePauseCallError(ctx, sandbox, checkpointID, err)
	}
	if observed == nil {
		return ctrl.Result{RequeueAfter: ObservationPollInterval}, orchestration.ErrUnknownFastletOutcome
	}
	return r.projectPauseObservation(ctx, orchestrator, sandbox, checkpointID, observed)
}

// observePause polls the admitted checkpoint task.
func (r *SandboxReconciler) observePause(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox) (ctrl.Result, error) {
	checkpointID := pauseCheckpointID(sandbox)
	observed, err := orchestrator.ObserveCheckpoint(ctx, sandbox, checkpointID)
	if err != nil {
		var failure *fastletapi.FastletError
		if errors.As(err, &failure) && failure.Code == fastletapi.ErrorNotFound {
			// The Fastlet restarted and lost its in-memory task; the dump
			// resumes the VM on every failure path, so the Sandbox is still
			// running and the pause is retried as a new attempt.
			return r.retryPause(ctx, orchestrator, sandbox, checkpointID, "PauseLost",
				"Fastlet no longer reports the pause task; retrying with a new attempt")
		}
		return r.handlePauseCallError(ctx, sandbox, checkpointID, err)
	}
	if observed == nil {
		return ctrl.Result{RequeueAfter: ObservationPollInterval}, orchestration.ErrUnknownFastletOutcome
	}
	return r.projectPauseObservation(ctx, orchestrator, sandbox, checkpointID, observed)
}

// projectPauseObservation maps the checkpoint task phase onto the pause FSM.
func (r *SandboxReconciler) projectPauseObservation(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox, checkpointID string, observed *fastletapi.SnapshotStatus) (ctrl.Result, error) {
	switch observed.Phase {
	case fastletapi.SnapshotPhaseSucceeded:
		return r.persistPauseCheckpoint(ctx, orchestrator, sandbox, checkpointID, observed)
	case fastletapi.SnapshotPhaseFailed:
		reason := observed.Reason
		if reason == "" {
			reason = "PauseFailed"
		}
		return r.retryPause(ctx, orchestrator, sandbox, checkpointID, reason, observed.Message)
	default:
		message := observed.Message
		if message == "" {
			message = "Sandbox checkpoint is " + string(observed.Phase)
		}
		if err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
			setControllerStates(status, apiv1alpha2.RuntimePausing, apiv1alpha2.DataPlaneUnavailable, message)
			setSandboxReadyCondition(status, sandbox.Generation, "Pausing", message)
			setSandboxSuspendedCondition(status, sandbox.Generation, false, string(observed.Phase), message)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: ObservationPollInterval}, nil
	}
}

// persistPauseCheckpoint records the durable artifact address BEFORE the
// runtime is touched. The state stays Pausing (the Sandbox is not resumable
// until the runtime is released), but the address is already safe.
func (r *SandboxReconciler) persistPauseCheckpoint(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox, checkpointID string, observed *fastletapi.SnapshotStatus) (ctrl.Result, error) {
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	if envelope == nil {
		return ctrl.Result{RequeueAfter: PauseRetryInterval}, orchestration.ErrAssignedFastletUnavailable
	}
	now := metav1.Now()
	if err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		status.Runtime.Checkpoint = &apiv1alpha2.CheckpointStatus{
			CheckpointID: checkpointID,
			ManifestRef:  observed.ManifestRef, ArtifactDigest: observed.ArtifactDigest, SizeBytes: observed.SizeBytes,
			PausedAt:    &now,
			FastletName: envelope.FastletName, FastletPodUID: types.UID(envelope.FastletPodUID),
		}
		message := "Checkpoint is durable in the artifact store; releasing the runtime"
		setControllerStates(status, apiv1alpha2.RuntimePausing, apiv1alpha2.DataPlaneUnavailable, message)
		setSandboxReadyCondition(status, sandbox.Generation, "Pausing", message)
		setSandboxSuspendedCondition(status, sandbox.Generation, false, "CheckpointPublished", message)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return r.releasePausedRuntime(ctx, orchestrator, sandbox, checkpointID)
}

// releasePausedRuntime deletes the runtime and releases the durable
// assignment, then marks the Sandbox Paused.
func (r *SandboxReconciler) releasePausedRuntime(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox, checkpointID string) (ctrl.Result, error) {
	if sandbox.Status.Placement.FastletName != "" {
		done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
		}
	}
	if err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		message := "Sandbox is paused; set spec.state=Running to resume from the checkpoint"
		setControllerStates(status, apiv1alpha2.RuntimePaused, apiv1alpha2.DataPlaneUnavailable, message)
		setSandboxReadyCondition(status, sandbox.Generation, "Suspended", message)
		setSandboxSuspendedCondition(status, sandbox.Generation, true, "CheckpointPublished", message)
	}); err != nil {
		return ctrl.Result{}, err
	}
	// The checkpoint address is persisted: node-local task records are
	// best-effort cleanup while the assignment still resolves the Fastlet.
	if err := orchestrator.DeleteCheckpoint(ctx, sandbox, checkpointID); err != nil {
		var failure *fastletapi.FastletError
		if !errors.As(err, &failure) || failure.Code != fastletapi.ErrorNotFound {
			klog.FromContext(ctx).Info("Checkpoint task cleanup deferred", "sandbox", sandbox.Name, "err", err.Error())
		}
	}
	if _, err := orchestrator.ClearAssignment(ctx, sandbox, false); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: ReadyRequeueInterval}, nil
}

// retryPause abandons a transiently failed attempt and schedules a new one:
// the epoch advances, the old task record is dropped best-effort, and the
// runtime stays Ready.
func (r *SandboxReconciler) retryPause(ctx context.Context, orchestrator *orchestration.Orchestrator, sandbox *apiv1alpha2.Sandbox, checkpointID, reason, message string) (ctrl.Result, error) {
	if err := orchestrator.DeleteCheckpoint(ctx, sandbox, checkpointID); err != nil {
		var failure *fastletapi.FastletError
		if !errors.As(err, &failure) || failure.Code != fastletapi.ErrorNotFound {
			klog.FromContext(ctx).Info("Failed checkpoint task cleanup deferred", "sandbox", sandbox.Name, "err", err.Error())
		}
	}
	if message == "" {
		message = "Sandbox pause attempt failed; retrying"
	}
	if err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		bumpPauseAttempt(&status.Runtime)
		setControllerStates(status, apiv1alpha2.RuntimeReady, apiv1alpha2.DataPlaneReady, message)
		setSandboxReadyCondition(status, sandbox.Generation, "PauseFailed", message)
		setSandboxSuspendedCondition(status, sandbox.Generation, false, reason, message)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: PauseRetryInterval}, nil
}

// handlePauseCallError classifies trigger/observe transport and Fastlet
// errors: deterministic rejections surface a condition and back off, ambient
// conditions retry on the short interval.
func (r *SandboxReconciler) handlePauseCallError(ctx context.Context, sandbox *apiv1alpha2.Sandbox, checkpointID string, err error) (ctrl.Result, error) {
	if errors.Is(err, orchestration.ErrAssignedFastletUnavailable) {
		return ctrl.Result{RequeueAfter: PauseRetryInterval},
			r.markPauseWaiting(ctx, sandbox, "FastletUnavailable", "assigned Fastlet is unavailable; pause is retrying")
	}
	var failure *fastletapi.FastletError
	if errors.As(err, &failure) {
		switch failure.Code {
		case fastletapi.ErrorSnapshotUnsupported, fastletapi.ErrorNotFound, fastletapi.ErrorConflict,
			fastletapi.ErrorStaleAssignment, fastletapi.ErrorStaleGeneration, fastletapi.ErrorGenerationFenced:
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval},
				r.markPauseRejected(ctx, sandbox, string(failure.Code), failure.Message)
		case fastletapi.ErrorDraining, fastletapi.ErrorRuntimeUnavailable, fastletapi.ErrorInProgress, fastletapi.ErrorSnapshotInProgress:
			return ctrl.Result{RequeueAfter: PauseRetryInterval},
				r.markPauseWaiting(ctx, sandbox, string(failure.Code), failure.Message)
		}
	}
	return ctrl.Result{RequeueAfter: ObservationPollInterval}, err
}

// markPauseWaiting records a retryable pause obstruction without touching the
// runtime state (the Sandbox keeps running).
func (r *SandboxReconciler) markPauseWaiting(ctx context.Context, sandbox *apiv1alpha2.Sandbox, reason, message string) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		setSandboxReadyCondition(status, sandbox.Generation, reason, message)
		setSandboxSuspendedCondition(status, sandbox.Generation, false, reason, message)
	})
}

// markPauseRejected records a deterministic pause rejection. The runtime
// state is left untouched: a Ready Sandbox stays Ready and keeps serving.
func (r *SandboxReconciler) markPauseRejected(ctx context.Context, sandbox *apiv1alpha2.Sandbox, reason, message string) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha2.SandboxStatus) {
		setSandboxReadyCondition(status, sandbox.Generation, reason, message)
		setSandboxSuspendedCondition(status, sandbox.Generation, false, reason, message)
	})
}

// projectResumeFromCheckpoint projects a resume in flight: while the restore
// ensure has not reached Ready the Sandbox reads Resuming; once it is Ready
// the one-shot checkpoint is cleared (its artifacts become unreferenced store
// objects, managed by store lifecycle).
func projectResumeFromCheckpoint(status *apiv1alpha2.SandboxStatus, sandbox *apiv1alpha2.Sandbox, observed *fastletapi.SandboxStatus) {
	if status == nil || observed == nil || status.Runtime.Checkpoint == nil {
		return
	}
	if observed.Runtime.State == fastletapi.RuntimeStateReady {
		status.Runtime.Checkpoint = nil
		status.Runtime.PauseAttempt = 0
		setSandboxSuspendedCondition(status, sandbox.Generation, false, "Resumed", "Sandbox resumed from its checkpoint")
		return
	}
	setControllerStates(status, apiv1alpha2.RuntimeResuming, apiv1alpha2.DataPlanePending, "Sandbox is resuming from its checkpoint")
	setSandboxSuspendedCondition(status, sandbox.Generation, false, "Resuming", "Sandbox is resuming from its checkpoint")
}

// setSandboxSuspendedCondition maintains the pause lifecycle condition.
func setSandboxSuspendedCondition(status *apiv1alpha2.SandboxStatus, generation int64, suspended bool, reason, message string) {
	conditionStatus := metav1.ConditionFalse
	if suspended {
		conditionStatus = metav1.ConditionTrue
	}
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: apiv1alpha2.SandboxConditionSuspended, Status: conditionStatus, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.Now(),
	})
}
