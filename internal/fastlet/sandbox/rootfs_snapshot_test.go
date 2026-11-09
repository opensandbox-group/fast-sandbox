package sandbox

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/dataplane/fastletproxy"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestRootfsSnapshotAdmissionAndModeFence(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		mode        fastletapi.SnapshotMode
		kind        fastletapi.SnapshotKind
		allowed     bool
	}{
		{"rootfs", "oci://registry/repo:tag", fastletapi.SnapshotModeRootfsOnly, fastletapi.SnapshotKindTemplate, true},
		{"legacy-full", "alpine:latest", "", "", true},
		{"invalid-mode", "oci://registry/repo:tag", "future", "", false},
		{"checkpoint", "oci://registry/repo:tag", fastletapi.SnapshotModeRootfsOnly, fastletapi.SnapshotKindCheckpoint, false},
		{"native", "s3://bucket/template", fastletapi.SnapshotModeRootfsOnly, fastletapi.SnapshotKindTemplate, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newSnapshotRuntime()
			manager := newSnapshotManager(t, runtime)
			manager.sandboxes["uid-a"].Config.Desired.Image = tc.image
			req := snapshotRequest("mode-test")
			req.Snapshot.Mode = tc.mode
			req.Snapshot.Kind = tc.kind
			if tc.kind == fastletapi.SnapshotKindCheckpoint {
				req.Snapshot.TemplateName = ""
			}
			_, err := manager.CreateSnapshot(context.Background(), req)
			if !tc.allowed {
				require.Error(t, err)
				require.Empty(t, manager.snapshots)
				require.Zero(t, runtime.createCalls)
				return
			}
			require.NoError(t, err)
			require.Eventually(t, func() bool { runtime.mu.Lock(); defer runtime.mu.Unlock(); return runtime.createCalls == 1 }, time.Second, time.Millisecond)
			runtime.mu.Lock()
			input := runtime.lastInput
			runtime.mu.Unlock()
			require.Equal(t, fastletapi.NormalizeSnapshotMode(tc.mode), input.Mode)
			require.Equal(t, req.Identity.Sandbox, *input.Identity)
			replay, err := manager.CreateSnapshot(context.Background(), req)
			require.NoError(t, err)
			require.Equal(t, fastletapi.CreateDispositionExisting, replay.Disposition)
			if tc.mode == fastletapi.SnapshotModeRootfsOnly {
				req.Snapshot.Mode = fastletapi.SnapshotModeFull
			} else {
				req.Snapshot.Mode = fastletapi.SnapshotModeRootfsOnly
			}
			_, err = manager.CreateSnapshot(context.Background(), req)
			require.Error(t, err)
		})
	}
}

func TestRootfsSnapshotWorkerRejectsReplacedSandbox(t *testing.T) {
	runtime := newSnapshotRuntime()
	manager := newSnapshotManager(t, runtime)
	req := snapshotRequest("stale")
	task := &snapshotTask{identity: req.Identity, mode: fastletapi.SnapshotModeRootfsOnly, phase: fastletapi.SnapshotPhasePending}
	manager.sandboxes["uid-a"].Config.Identity.RuntimeInstanceID = "replacement"
	manager.runSnapshotWorker(runtime, task)
	require.Zero(t, runtime.createCalls)
	require.Equal(t, fastletapi.SnapshotPhaseFailed, task.phase)
}

// The runtime can keep a live VMM fenced after either Resume or Thaw fails.
type recoverySnapshotRuntime struct {
	*snapshotRuntime
	recoveryMu    sync.Mutex
	required      bool
	recoveryErr   error
	recoveryCalls int
	inspectCalls  int
	inspectErr    error
	recoveryBlock <-chan struct{}
	identity      fastletapi.SandboxIdentity
}

func (r *recoverySnapshotRuntime) InspectSandbox(context.Context, string) (*SandboxMetadata, error) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()
	r.inspectCalls++
	if r.inspectErr != nil {
		return nil, r.inspectErr
	}
	return &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: r.identity}, Phase: "Running", RecoveryRequired: r.required, RuntimeMessage: "capture recovery pending"}, nil
}
func (r *recoverySnapshotRuntime) ReconcileSandbox(ctx context.Context, identity fastletapi.SandboxIdentity) error {
	r.recoveryMu.Lock()
	if identity != r.identity {
		r.recoveryMu.Unlock()
		return errors.New("identity mismatch")
	}
	r.recoveryCalls++
	block, recoveryErr := r.recoveryBlock, r.recoveryErr
	r.recoveryMu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-block:
		}
	}
	if recoveryErr != nil {
		return recoveryErr
	}
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()
	if identity != r.identity {
		return errors.New("identity mismatch")
	}
	r.required = false
	return nil
}

