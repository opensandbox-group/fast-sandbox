package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
