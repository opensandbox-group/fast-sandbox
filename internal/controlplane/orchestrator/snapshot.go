package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// ProjectSnapshotStatus is the pure projection from a Fastlet snapshot
// observation onto the SandboxSnapshot status. requestedMode is the mode of
// the snapshot intent (spec.mode) the observation is checked against. It is
// used by both FastPath (after the direct trigger) and the Controller
// (observe loop). A terminal phase is final: a stale observation from the
// optimistic-concurrency loser must never roll Succeeded/Failed back to an
// in-flight phase (the Controller already stopped reconciling a terminal
// object, so a rollback would wedge it non-terminal forever).
func ProjectSnapshotStatus(status *apiv1alpha2.SandboxSnapshotStatus, requestedMode apiv1alpha2.SandboxSnapshotMode, observed *fastletapi.SnapshotStatus) {
	if status == nil || observed == nil || status.Phase.Terminal() {
		return
	}
	now := metav1.Now()
	requested := fastletapi.NormalizeSnapshotMode(fastletapi.SnapshotMode(requestedMode))
	produced := fastletapi.NormalizeSnapshotMode(observed.Mode)
	// The observation's artifact facts are copied before any branch: even a
	// rejected observation can carry a published reference (the drift case
	// below, or a driver failure after upload), and dropping it would orphan
	// the remote set — deletion resolves status.manifestRef for cleanup.
	status.SnapshotID = observed.SnapshotID
	status.Mode = apiv1alpha2.SandboxSnapshotMode(produced)
	status.ManifestRef = observed.ManifestRef
	status.ArtifactDigest = observed.ArtifactDigest
	status.SizeBytes = observed.SizeBytes
	if !observed.StartedAt.IsZero() {
		started := metav1.NewTime(observed.StartedAt)
		status.StartedAt = &started
	}
	if !observed.CompletedAt.IsZero() {
		completed := metav1.NewTime(observed.CompletedAt)
		status.CompletedAt = &completed
	} else if fastletapi.SnapshotPhaseTerminal(observed.Phase) && status.CompletedAt == nil {
		status.CompletedAt = &now
	}
	if observed.Phase == fastletapi.SnapshotPhaseSucceeded && produced != requested {
		// Mode drift fence: a success whose produced artifact class does
		// not match the requested mode is never reported as success. This
		// is the projection-side counterpart of the dispatch capability
		// gate: an old Fastlet that ignored the mode field would report a
		// Full artifact (no mode echo) from a RootfsOnly request. The
		// artifact facts copied above stay on the status so the deletion
		// path keeps remote cleanup ownership.
		status.Phase = apiv1alpha2.SandboxSnapshotPhaseFailed
		status.Message = fmt.Sprintf("snapshot produced %s artifacts for a %s request", produced, requested)
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: apiv1alpha2.SandboxSnapshotConditionCompleted, Status: metav1.ConditionFalse,
			Reason: "SnapshotModeDrift", Message: status.Message, LastTransitionTime: now,
		})
		return
	}
	status.Phase = apiv1alpha2.SandboxSnapshotPhase(observed.Phase)
	status.Message = observed.Message
	switch observed.Phase {
	case fastletapi.SnapshotPhaseSucceeded:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: apiv1alpha2.SandboxSnapshotConditionCompleted, Status: metav1.ConditionTrue,
			Reason: "SnapshotSucceeded", Message: "Snapshot artifacts were published under the template name",
			LastTransitionTime: now,
		})
	case fastletapi.SnapshotPhaseFailed:
		reason := "SnapshotFailed"
		if observed.Reason != "" {
			// Stable classification from the fastlet (e.g. InsufficientStorage):
			// clients branch on it to decide between freeing space and
			// re-issuing versus treating the failure as permanent.
			reason = observed.Reason
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: apiv1alpha2.SandboxSnapshotConditionCompleted, Status: metav1.ConditionFalse,
			Reason: reason, Message: observed.Message, LastTransitionTime: now,
		})
	}
}

