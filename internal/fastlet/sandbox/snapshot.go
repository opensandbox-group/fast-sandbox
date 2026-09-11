package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	"fast-sandbox/pkg/util/idgen"

	"k8s.io/klog/v2"
)

// snapshotWorkerTimeout bounds one dump+publish run. The pause window itself
// is short (rootfs reflink + vmstate/memory dump); the bulk is the artifact
// upload, which can legitimately take minutes for multi-GiB memory files.
var snapshotWorkerTimeout = 30 * time.Minute

// snapshotTask is the in-memory state of one snapshot task keyed by the
// SandboxSnapshot UID. Tasks are deliberately ephemeral: a Fastlet restart
// loses them and the Controller terminates the object (SnapshotLost) instead
// of resuming a half-published artifact set (snapshots are non-reentrant).
// Terminal tasks are retained until the object is deleted (the finalizer
// cleanup path removes them): the map is bounded by the number of snapshots
// ever taken toward this Fastlet since start, which is accepted as a small,
// inspectable leak.
type snapshotTask struct {
	identity       fastletapi.SnapshotIdentity
	templateName   string
	actionBindings []runtimecontract.SnapshotActionBinding
	snapshotID     string
	phase          fastletapi.SnapshotPhase
	reason         string
	message        string
	result         *SnapshotResult
	startedAt      time.Time
	completedAt    time.Time
}

func (t *snapshotTask) status() fastletapi.SnapshotStatus {
	status := fastletapi.SnapshotStatus{
		SnapshotID: t.snapshotID, Phase: t.phase, Reason: t.reason, Message: t.message,
		StartedAt: t.startedAt, CompletedAt: t.completedAt,
	}
	if t.result != nil {
		status.ManifestRef = t.result.ManifestRef
		status.ArtifactDigest = t.result.ArtifactDigest
		status.SizeBytes = t.result.SizeBytes
	}
	return status
}

func snapshotFailure(disposition fastletapi.CreateDisposition, failure *fastletapi.FastletError) (*fastletapi.CreateSnapshotResponse, error) {
	return &fastletapi.CreateSnapshotResponse{Disposition: disposition, Error: failure}, failure
}