func TestRootfsSnapshotFailureWithdrawsReadinessAndRetriesRecovery(t *testing.T) {
	for _, failure := range []string{"resume-not-delivered", "thaw-failed"} {
		t.Run(failure, func(t *testing.T) {
			r := &recoverySnapshotRuntime{snapshotRuntime: newSnapshotRuntime(), required: true, recoveryErr: errors.New("temporarily unavailable")}
			r.createErr = errors.New(failure)
			m := newSnapshotManager(t, r)
			publisher := &admissionRoutePublisher{}
			m.routePublisher, m.routeReady = publisher, true
			meta := m.sandboxes["uid-a"]
			r.identity = meta.Config.Identity
			r.admissionRuntime.sandboxes["uid-a"] = &SandboxMetadata{Config: meta.Config}
			req := snapshotRequest("recovery")
			task := &snapshotTask{identity: req.Identity, snapshotID: "recovery", phase: fastletapi.SnapshotPhasePending}
			m.snapshots[req.Identity.SnapshotUID] = task
			m.runSnapshotWorker(r, task)
			require.Equal(t, fastletapi.SnapshotPhaseFailed, task.phase)
			require.True(t, meta.RecoveryRequired)
			require.Zero(t, r.recoveryCalls, "worker observes failure; it must not hide it by immediate recovery")
			publisher.mu.Lock()
			require.NotEmpty(t, publisher.removed)
			publisher.mu.Unlock()
			response, err := m.InspectSandbox(&fastletapi.InspectSandboxRequest{Identity: r.identity})
			require.NoError(t, err)
			require.Equal(t, fastletapi.RuntimeStateUnavailable, response.Sandbox.Runtime.State)
			require.Equal(t, fastletapi.DataPlaneStateUnavailable, response.Sandbox.DataPlane.State)
			statuses := m.GetSandboxStatuses(t.Context())
			require.Equal(t, fastletapi.RuntimeStateUnavailable, statuses[0].Runtime.State)
			require.Eventually(t, func() bool {
				m.mu.RLock()
				done := len(m.recoveryWorkers) == 0
				m.mu.RUnlock()
				r.recoveryMu.Lock()
				defer r.recoveryMu.Unlock()
				return done && r.recoveryCalls == 1
			}, time.Second, time.Millisecond)
			require.Zero(t, r.deleteCalls)
			require.ErrorIs(t, m.ReconcileProxyRoutes(t.Context()), ErrInfraUnavailable)
			publisher.mu.Lock()
			require.Empty(t, publisher.reconciled, "quarantined routes must not be permanently deleted during reconciliation")
			publisher.mu.Unlock()
			_, err = m.CreateSnapshot(t.Context(), snapshotRequest("another"))
			requireFastletCode(t, err, fastletapi.ErrorRuntimeUnavailable)
			r.recoveryMu.Lock()
			r.recoveryErr = nil
			r.recoveryMu.Unlock()
			m.GetSandboxStatuses(t.Context())
			require.Eventually(t, func() bool {
				m.mu.RLock()
				defer m.mu.RUnlock()
				return !meta.RecoveryRequired && meta.Phase == sandboxStateRunning
			}, time.Second, time.Millisecond)
			require.Zero(t, r.deleteCalls, "recovery must not destroy the source")
		})
	}
}

func TestRootfsRecoveryWorkerDoesNotBlockHeartbeatsOrTouchReplacement(t *testing.T) {
	block := make(chan struct{})
	r := &recoverySnapshotRuntime{snapshotRuntime: newSnapshotRuntime(), required: true, recoveryBlock: block}
	m := newSnapshotManager(t, r)
	original := m.sandboxes["uid-a"]
	r.identity = original.Config.Identity
	m.GetSandboxStatuses(t.Context())
	require.Eventually(t, func() bool {
		r.recoveryMu.Lock()
		defer r.recoveryMu.Unlock()
		return r.recoveryCalls == 1
	}, time.Second, time.Millisecond)
	// The Guest recovery remains blocked, but repeated status reads finish.
	completed := make(chan struct{})
	go func() {
		for range 3 {
			m.GetSandboxStatuses(t.Context())
		}
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		close(block)
		t.Fatal("Guest recovery blocked heartbeat")
	}
	r.recoveryMu.Lock()
	require.Equal(t, 1, r.recoveryCalls)
	r.recoveryMu.Unlock()
	m.mu.Lock()
	replacement := &SandboxMetadata{Config: original.Config, Phase: sandboxStateCreating, RecoveryRequired: true}
	replacement.Config.Identity.RuntimeInstanceID = "replacement"
	m.sandboxes["uid-a"] = replacement
	m.mu.Unlock()
	close(block)
	require.Eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return len(m.recoveryWorkers) == 0
	}, time.Second, time.Millisecond)
	m.mu.RLock()
	defer m.mu.RUnlock()
	require.True(t, replacement.RecoveryRequired)
	require.Equal(t, sandboxStateCreating, replacement.Phase)
}