// SnapshotTarget resolves the durable placement of the snapshot's target
// Sandbox into the single Fastlet that must execute the snapshot. Unlike a
// create there is no Top-K: a snapshot always runs where the Sandbox runs.
// The Sandbox assignment annotation stays authoritative; the returned
// identity carries the full fence validated by Fastlet.
func (o *Orchestrator) SnapshotTarget(snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (fastletapi.SnapshotIdentity, placement.FastletInfo, error) {
	if snapshot == nil || snapshot.UID == "" {
		return fastletapi.SnapshotIdentity{}, placement.FastletInfo{}, errors.New("persisted SandboxSnapshot UID is required")
	}
	return o.snapshotIdentityOnAssigned(sandbox, string(snapshot.UID), snapshot.Namespace, snapshot.Name)
}

// snapshotIdentityOnAssigned builds the full Fastlet-side snapshot identity of
// one task (a SandboxSnapshot or a pause checkpoint) against the target
// Sandbox's live durable assignment. A checkpoint task has no CR: its
// snapshotUid is the derived checkpoint id and the namespace/name fields
// mirror the owning Sandbox.
func (o *Orchestrator) snapshotIdentityOnAssigned(sandbox *apiv1alpha2.Sandbox, snapshotUID, namespace, name string) (fastletapi.SnapshotIdentity, placement.FastletInfo, error) {
	if sandbox == nil || sandbox.UID == "" {
		return fastletapi.SnapshotIdentity{}, placement.FastletInfo{}, errors.New("target Sandbox is required")
	}
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil {
		return fastletapi.SnapshotIdentity{}, placement.FastletInfo{}, err
	}
	if envelope == nil {
		return fastletapi.SnapshotIdentity{}, placement.FastletInfo{}, fmt.Errorf("%w: target Sandbox has no durable assignment", ErrAssignedFastletUnavailable)
	}
	fastlet, ok := o.Registry.GetFastletByID(placement.NewFastletID(sandbox.Namespace, envelope.FastletName))
	if !ok || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" {
		return fastletapi.SnapshotIdentity{}, placement.FastletInfo{}, fmt.Errorf("%w: assigned Fastlet is unavailable", ErrAssignedFastletUnavailable)
	}
	identity := fastletapi.SnapshotIdentity{
		SnapshotUID: snapshotUID, Namespace: namespace, Name: name,
		Sandbox: fastletapi.SandboxIdentity{
			SandboxUID: string(sandbox.UID), Namespace: sandbox.Namespace, Name: sandbox.Name,
			InstanceGeneration: envelope.InstanceGeneration, RuntimeInstanceID: envelope.RuntimeInstanceID,
			AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration, FastletPodUID: envelope.FastletPodUID,
		},
	}
	return identity, fastlet, nil
}