// CreateSnapshot admits one snapshot task. Admission is synchronous (the
// response returns as soon as the task is registered); the dump and publish
// run in a worker and are observed via InspectSnapshot. Re-delivery of the
// same snapshot UID is deduplicated (EXISTING); a different snapshot for a
// Sandbox with a non-terminal task is rejected (no reentrancy).
func (m *SandboxManager) CreateSnapshot(_ context.Context, req *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
	if failure := m.validateSnapshotRequest(req); failure != nil {
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, failure)
	}
	snapshotter, ok := m.runtime.(RuntimeSnapshotter)
	if !ok {
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletError(fastletapi.ErrorSnapshotUnsupported,
			fmt.Sprintf("runtime %q does not implement sandbox snapshots", m.runtimeName), false))
	}
	sandboxUID := req.Identity.Sandbox.SandboxUID

	m.mu.Lock()
	if !m.runtimeReady || m.recovering {
		m.mu.Unlock()
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletError(fastletapi.ErrorRuntimeUnavailable,
			"Fastlet runtime is recovering; retry later", true))
	}
	if m.draining {
		m.mu.Unlock()
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletError(fastletapi.ErrorDraining,
			"Fastlet is draining", true))
	}
	if existing, found := m.snapshots[req.Identity.SnapshotUID]; found {
		sameClaim := sameSnapshotSandboxClaim(existing.identity.Sandbox, req.Identity.Sandbox)
		status := existing.status()
		m.mu.Unlock()
		if sameClaim {
			return &fastletapi.CreateSnapshotResponse{Disposition: fastletapi.CreateDispositionExisting, Snapshot: &status}, nil
		}
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletError(fastletapi.ErrorConflict,
			"snapshotUid is already bound to a different target Sandbox claim", false))
	}
	metadata := m.sandboxes[sandboxUID]
	if metadata == nil {
		m.mu.Unlock()
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletError(fastletapi.ErrorNotFound,
			"target Sandbox is not managed by this Fastlet", false))
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity.Sandbox); failure != nil {
		m.mu.Unlock()
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, failure)
	}
	if metadata.Phase != "running" {
		m.mu.Unlock()
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletError(fastletapi.ErrorRuntimeUnavailable,
			fmt.Sprintf("target Sandbox runtime is %q, not running", metadata.Phase), true))
	}
	// Sandbox fence — only the pause window (Pending/Creating) is
	// exclusive: a task in Publishing no longer touches the VM, and its
	// upload may overlap the next snapshot's dump.
	for _, task := range m.snapshots {
		if task.identity.Sandbox.SandboxUID != sandboxUID {
			continue
		}
		if holdsPauseWindow(task.phase) {
			failure := fastletError(fastletapi.ErrorSnapshotInProgress,
				"another snapshot is still dumping this Sandbox; retry once it reaches Publishing", false)
			m.mu.Unlock()
			return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, failure)
		}
	}
	// Template-name fence — holds to terminal: concurrent publishers of one
	// index key would race last-writer-wins on the store.
	for _, task := range m.snapshots {
		if !fastletapi.SnapshotPhaseTerminal(task.phase) && task.templateName == req.Snapshot.TemplateName {
			failure := fastletError(fastletapi.ErrorSnapshotInProgress,
				"template name is held by a snapshot that has not terminated; retry after it terminates or use another name", false)
			m.mu.Unlock()
			return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, failure)
		}
	}
	snapshotID, err := idgen.GenerateRequestID()
	if err != nil {
		m.mu.Unlock()
		return snapshotFailure(fastletapi.CreateDispositionRejectedBeforeSideEffects, fastletErrorWithCause(fastletapi.ErrorUnknownOutcome,
			"generate snapshot id: "+err.Error(), true, err))
	}
	bindings := make([]runtimecontract.SnapshotActionBinding, 0, len(req.ActionBindings))
	for _, binding := range req.ActionBindings {
		bindings = append(bindings, runtimecontract.SnapshotActionBinding{Handler: binding.Handler, Input: binding.Input})
	}
	task := &snapshotTask{
		identity: req.Identity, templateName: req.Snapshot.TemplateName, actionBindings: bindings,
		snapshotID: snapshotID, phase: fastletapi.SnapshotPhasePending,
	}
	m.snapshots[req.Identity.SnapshotUID] = task
	// Read the admitted status while still holding the lock: the worker
	// goroutine mutates task fields under m.mu from here on.
	status := task.status()
	m.mu.Unlock()

	m.recordDiagnostic(sandboxUID, "info", "snapshot", "pending", "snapshot task admitted; worker starting")
	go m.runSnapshotWorker(snapshotter, task)
	return &fastletapi.CreateSnapshotResponse{Disposition: fastletapi.CreateDispositionCreated, Snapshot: &status}, nil
}

func (m *SandboxManager) validateSnapshotRequest(req *fastletapi.CreateSnapshotRequest) *fastletapi.FastletError {
	if req == nil || req.Identity.SnapshotUID == "" || req.Identity.Namespace == "" || req.Identity.Name == "" {
		return fastletError(fastletapi.ErrorConflict, "snapshotUid, namespace, and name are required", false)
	}
	if req.Snapshot.TemplateName == "" {
		return fastletError(fastletapi.ErrorConflict, "templateName is required", false)
	}
	if failure := m.validateIdentityTarget(&req.Identity.Sandbox); failure != nil {
		return failure
	}
	return nil
}

// holdsPauseWindow reports whether a phase still occupies the sandbox's
// pause window (the exclusive resource). Publishing only uploads.
func holdsPauseWindow(phase fastletapi.SnapshotPhase) bool {
	return !fastletapi.SnapshotPhaseTerminal(phase) && phase != fastletapi.SnapshotPhasePublishing
}

func sameSnapshotSandboxClaim(existing, requested fastletapi.SandboxIdentity) bool {
	return existing.SandboxUID == requested.SandboxUID && existing.Namespace == requested.Namespace &&
		existing.Name == requested.Name && existing.InstanceGeneration == requested.InstanceGeneration &&
		existing.RuntimeInstanceID == requested.RuntimeInstanceID && existing.AssignmentAttempt == requested.AssignmentAttempt &&
		existing.FastletPodUID == requested.FastletPodUID
}

