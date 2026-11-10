package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type snapshotRuntime struct {
	*admissionRuntime
	mu                    sync.Mutex
	createCalls           int
	lastInput             *RuntimeSnapshotInput
	createErr             error
	block                 chan struct{}
	blockBeforePublishing bool
	deleted               []string
}

func newSnapshotRuntime() *snapshotRuntime {
	return &snapshotRuntime{admissionRuntime: newAdmissionRuntime()}
}

func (r *snapshotRuntime) CreateSnapshot(_ context.Context, input *RuntimeSnapshotInput) (*SnapshotResult, error) {
	r.mu.Lock()
	r.createCalls++
	r.lastInput = input
	err := r.createErr
	block := r.block
	blockBeforePublishing := r.blockBeforePublishing
	r.mu.Unlock()
	if blockBeforePublishing && input.OnPublishing != nil {
		// Surface Publishing, then stall inside the (excluded) upload.
		input.OnPublishing()
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	return &SnapshotResult{
		SnapshotID: input.SnapshotID, ManifestRef: "s3://bucket/publish/" + input.SnapshotID[:8] + "/manifest.json",
		ArtifactDigest: "digest-" + input.SnapshotID, SizeBytes: 1234,
		// A mode-aware runtime reports the artifact class it produced.
		Mode: fastletapi.NormalizeSnapshotMode(input.Mode),
	}, nil
}

func (r *snapshotRuntime) DeleteSnapshot(_ context.Context, snapshotID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, snapshotID)
	return nil
}

func newSnapshotManager(t *testing.T, runtime RuntimeDriver) *SandboxManager {
	t.Helper()
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 2, FastletPodUID: "pod-a"})
	require.NoError(t, err)
	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "uid-a", Namespace: "default", Name: "sandbox-a", FastletPodUID: "pod-a",
		RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1,
	}}, Phase: "running", AppliedGeneration: 1}
	return manager
}

func snapshotRequest(snapshotUID string) *fastletapi.CreateSnapshotRequest {
	return &fastletapi.CreateSnapshotRequest{
		RequestID: "request-" + snapshotUID,
		Identity: fastletapi.SnapshotIdentity{
			SnapshotUID: snapshotUID, Namespace: "default", Name: "snap-" + snapshotUID,
			Sandbox: fastletapi.SandboxIdentity{
				SandboxUID: "uid-a", Namespace: "default", Name: "sandbox-a", FastletPodUID: "pod-a",
				RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1,
			},
		},
		Snapshot: fastletapi.SnapshotSpec{TemplateName: "app-v2"},
	}
}

func TestCreateSnapshotRejectsRuntimeWithoutSnapshotter(t *testing.T) {
	manager := newSnapshotManager(t, newAdmissionRuntime())
	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.Error(t, err)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorSnapshotUnsupported, failure.Code)
}

func TestCreateSnapshotRejectsActionsV2BeforeSnapshotter(t *testing.T) {
	runtime := newSnapshotRuntime()
	manager := newSnapshotManager(t, runtime)
	manager.sandboxes["uid-a"].Config.ActionsAPIVersion = actionapi.APIVersionV2

	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-v2"))
	require.Error(t, err)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorSnapshotUnsupported, failure.Code)
	require.Zero(t, runtime.createCalls, "strict snapshot admission must fail before runtime snapshot effects")
	require.Empty(t, manager.snapshots)
}

func TestCreateSnapshotAdmitsRunsAndObserves(t *testing.T) {
	runtime := newSnapshotRuntime()
	manager := newSnapshotManager(t, runtime)
	response, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)
	require.NotEmpty(t, response.Snapshot.SnapshotID)

	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseSucceeded
	}, 2*time.Second, 10*time.Millisecond)
	inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
	require.NoError(t, err)
	require.Equal(t, fastletapi.SnapshotPhaseSucceeded, inspected.Snapshot.Phase)
	require.Equal(t, "digest-"+response.Snapshot.SnapshotID, inspected.Snapshot.ArtifactDigest)
	require.NotEmpty(t, inspected.Snapshot.ManifestRef)
	require.False(t, inspected.Snapshot.StartedAt.IsZero())
	require.False(t, inspected.Snapshot.CompletedAt.IsZero())
}