// CreateSnapshot performs exactly one Fastlet call that registers the
// snapshot task. Like CreateRuntime it never writes Kubernetes status.
func (o *Orchestrator) CreateSnapshot(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (*fastletapi.SnapshotStatus, error) {
	identity, fastlet, err := o.SnapshotTarget(snapshot, sandbox)
	if err != nil {
		return nil, err
	}
	mode := fastletapi.SnapshotMode(apiv1alpha2.NormalizeSandboxSnapshotMode(snapshot.Spec.Mode))
	if mode == fastletapi.SnapshotModeRootfsOnly && !fastlet.SupportsCapability(fastletapi.CapabilityRootfsOnlySnapshots) {
		// Version-skew gate: an old Fastlet ignores the unknown mode JSON
		// field and would silently run Full. An observed heartbeat without
		// the capability is a confirmed rejection — ErrorSnapshotUnsupported
		// is deterministic and permanently fails the one-shot object. An
		// UNOBSERVED capability (the Pod informer populated this entry but
		// no heartbeat arrived yet, e.g. after a control-plane restart) is
		// unknown, not absent: refresh once and otherwise keep the intent
		// retryable so a late heartbeat can still satisfy the same
		// request_id.
		if fastlet.LastHeartbeat.IsZero() {
			fastlet = o.refreshFastletObservation(ctx, fastlet)
		}
		if !fastlet.SupportsCapability(fastletapi.CapabilityRootfsOnlySnapshots) {
			if fastlet.LastHeartbeat.IsZero() {
				return nil, fmt.Errorf("%w: rootfs-only snapshot capability of the assigned Fastlet is not observed yet", ErrAssignedFastletUnavailable)
			}
			return nil, &fastletapi.FastletError{
				Code:      fastletapi.ErrorSnapshotUnsupported,
				Message:   "assigned Fastlet does not advertise the rootfs-only snapshot capability",
				Retryable: false,
			}
		}
	}
	request := &fastletapi.CreateSnapshotRequest{
		RequestID: snapshot.Annotations[assignment.AnnotationRequestID], Identity: identity,
		Snapshot: fastletapi.SnapshotSpec{Mode: mode, TemplateName: snapshot.Spec.TemplateName},
	}
	response, callErr := o.FastletClient.CreateSnapshot(ctx, fastlet.PodIP, request)
	if callErr == nil && response != nil &&
		(response.Disposition == fastletapi.CreateDispositionCreated || response.Disposition == fastletapi.CreateDispositionExisting) &&
		response.Snapshot != nil {
		return response.Snapshot, nil
	}
	if callErr == nil {
		callErr = ErrUnknownFastletOutcome
	} else {
		var failure *fastletapi.FastletError
		if !errors.As(callErr, &failure) {
			callErr = fmt.Errorf("%w: %w", ErrUnknownFastletOutcome, callErr)
		}
	}
	return response.Snapshot, callErr
}

// refreshFastletObservation performs one best-effort synchronous heartbeat
// refresh and returns the current registry view of the Fastlet. It never
// fails the caller: a lost or stale refresh keeps the existing observation
// and the periodic heartbeat loop recovers (the same posture as RuntimeGone).
func (o *Orchestrator) refreshFastletObservation(ctx context.Context, fastlet placement.FastletInfo) placement.FastletInfo {
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	heartbeat, err := o.FastletClient.Heartbeat(refreshCtx, fastlet.PodIP, &fastletapi.HeartbeatRequest{
		Cache: fastletapi.CacheCursor{Epoch: fastlet.CacheEpoch, Revision: fastlet.CacheRevision},
	})
	if err == nil {
		err = o.Registry.ApplyHeartbeat(fastlet.ID, fastlet.PodUID, heartbeat, o.now())
	}
	if err != nil && !errors.Is(err, placement.ErrStaleHeartbeat) {
		// A newer periodic heartbeat may have won the race (stale) or the
		// Fastlet is unreachable; either way the caller keeps the current
		// view and decides.
		klog.FromContext(ctx).Error(err, "Refresh Fastlet observation before the snapshot capability gate", "fastlet", fastlet.ID, "podUID", fastlet.PodUID)
	}
	if updated, ok := o.Registry.GetFastletByID(fastlet.ID); ok && updated.PodUID == fastlet.PodUID {
		return updated
	}
	return fastlet
}

// observeTarget resolves the fastlet an observation (or cleanup) must hit:
// the pinned trigger placement when present, else the Sandbox's live
// assignment. Pinning keeps observations on the fastlet that owns the task
// across a mid-flight Sandbox reassignment.
func (o *Orchestrator) observeTarget(snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (fastletapi.SnapshotIdentity, placement.FastletInfo, error) {
	if triggered := snapshot.Status.Triggered; triggered != nil && triggered.FastletName != "" {
		fastlet, ok := o.Registry.GetFastletByID(placement.NewFastletID(snapshot.Namespace, triggered.FastletName))
		if !ok || fastlet.PodUID != triggered.FastletPodUID || fastlet.PodIP == "" {
			return fastletapi.SnapshotIdentity{}, placement.FastletInfo{},
				fmt.Errorf("%w: pinned fastlet %s is unavailable", ErrAssignedFastletUnavailable, triggered.FastletName)
		}
		identity := fastletapi.SnapshotIdentity{
			SnapshotUID: string(snapshot.UID), Namespace: snapshot.Namespace, Name: snapshot.Name,
			Sandbox: fastletapi.SandboxIdentity{
				SandboxUID: string(sandbox.UID), Namespace: sandbox.Namespace, Name: sandbox.Name,
				InstanceGeneration: triggered.InstanceGeneration, RuntimeInstanceID: triggered.RuntimeInstanceID,
				AssignmentAttempt: triggered.AssignmentAttempt, FastletPodUID: triggered.FastletPodUID,
			},
		}
		return identity, fastlet, nil
	}
	return o.SnapshotTarget(snapshot, sandbox)
}

// ObserveSnapshot inspects the snapshot task on the fastlet that owns it
// (the pinned trigger placement when present). A structured status is
// returned whenever the Fastlet knows the task.
func (o *Orchestrator) ObserveSnapshot(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (*fastletapi.SnapshotStatus, error) {
	identity, fastlet, err := o.observeTarget(snapshot, sandbox)
	if err != nil {
		return nil, err
	}
	response, inspectErr := o.FastletClient.InspectSnapshot(ctx, fastlet.PodIP, &fastletapi.InspectSnapshotRequest{Identity: identity})
	if inspectErr != nil {
		return nil, inspectErr
	}
	if response == nil || response.Snapshot == nil {
		return nil, ErrUnknownFastletOutcome
	}
	return response.Snapshot, nil
}

// DeleteSnapshot discards node-local artifacts of a terminal snapshot task,
// resolving the owning fastlet from the pinned trigger placement when
// present. It never unpublishes stored objects; callers treat the outcome as
// best-effort cleanup.
func (o *Orchestrator) DeleteSnapshot(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) error {
	identity, fastlet, err := o.observeTarget(snapshot, sandbox)
	if err != nil {
		return err
	}
	_, err = o.FastletClient.DeleteSnapshot(ctx, fastlet.PodIP, &fastletapi.DeleteSnapshotRequest{Identity: identity})
	return err
}
