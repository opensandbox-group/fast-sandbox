package orchestrator

import (
	"context"
	"errors"
	"fmt"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ProjectSnapshotStatus is the pure projection from a Fastlet snapshot
// observation onto the SandboxSnapshot status. It is used by both FastPath
// (after the direct trigger) and the Controller (observe loop). A terminal
// phase is final: a stale observation from the optimistic-concurrency loser
// must never roll Succeeded/Failed back to an in-flight phase (the
// Controller already stopped reconciling a terminal object, so a rollback
// would wedge it non-terminal forever).
func ProjectSnapshotStatus(status *apiv1alpha2.SandboxSnapshotStatus, observed *fastletapi.SnapshotStatus) {
	if status == nil || observed == nil || status.Phase.Terminal() {
		return
	}
	now := metav1.Now()
	status.Phase = apiv1alpha2.SandboxSnapshotPhase(observed.Phase)
	status.Message = observed.Message
	status.SnapshotID = observed.SnapshotID
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
	fastlet, ok := o.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !ok || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" {
		return fastletapi.SnapshotIdentity{}, placement.FastletInfo{}, fmt.Errorf("%w: assigned Fastlet is unavailable", ErrAssignedFastletUnavailable)
	}
	identity := fastletapi.SnapshotIdentity{
		SnapshotUID: string(snapshot.UID), Namespace: snapshot.Namespace, Name: snapshot.Name,
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
	request := &fastletapi.CreateSnapshotRequest{
		RequestID: snapshot.Annotations[assignment.AnnotationRequestID], Identity: identity,
		Snapshot: fastletapi.SnapshotSpec{TemplateName: snapshot.Spec.TemplateName},
	}
	// The source Sandbox's bindings ride along: the driver records them in
	// the published manifest, making the artifact set self-contained (the
	// CR annotation is only the in-cluster auto-apply path and may be gone
	// by restore time).
	if bindings, bindErr := compileActionBindings(sandbox.Spec.ActionBindings); bindErr == nil {
		request.ActionBindings = bindings
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
			callErr = fmt.Errorf("%w: %v", ErrUnknownFastletOutcome, callErr)
		}
	}
	return response.Snapshot, callErr
}

// observeTarget resolves the fastlet an observation (or cleanup) must hit:
// the pinned trigger placement when present, else the Sandbox's live
// assignment. Pinning keeps observations on the fastlet that owns the task
// across a mid-flight Sandbox reassignment.
func (o *Orchestrator) observeTarget(snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (fastletapi.SnapshotIdentity, placement.FastletInfo, error) {
	if triggered := snapshot.Status.Triggered; triggered != nil && triggered.FastletName != "" {
		fastlet, ok := o.Registry.GetFastletByID(placement.FastletID(triggered.FastletName))
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

// SnapshotSandboxKey returns the namespace/name key of a SandboxSnapshot's
// target.
func SnapshotSandboxKey(snapshot *apiv1alpha2.SandboxSnapshot) types.NamespacedName {
	return types.NamespacedName{Namespace: snapshot.Spec.SandboxRef.Namespace, Name: snapshot.Spec.SandboxRef.Name}
}