func TestCreateSnapshotAllowsNextSandboxSnapshotWhilePublishing(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.block = make(chan struct{})
	runtime.blockBeforePublishing = true
	manager := newSnapshotManager(t, runtime)
	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)
	// The task reached Publishing and is stalled in its upload.
	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhasePublishing
	}, 2*time.Second, 10*time.Millisecond)

	// Same Sandbox, DIFFERENT template name: admitted while the upload runs.
	second := snapshotRequest("snap-b")
	second.Snapshot.TemplateName = "app-v3"
	response, err := manager.CreateSnapshot(context.Background(), second)
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)

	close(runtime.block)
	for _, id := range []string{"snap-a", "snap-b"} {
		require.Eventually(t, func() bool {
			inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor(id)})
			return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseSucceeded
		}, 2*time.Second, 10*time.Millisecond)
	}
}

func TestCreateSnapshotRejectsSameTemplateNameWhilePublishing(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.block = make(chan struct{})
	runtime.blockBeforePublishing = true
	defer close(runtime.block)
	manager := newSnapshotManager(t, runtime)
	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhasePublishing
	}, 2*time.Second, 10*time.Millisecond)

	// Same template name on the SAME Sandbox (whose sandbox fence already
	// released at Publishing): the index key stays exclusive until the
	// holder terminates.
	other := snapshotRequest("snap-b")
	other.Snapshot.TemplateName = "app-v2"
	_, err = manager.CreateSnapshot(context.Background(), other)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorSnapshotInProgress, failure.Code)
}

func TestSnapshotInsufficientStorageFailsWithDistinctReason(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.createErr = ErrInsufficientStorage
	manager := newSnapshotManager(t, runtime)
	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseFailed
	}, 2*time.Second, 10*time.Millisecond)
	inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
	require.NoError(t, err)
	require.Equal(t, "InsufficientStorage", inspected.Snapshot.Reason)
	require.Contains(t, inspected.Snapshot.Message, "insufficient local storage")
}

func TestCreateSnapshotRedeliveryIsDeduplicated(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.block = make(chan struct{})
	manager := newSnapshotManager(t, runtime)
	first, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)

	second, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionExisting, second.Disposition)
	require.Equal(t, first.Snapshot.SnapshotID, second.Snapshot.SnapshotID)

	close(runtime.block)
	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseSucceeded
	}, 2*time.Second, 10*time.Millisecond)
	runtime.mu.Lock()
	require.Equal(t, 1, runtime.createCalls)
	runtime.mu.Unlock()
}

func TestCreateSnapshotRejectsConcurrentSnapshotForSameSandbox(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.block = make(chan struct{})
	defer close(runtime.block)
	manager := newSnapshotManager(t, runtime)
	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)

	_, err = manager.CreateSnapshot(context.Background(), snapshotRequest("snap-b"))
	require.Error(t, err)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorSnapshotInProgress, failure.Code)
}

func TestCreateSnapshotRejectsMissingOrNotRunningSandbox(t *testing.T) {
	runtime := newSnapshotRuntime()
	manager := newSnapshotManager(t, runtime)

	missing := snapshotRequest("snap-a")
	missing.Identity.Sandbox.SandboxUID = "uid-missing"
	_, err := manager.CreateSnapshot(context.Background(), missing)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorNotFound, failure.Code)

	manager.mu.Lock()
	manager.sandboxes["uid-a"].Phase = "creating"
	manager.mu.Unlock()
	_, err = manager.CreateSnapshot(context.Background(), snapshotRequest("snap-b"))
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorRuntimeUnavailable, failure.Code)
}

func TestCreateSnapshotRejectsStaleAssignmentFence(t *testing.T) {
	runtime := newSnapshotRuntime()
	manager := newSnapshotManager(t, runtime)
	stale := snapshotRequest("snap-a")
	stale.Identity.Sandbox.FastletPodUID = "pod-replaced"
	_, err := manager.CreateSnapshot(context.Background(), stale)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorStaleAssignment, failure.Code)
}

func TestSnapshotWorkerFailsWithoutPublishingOnDriverError(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.createErr = errors.New("dump failed")
	manager := newSnapshotManager(t, runtime)
	_, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseFailed
	}, 2*time.Second, 10*time.Millisecond)
	inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
	require.NoError(t, err)
	require.Contains(t, inspected.Snapshot.Message, "dump failed")
	require.Empty(t, inspected.Snapshot.ManifestRef)
}