// runSnapshotWorker executes the one-shot dump+publish. The Sandbox must be
// running when the dump starts; if it vanished (deleted or reassigned) the
// task aborts without publishing.
func (m *SandboxManager) runSnapshotWorker(snapshotter RuntimeSnapshotter, task *snapshotTask) {
	sandboxUID := task.identity.Sandbox.SandboxUID
	ctx, cancel := context.WithTimeout(context.Background(), snapshotWorkerTimeout)
	defer cancel()
	ctx = observability.WithIdentity(ctx, observability.Identity{
		Namespace: task.identity.Sandbox.Namespace, SandboxName: task.identity.Sandbox.Name,
		SandboxUID: sandboxUID, FastletPodUID: task.identity.Sandbox.FastletPodUID,
		InstanceGeneration: task.identity.Sandbox.InstanceGeneration, AssignmentAttempt: task.identity.Sandbox.AssignmentAttempt,
	})

	m.mu.RLock()
	metadata, found := m.sandboxes[sandboxUID]
	runnable := found && metadata.Phase == "running"
	m.mu.RUnlock()
	if !runnable {
		m.finishSnapshotTask(task, fastletapi.SnapshotPhaseFailed, "target Sandbox is no longer running on this Fastlet")
		return
	}

	m.setSnapshotPhase(task, fastletapi.SnapshotPhaseCreating, "")
	result, err := snapshotter.CreateSnapshot(ctx, &RuntimeSnapshotInput{
		SandboxID:      sandboxUID,
		SnapshotID:     task.snapshotID,
		TemplateName:   task.templateName,
		ActionBindings: task.actionBindings,
		// The driver reports Publishing once the pause window closed and the
		// staged set is complete: from that moment the task no longer
		// touches the VM and the sandbox fence releases.
		OnPublishing: func() {
			m.setSnapshotPhase(task, fastletapi.SnapshotPhasePublishing, "")
		},
	})
	if err != nil || result == nil {
		message := "snapshot dump failed"
		reason := ""
		if err != nil {
			message = err.Error()
			if errors.Is(err, ErrInsufficientStorage) {
				// Rejected before the VM was ever paused: zero interruption,
				// nothing staged. Clients free space and re-issue with a new
				// request_id (the object is one-shot).
				reason = "InsufficientStorage"
			}
		}
		m.finishSnapshotTaskWithReason(task, fastletapi.SnapshotPhaseFailed, reason, message)
		return
	}
	// SnapshotPhasePublishing is reserved: the firecracker driver
	// implementation will report the artifact upload as a distinct stage
	// once the runtime-agent publish path lands. This worker keeps the
	// coarse Creating -> Succeeded/Failed projection until then.
	m.mu.Lock()
	task.result = result
	m.mu.Unlock()
	m.finishSnapshotTask(task, fastletapi.SnapshotPhaseSucceeded, "")
	klog.InfoS("Sandbox snapshot completed", "sandboxID", sandboxUID, "snapshotID", task.snapshotID,
		"manifestRef", result.ManifestRef, "sizeBytes", result.SizeBytes)
}

func (m *SandboxManager) setSnapshotPhase(task *snapshotTask, phase fastletapi.SnapshotPhase, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setSnapshotPhaseLocked(task, phase, message)
}

func (m *SandboxManager) setSnapshotPhaseLocked(task *snapshotTask, phase fastletapi.SnapshotPhase, message string) {
	if task.phase == phase && task.message == message {
		return
	}
	task.phase = phase
	if message != "" {
		task.message = message
	}
	switch phase {
	case fastletapi.SnapshotPhaseCreating:
		if task.startedAt.IsZero() {
			task.startedAt = m.clock.Now()
		}
	case fastletapi.SnapshotPhaseSucceeded, fastletapi.SnapshotPhaseFailed:
		if task.completedAt.IsZero() {
			task.completedAt = m.clock.Now()
		}
	}
}

func (m *SandboxManager) finishSnapshotTask(task *snapshotTask, phase fastletapi.SnapshotPhase, message string) {
	m.finishSnapshotTaskWithReason(task, phase, "", message)
}

// finishSnapshotTaskWithReason terminates a task with a stable machine-readable
// classification (clients branch on it; the message is human-oriented).
func (m *SandboxManager) finishSnapshotTaskWithReason(task *snapshotTask, phase fastletapi.SnapshotPhase, reason, message string) {
	m.mu.Lock()
	m.setSnapshotPhaseLocked(task, phase, message)
	if task.reason != reason {
		task.reason = reason
	}
	m.mu.Unlock()
	level := "info"
	if phase == fastletapi.SnapshotPhaseFailed {
		level = "error"
	}
	m.recordDiagnostic(task.identity.Sandbox.SandboxUID, level, "snapshot", string(phase), message)
}

