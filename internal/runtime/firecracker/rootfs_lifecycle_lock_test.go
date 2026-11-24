package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSandboxLifecycleLockChild(t *testing.T) {
	root := os.Getenv("FSB_LIFECYCLE_LOCK_CHILD")
	if root == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	unlock, err := lockSandboxLifecycle(ctx, root, "sandbox")
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock unexpectedly acquired: %v", err)
	}
}
func TestSandboxLifecycleLockSurvivesDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	release, err := lockSandboxLifecycle(t.Context(), root, "sandbox")
	require.NoError(t, err)
	child := exec.Command(os.Args[0], "-test.run=^TestSandboxLifecycleLockChild$")
	child.Env = append(os.Environ(), "FSB_LIFECYCLE_LOCK_CHILD="+root)
	out, err := child.CombinedOutput()
	require.NoError(t, err, string(out))
	dir := filepath.Join(root, "sandboxes", "sandbox")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0700))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = lockSandboxLifecycle(ctx, root, "sandbox")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	release()
	next, err := lockSandboxLifecycle(t.Context(), root, "sandbox")
	require.NoError(t, err)
	next()
}
func TestSandboxLifecycleLockRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "lifecycle-locks"), 0700))
	require.NoError(t, os.Symlink(filepath.Join(root, "victim"), filepath.Join(root, "lifecycle-locks", "sandbox.lock")))
	_, err := lockSandboxLifecycle(t.Context(), root, "sandbox")
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(root, "victim"))
}

func TestSandboxMutationFencedByCaptureIntent(t *testing.T) {
	for _, phase := range []capturePhase{capturePrepared, captureFreezing, capturePausing, captureFlushing, captureCutting, captureRecovering, captureQuarantined, captureRecovered} {
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, sandboxStateDir, "sandbox")
			require.NoError(t, os.MkdirAll(dir, 0700))
			f := &captureFixture{}
			tx := f.transaction()
			j, err := newCaptureJournal(filepath.Join(dir, "rootfs-capture"), captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
			require.NoError(t, err)
			j.record.Phase = phase
			require.NoError(t, j.persist())
			unlock, err := lockSandboxMutation(t.Context(), root, "sandbox")
			if phase == captureRecovered {
				require.NoError(t, err)
				unlock()
			} else {
				require.ErrorContains(t, err, "capture fence")
				require.Nil(t, unlock)
			}
			// Release the mutex after rejecting an operation so the reconciler can acquire it.
			release, err := lockSandboxLifecycle(t.Context(), root, "sandbox")
			require.NoError(t, err)
			release()
		})
	}
}

func TestDriverLifecyclePreservesUnreconciledCapture(t *testing.T) {
	fixture := newDriverFixture(t)
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "true")
	fixture.sandboxSpec.Desired.Image = "oci://example.com/rootfs:test"
	require.NoError(t, fixture.driver.Initialize(t.Context(), ""))
	directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{Config: fixture.sandboxSpec, Phase: PhaseRunning, PID: 777, APIAddress: filepath.Join(directory, "api.sock")}))
	f := &captureFixture{}
	tx := f.transaction()
	_, err = newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
	require.NoError(t, err)
	require.ErrorContains(t, fixture.driver.DeleteSandbox(t.Context(), "sandbox-1"), "capture fence")
	require.ErrorContains(t, fixture.driver.RecordTerminalFailure(t.Context(), fixture.sandboxSpec.Identity, "exit"), "capture fence")
	require.ErrorContains(t, fixture.driver.RecoverRuntimeResources(t.Context(), nil, nil), "capture fence")
	_, err = fixture.driver.EnsureSandbox(t.Context(), ensureInput(&fixture.sandboxSpec))
	require.ErrorContains(t, err, "capture fence")
	require.ErrorContains(t, fixture.driver.dumpRunningSandbox(t.Context(), &dumpPlan{stateRoot: fixture.stateRoot, sandboxID: "sandbox-1"}), "capture fence")
	state, err := loadState(directory)
	require.NoError(t, err)
	require.Equal(t, PhaseRunning, state.Phase)
	require.Equal(t, 777, state.PID)
	require.Empty(t, fixture.killCalls)
}

func TestRootfsRecoveryRequiredOverridesLiveAPI(t *testing.T) {
	for _, failure := range []string{"resume-not-delivered", "thaw-failed"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newDriverFixture(t)
			directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
			require.NoError(t, err)
			state := &SandboxState{Config: fixture.sandboxSpec, Phase: PhaseRunning, PID: 777, APIAddress: "/unused/api.sock"}
			require.NoError(t, saveState(directory, state))
			f := &captureFixture{}
			j, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &state.Config.Identity, Incarnation: "boot", Device: "/dev/test", Staging: "/tmp/cut", Request: f.transaction().Request})
			require.NoError(t, err)
			require.NoError(t, j.Quarantine(errors.New(failure)))
			metadata, err := fixture.driver.InspectSandbox(t.Context(), "sandbox-1")
			require.NoError(t, err)
			require.True(t, metadata.RecoveryRequired)
			require.Equal(t, "RecoveryRequired", metadata.Phase)
			_, err = fixture.driver.GetAccessDescriptor("sandbox-1")
			require.ErrorContains(t, err, "capture fence")
			saved, err := loadState(directory)
			require.NoError(t, err)
			require.Equal(t, PhaseRunning, saved.Phase, "readiness must not declare the fenced runtime dead")
			require.Empty(t, fixture.killCalls)
		})
	}
}
