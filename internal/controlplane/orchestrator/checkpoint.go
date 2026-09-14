package orchestrator

// checkpoint.go wires the pause checkpoint task (SnapshotKindCheckpoint) of a
// Sandbox into the Fastlet snapshot protocol and reuses the snapshot
// transport end to end: trigger is a checkpoint-kind CreateSnapshot on the
// assigned Fastlet, observation is InspectSnapshot, cleanup is DeleteSnapshot.
// Unlike a SandboxSnapshot there is no owning CR: the checkpoint id is
// derived by the Controller from the Sandbox identity and pause attempt, and
// the published artifact address is persisted in the Sandbox status.

import (
	"context"
	"errors"
	"fmt"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// CreateCheckpoint registers (or replays) the checkpoint task of a pause on
// the Sandbox's assigned Fastlet. The Sandbox runtime must be running there;
// the task returns as soon as it is admitted and is observed via
// ObserveCheckpoint.
func (o *Orchestrator) CreateCheckpoint(ctx context.Context, sandbox *apiv1alpha2.Sandbox, checkpointID string) (*fastletapi.SnapshotStatus, error) {
	if checkpointID == "" {
		return nil, errors.New("checkpoint id is required")
	}
	identity, fastlet, err := o.snapshotIdentityOnAssigned(sandbox, checkpointID, sandbox.Namespace, sandbox.Name)
	if err != nil {
		return nil, err
	}
	request := &fastletapi.CreateSnapshotRequest{
		RequestID: checkpointID, Identity: identity,
		Snapshot: fastletapi.SnapshotSpec{Kind: fastletapi.SnapshotKindCheckpoint},
	}
	// The source Sandbox's bindings ride along into the checkpoint manifest
	// exactly as for a template snapshot: the manifest stays self-contained
	// and the resume path needs no side records.
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

// ObserveCheckpoint reports the checkpoint task state on the assigned
// Fastlet. The pause FSM resolves the assignment live: the task exists only
// while the runtime (and therefore the durable assignment) is still live.
func (o *Orchestrator) ObserveCheckpoint(ctx context.Context, sandbox *apiv1alpha2.Sandbox, checkpointID string) (*fastletapi.SnapshotStatus, error) {
	identity, fastlet, err := o.snapshotIdentityOnAssigned(sandbox, checkpointID, sandbox.Namespace, sandbox.Name)
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

// DeleteCheckpoint discards the node-local checkpoint task record. Published
// store objects are never deleted by it. Callers invoke it once the
// checkpoint address is persisted in the Sandbox status (or the intent is
// abandoned) and treat the outcome as best-effort.
func (o *Orchestrator) DeleteCheckpoint(ctx context.Context, sandbox *apiv1alpha2.Sandbox, checkpointID string) error {
	identity, fastlet, err := o.snapshotIdentityOnAssigned(sandbox, checkpointID, sandbox.Namespace, sandbox.Name)
	if err != nil {
		return err
	}
	_, err = o.FastletClient.DeleteSnapshot(ctx, fastlet.PodIP, &fastletapi.DeleteSnapshotRequest{Identity: identity})
	return err
}

// ResumeCheckpoint returns the checkpoint a Sandbox resuming from pause must
// restore from. While the desired state is Running, any recorded checkpoint
// keeps the restore lineage — including transient projections (Pending,
// Unavailable, Failed) and the window between a completed restore and its
// status projection, so a resume can never silently degrade into a fresh
// boot. The pause FSM (desired state Paused) owns the checkpoint instead.
func ResumeCheckpoint(sandbox *apiv1alpha2.Sandbox) *apiv1alpha2.CheckpointStatus {
	if sandbox == nil || sandbox.Status.Runtime.Checkpoint == nil {
		return nil
	}
	if sandbox.Spec.State == apiv1alpha2.SandboxStatePaused {
		return nil
	}
	return sandbox.Status.Runtime.Checkpoint
}