// InspectSnapshot reports the current task state. An unknown SnapshotUID is
// NotFound: tasks are in-memory only, so this is also the restart signal the
// Controller turns into a terminal SnapshotLost.
func (m *SandboxManager) InspectSnapshot(req *fastletapi.InspectSnapshotRequest) (*fastletapi.InspectSnapshotResponse, error) {
	if failure := m.validateSnapshotInspectRequest(req); failure != nil {
		return &fastletapi.InspectSnapshotResponse{Error: failure}, failure
	}
	m.mu.RLock()
	task, found := m.snapshots[req.Identity.SnapshotUID]
	var status fastletapi.SnapshotStatus
	if found {
		if !sameSnapshotSandboxClaim(task.identity.Sandbox, req.Identity.Sandbox) {
			found = false
		} else {
			status = task.status()
		}
	}
	m.mu.RUnlock()
	if !found {
		failure := fastletError(fastletapi.ErrorNotFound, "snapshot task is not known to this Fastlet", false)
		return &fastletapi.InspectSnapshotResponse{Error: failure}, failure
	}
	return &fastletapi.InspectSnapshotResponse{Snapshot: &status}, nil
}

func (m *SandboxManager) validateSnapshotInspectRequest(req *fastletapi.InspectSnapshotRequest) *fastletapi.FastletError {
	if req == nil || req.Identity.SnapshotUID == "" {
		return fastletError(fastletapi.ErrorConflict, "snapshotUid is required", false)
	}
	return m.validateIdentityTarget(&req.Identity.Sandbox)
}

// DeleteSnapshot discards the node-local artifacts of a terminal task. A
// non-terminal task is rejected: a running dump is never aborted. An unknown
// task is a no-op (idempotent cleanup).
func (m *SandboxManager) DeleteSnapshot(ctx context.Context, req *fastletapi.DeleteSnapshotRequest) (*fastletapi.DeleteSnapshotResponse, error) {
	if req == nil || req.Identity.SnapshotUID == "" {
		failure := fastletError(fastletapi.ErrorConflict, "snapshotUid is required", false)
		return &fastletapi.DeleteSnapshotResponse{Error: failure}, failure
	}
	if failure := m.validateIdentityTarget(&req.Identity.Sandbox); failure != nil {
		return &fastletapi.DeleteSnapshotResponse{Error: failure}, failure
	}
	m.mu.Lock()
	task, found := m.snapshots[req.Identity.SnapshotUID]
	if found && !sameSnapshotSandboxClaim(task.identity.Sandbox, req.Identity.Sandbox) {
		m.mu.Unlock()
		failure := fastletError(fastletapi.ErrorConflict, "snapshotUid is bound to a different target Sandbox claim", false)
		return &fastletapi.DeleteSnapshotResponse{Error: failure}, failure
	}
	if found && !fastletapi.SnapshotPhaseTerminal(task.phase) {
		m.mu.Unlock()
		failure := fastletError(fastletapi.ErrorInProgress, "snapshot task is still running; deletion waits for a terminal phase", true)
		return &fastletapi.DeleteSnapshotResponse{Error: failure}, failure
	}
	if found {
		delete(m.snapshots, req.Identity.SnapshotUID)
	}
	m.mu.Unlock()
	if !found {
		return &fastletapi.DeleteSnapshotResponse{}, nil
	}
	if snapshotter, ok := m.runtime.(RuntimeSnapshotter); ok {
		cleanupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := snapshotter.DeleteSnapshot(cleanupCtx, task.snapshotID); err != nil && !errors.Is(err, ErrSandboxNotFound) {
			m.mu.Lock()
			m.snapshots[req.Identity.SnapshotUID] = task
			m.mu.Unlock()
			failure := fastletErrorWithCause(fastletapi.ErrorUnknownOutcome, "snapshot artifact cleanup failed: "+err.Error(), true, err)
			return &fastletapi.DeleteSnapshotResponse{Error: failure}, failure
		}
	}
	return &fastletapi.DeleteSnapshotResponse{}, nil
}