func TestInspectSnapshotUnknownTaskIsNotFound(t *testing.T) {
	manager := newSnapshotManager(t, newSnapshotRuntime())
	_, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotRequest("snap-ghost").Identity})
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorNotFound, failure.Code)
}

func TestDeleteSnapshotWaitsForTerminalThenCleansArtifacts(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.block = make(chan struct{})
	manager := newSnapshotManager(t, runtime)
	response, err := manager.CreateSnapshot(context.Background(), snapshotRequest("snap-a"))
	require.NoError(t, err)

	_, err = manager.DeleteSnapshot(context.Background(), &fastletapi.DeleteSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorInProgress, failure.Code)

	close(runtime.block)
	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
		return err == nil && fastletapi.SnapshotPhaseTerminal(inspected.Snapshot.Phase)
	}, 2*time.Second, 10*time.Millisecond)

	_, err = manager.DeleteSnapshot(context.Background(), &fastletapi.DeleteSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
	require.NoError(t, err)
	runtime.mu.Lock()
	require.Equal(t, []string{response.Snapshot.SnapshotID}, runtime.deleted)
	runtime.mu.Unlock()

	_, err = manager.DeleteSnapshot(context.Background(), &fastletapi.DeleteSnapshotRequest{Identity: snapshotIdentityFor("snap-a")})
	require.NoError(t, err, "cleanup is idempotent for unknown tasks")
}

func snapshotIdentityFor(snapshotUID string) fastletapi.SnapshotIdentity {
	return snapshotRequest(snapshotUID).Identity
}

func checkpointRequest(checkpointUID string) *fastletapi.CreateSnapshotRequest {
	request := snapshotRequest(checkpointUID)
	request.Snapshot = fastletapi.SnapshotSpec{Kind: fastletapi.SnapshotKindCheckpoint}
	return request
}

func TestCreateCheckpointAdmitsWithoutTemplateName(t *testing.T) {
	runtime := newSnapshotRuntime()
	manager := newSnapshotManager(t, runtime)
	response, err := manager.CreateSnapshot(context.Background(), checkpointRequest("ckpt-a"))
	require.NoError(t, err)
	require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)

	require.Eventually(t, func() bool {
		inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("ckpt-a")})
		return err == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseSucceeded
	}, 2*time.Second, 10*time.Millisecond)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	require.Equal(t, fastletapi.SnapshotKindCheckpoint, runtime.lastInput.Kind)
	require.Empty(t, runtime.lastInput.TemplateName)
}

func TestCreateCheckpointRejectsTemplateName(t *testing.T) {
	manager := newSnapshotManager(t, newSnapshotRuntime())
	request := checkpointRequest("ckpt-a")
	request.Snapshot.TemplateName = "app-v2"
	_, err := manager.CreateSnapshot(context.Background(), request)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorConflict, failure.Code)
	require.Contains(t, failure.Message, "templateName must be empty")
}

func TestCreateTemplateSnapshotStillRequiresTemplateName(t *testing.T) {
	manager := newSnapshotManager(t, newSnapshotRuntime())
	request := snapshotRequest("snap-a")
	request.Snapshot.TemplateName = ""
	_, err := manager.CreateSnapshot(context.Background(), request)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorConflict, failure.Code)
	require.Contains(t, failure.Message, "templateName is required")
}

func TestCreateOCITemplateSnapshotUsesIndependentUUIDsWithoutNameFence(t *testing.T) {
	runtime := newSnapshotRuntime()
	runtime.block = make(chan struct{})
	runtime.blockBeforePublishing = true
	defer close(runtime.block)
	manager := newSnapshotManager(t, runtime)
	manager.sandboxes["uid-a"].Config.Desired.Image = "oci://r.example/ns/template:base"
	var ids []string
	for _, taskID := range []string{"first", "second"} {
		request := snapshotRequest(taskID)
		request.Snapshot.TemplateName = ""
		response, err := manager.CreateSnapshot(t.Context(), request)
		require.NoError(t, err)
		_, err = uuid.Parse(response.Snapshot.SnapshotID)
		require.NoError(t, err)
		ids = append(ids, response.Snapshot.SnapshotID)
		require.Eventually(t, func() bool {
			v, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor(taskID)})
			return err == nil && v.Snapshot.Phase == fastletapi.SnapshotPhasePublishing
		}, 2*time.Second, 10*time.Millisecond)
	}
	require.NotEqual(t, ids[0], ids[1])
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	require.Equal(t, sandboxStateRunning, manager.sandboxes["uid-a"].Phase)
}