func TestRootfsRecoveryRestoresRealProxyRoute(t *testing.T) {
	r := &recoverySnapshotRuntime{snapshotRuntime: newSnapshotRuntime()}
	m := newSnapshotManager(t, r)
	meta := m.sandboxes["uid-a"]
	r.identity = meta.Config.Identity
	r.admissionRuntime.sandboxes["uid-a"] = &SandboxMetadata{Config: meta.Config}
	store := fastletproxy.NewStore()
	socket := filepath.Join(t.TempDir(), "proxy.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- (&fastletproxy.ControlServer{Store: store, SocketPath: socket}).Serve(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	client := fastletproxy.NewControlClient(socket)
	require.Eventually(t, func() bool { _, err := client.Snapshot(t.Context()); return err == nil }, time.Second, time.Millisecond)
	m.routePublisher = fastletproxy.NewRoutePublisher(client)
	m.routeReady = true
	require.NoError(t, m.publishRoute(t.Context(), meta))
	_, err := store.Lookup("uid-a")
	require.NoError(t, err)
	r.recoveryMu.Lock()
	r.required = true
	r.recoveryMu.Unlock()
	m.refreshRuntimeRecovery(t.Context(), r.identity, false)
	_, err = store.Lookup("uid-a")
	require.ErrorIs(t, err, fastletproxy.ErrRouteDraining)
	require.ErrorIs(t, m.ReconcileProxyRoutes(t.Context()), ErrInfraUnavailable)
	require.Len(t, store.Snapshot().Routes, 1, "temporary quarantine must retain route ownership")
	r.recoveryMu.Lock()
	r.required = false
	r.recoveryMu.Unlock()
	m.refreshRuntimeRecovery(t.Context(), r.identity, false)
	require.Eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return meta.Phase == sandboxStateRunning && !meta.RecoveryRequired
	}, time.Second, time.Millisecond)
	require.NoError(t, m.ReconcileProxyRoutes(t.Context()))
	route, err := store.Lookup("uid-a")
	require.NoError(t, err)
	require.Equal(t, r.identity.RouteGeneration, route.RouteGeneration)
	require.NoError(t, m.removeRoute(t.Context(), meta))
	require.Error(t, m.publishRoute(t.Context(), meta), "accepted deletion must not be undone by late publication")
}

func TestRootfsDeletingRecoveryRetriesWithoutRestoringReadiness(t *testing.T) {
	r := &recoverySnapshotRuntime{snapshotRuntime: newSnapshotRuntime(), required: true}
	m := newSnapshotManager(t, r)
	meta := m.sandboxes["uid-a"]
	r.identity = meta.Config.Identity
	meta.RecoveryRequired = true
	publisher := &admissionRoutePublisher{}
	m.routePublisher, m.routeReady = publisher, true
	r.admissionRuntime.deleteError = errors.New("temporary capture recovery failure")
	_, err := m.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: r.identity})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return meta.Phase == sandboxStateDeleteFailed && m.deleteWorkers["uid-a"] == nil
	}, time.Second, time.Millisecond)
	r.admissionRuntime.mu.Lock()
	r.admissionRuntime.deleteError = nil
	r.admissionRuntime.mu.Unlock()
	m.GetSandboxStatuses(t.Context())
	require.Eventually(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.sandboxes["uid-a"] == nil
	}, time.Second, time.Millisecond)
	r.recoveryMu.Lock()
	require.Zero(t, r.recoveryCalls, "only the deleting driver may recover the source")
	r.recoveryMu.Unlock()
	publisher.mu.Lock()
	require.Empty(t, publisher.applied, "deletion must never republish access")
	publisher.mu.Unlock()
}

func TestRootfsHeartbeatUsesOneRuntimeObservation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "probe-failure"}[failed], func(t *testing.T) {
			r := &recoverySnapshotRuntime{snapshotRuntime: newSnapshotRuntime()}
			m := newSnapshotManager(t, r)
			r.identity = m.sandboxes["uid-a"].Config.Identity
			if failed {
				r.inspectErr = errors.New("temporary API failure")
			}
			statuses := m.GetSandboxStatuses(t.Context())
			require.Len(t, statuses, 1)
			r.recoveryMu.Lock()
			defer r.recoveryMu.Unlock()
			require.Equal(t, 1, r.inspectCalls, "a heartbeat must not perform a second Inspect even after probe failure")
		})
	}
}