func TestSnapshotTaskStatusEchoesMode(t *testing.T) {
	// In flight, the status reports the admitted mode...
	task := &snapshotTask{mode: fastletapi.SnapshotModeRootfsOnly, phase: fastletapi.SnapshotPhaseCreating}
	require.Equal(t, fastletapi.SnapshotModeRootfsOnly, task.status().Mode)
	// ...and once terminal, the produced artifact's class from the result.
	task.result = &SnapshotResult{Mode: fastletapi.SnapshotModeRootfsOnly}
	require.Equal(t, fastletapi.SnapshotModeRootfsOnly, task.status().Mode)
	// A result without a mode report falls back to the admitted mode.
	task.result = &SnapshotResult{}
	require.Equal(t, fastletapi.SnapshotModeRootfsOnly, task.status().Mode)
	// A legacy Full task normalizes an absent mode.
	legacy := &snapshotTask{phase: fastletapi.SnapshotPhasePending}
	require.Equal(t, fastletapi.SnapshotModeFull, legacy.status().Mode)
}

// modeBlindRuntime strips the produced-mode report, emulating a runtime
// that predates the mode contract.
type modeBlindRuntime struct{ *snapshotRuntime }

func (r *modeBlindRuntime) CreateSnapshot(ctx context.Context, input *RuntimeSnapshotInput) (*SnapshotResult, error) {
	result, err := r.snapshotRuntime.CreateSnapshot(ctx, input)
	if result != nil {
		result.Mode = ""
	}
	return result, err
}

func TestSnapshotWorkerFailsClosedOnProducedModeDrift(t *testing.T) {
	runtime := &modeBlindRuntime{snapshotRuntime: newSnapshotRuntime()}
	manager := newSnapshotManager(t, runtime)
	manager.sandboxes["uid-a"].Config.Desired.Image = "oci://r.example/ns/template:base"
	request := snapshotRequest("snap-drift")
	request.Snapshot.Mode = fastletapi.SnapshotModeRootfsOnly
	request.Snapshot.TemplateName = ""
	_, err := manager.CreateSnapshot(t.Context(), request)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		inspected, inspectErr := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-drift")})
		return inspectErr == nil && inspected.Snapshot.Phase == fastletapi.SnapshotPhaseFailed
	}, 2*time.Second, 10*time.Millisecond)
	inspected, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snapshotIdentityFor("snap-drift")})
	require.NoError(t, err)
	require.Equal(t, "SnapshotModeDrift", inspected.Snapshot.Reason)
	require.Contains(t, inspected.Snapshot.Message, "admitted mode")
	// The artifact was already published when the drift was detected: the
	// reference stays on the task so deleting the object can clean up the
	// remote set instead of orphaning it.
	require.NotEmpty(t, inspected.Snapshot.ManifestRef, "the drifted artifact reference must be retained for cleanup")
	require.NotEmpty(t, inspected.Snapshot.ArtifactDigest)
	// The task itself never reports success.
	require.Equal(t, fastletapi.SnapshotPhaseFailed, inspected.Snapshot.Phase)
}

// capableSnapshotRuntime advertises the rootfs-only capture branch.
type capableSnapshotRuntime struct{ *snapshotRuntime }

func (capableSnapshotRuntime) SupportsRootfsOnlySnapshots() bool { return true }

func TestSnapshotCapabilitiesFollowRuntimeSupport(t *testing.T) {
	// A runtime without the Snapshotter extension reports nothing.
	require.Empty(t, newSnapshotManager(t, newAdmissionRuntime()).SnapshotCapabilities())
	// A mode-aware snapshotter that did not implement the capability probe
	// reports nothing (fail closed for version skew).
	require.Empty(t, newSnapshotManager(t, newSnapshotRuntime()).SnapshotCapabilities())
	// A runtime with the rootfs-only branch advertises exactly that.
	require.Equal(t, []string{fastletapi.CapabilityRootfsOnlySnapshots},
		newSnapshotManager(t, &capableSnapshotRuntime{snapshotRuntime: newSnapshotRuntime()}).SnapshotCapabilities())
}
