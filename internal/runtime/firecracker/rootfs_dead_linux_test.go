//go:build linux

package firecracker

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestRootfsDeadCaptureAbortsWithoutGuestOrDevice(t *testing.T) {
	for _, phase := range []capturePhase{captureFreezing, captureQuarantined} {
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			d := &Driver{config: firecrackerConfigForTest(t, root)}
			owner := fastletapi.SandboxIdentity{SandboxUID: "source", FastletPodUID: "pod", Namespace: "test"}
			directory := filepath.Join(root, sandboxStateDir, "source")
			require.NoError(t, os.MkdirAll(directory, 0700))
			command := exec.Command("true")
			require.NoError(t, command.Run())
			state := &SandboxState{PID: command.Process.Pid, Phase: PhaseRunning, Config: fastletapi.RuntimeSandboxConfig{Identity: owner}}
			require.NoError(t, saveState(directory, state))
			fixture := &captureFixture{}
			request := fixture.transaction().Request
			staging := snapshotStagingPath(root, "pod", request.OperationID, true)
			require.NoError(t, os.MkdirAll(staging, 0700))
			journal, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &owner, Incarnation: "dead", Device: "/nonexistent/device", Staging: staging, Request: request})
			require.NoError(t, err)
			journal.record.Phase = phase
			require.NoError(t, journal.persist())
			_, err = d.abortDeadRootfsCapture(t.Context(), state, directory, func(string) error { return errors.New("cgroup still populated") })
			require.ErrorContains(t, err, "cgroup still populated")
			require.Equal(t, PhaseRunning, state.Phase)
			require.DirExists(t, staging)
			dead, err := d.abortDeadRootfsCapture(t.Context(), state, directory, nil)
			require.NoError(t, err)
			require.True(t, dead)
			require.Equal(t, 0, state.PID)
			require.Equal(t, PhaseFailed, state.Phase)
			require.NoDirExists(t, staging)
			saved, err := loadCaptureJournal(journal.directory)
			require.NoError(t, err)
			require.Equal(t, captureAborted, saved.record.Phase)
			require.Error(t, saved.Record(captureRecovering))
			dead, err = d.abortDeadRootfsCapture(t.Context(), state, directory, nil)
			require.NoError(t, err)
			require.True(t, dead)
		})
	}
}
func TestRootfsLiveCaptureIsNotDeclaredDead(t *testing.T) {
	absent, err := capturePIDAbsent(os.Getpid())
	require.NoError(t, err)
	require.False(t, absent)
	_, err = capturePIDAbsent(0)
	require.Error(t, err)
}

func TestRootfsProcessIdentityFencesSocketReuse(t *testing.T) {
	socket := startFakeFirecracker(t, func(http.ResponseWriter, *http.Request) {})
	first, err := captureProcessIdentity(t.Context(), socket, os.Getpid())
	require.NoError(t, err)
	second, err := captureProcessIdentity(t.Context(), socket, os.Getpid())
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, err = captureProcessIdentity(t.Context(), socket, os.Getpid()+1)
	require.ErrorContains(t, err, "recorded VMM PID")
}

func TestRootfsLostPodFenceWaitsForResourceCleanup(t *testing.T) {
	for _, phase := range []capturePhase{captureFreezing, captureCutting, captureQuarantined, captureAborted} {
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			d := &Driver{config: firecrackerConfigForTest(t, root), podUID: "new-pod"}
			owner := fastletapi.SandboxIdentity{SandboxUID: "source", FastletPodUID: "old-pod"}
			directory := filepath.Join(root, sandboxStateDir, owner.SandboxUID)
			require.NoError(t, os.MkdirAll(directory, 0700))
			state := &SandboxState{PID: os.Getpid(), Phase: PhaseRunning, Config: fastletapi.RuntimeSandboxConfig{Identity: owner}}
			state.Allocation.Network = fastletapi.NetworkAllocation{SlotID: "old-slot", DNSPath: filepath.Join(root, "network", owner.FastletPodUID, "old-slot.resolv.conf")}
			networkRecord := filepath.Join(filepath.Dir(state.Allocation.Network.DNSPath), "old-slot.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(networkRecord), 0700))
			require.NoError(t, os.WriteFile(networkRecord, []byte("owned network slot"), 0600))
			require.NoError(t, os.WriteFile(state.Allocation.Network.DNSPath, nil, 0600))
			require.NoError(t, saveState(directory, state))
			f := &captureFixture{}
			req := f.transaction().Request
			staging := snapshotStagingPath(root, owner.FastletPodUID, req.OperationID, true)
			device := filepath.Join(root, "old-device")
			upper := filepath.Join(ociRootForPod(root, owner.FastletPodUID), "sandboxes", owner.SandboxUID)
			currentUpper := filepath.Join(d.ociRoot(), "sandboxes", owner.SandboxUID)
			require.NoError(t, os.MkdirAll(currentUpper, 0700))
			require.NoError(t, os.MkdirAll(staging, 0700))
			require.NoError(t, os.MkdirAll(upper, 0700))
			require.NoError(t, os.WriteFile(device, nil, 0600))
			j, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &owner, Incarnation: "old-vmm", Device: device, Staging: staging, Request: req})
			require.NoError(t, err)
			j.record.Phase = phase
			require.NoError(t, j.persist())
			// Each independent leftover must keep Ensure fenced until Janitor removes it.
			for _, resource := range []string{networkRecord, state.Allocation.Network.DNSPath, staging, upper} {
				unlock, err := d.lockSandboxEnsure(t.Context(), root, owner.SandboxUID)
				require.ErrorContains(t, err, "capture fence")
				require.Nil(t, unlock)
				require.NoError(t, os.RemoveAll(resource))
			}
			// A reused Pod-local PID and a stale device node do not name a live old VMM.
			require.FileExists(t, device)
			unlock, err := d.lockSandboxEnsure(t.Context(), root, owner.SandboxUID)
			require.NoError(t, err)
			unlock()
			saved, err := loadState(directory)
			require.NoError(t, err)
			require.Zero(t, saved.PID)
			require.Empty(t, saved.APIAddress)
			j, err = loadCaptureJournal(j.directory)
			require.NoError(t, err)
			require.Equal(t, captureAborted, j.record.Phase)
			require.DirExists(t, currentUpper, "old Pod reconciliation must not consume the replacement's resources")
			// A retry after terminal journal persistence is safe even if meta.json was stale.
			state.PID = os.Getpid()
			require.NoError(t, saveState(directory, state))
			unlock, err = d.lockSandboxEnsure(t.Context(), root, owner.SandboxUID)
			require.NoError(t, err)
			unlock()
			saved, err = loadState(directory)
			require.NoError(t, err)
			require.Zero(t, saved.PID)
		})
	}
}

func TestRootfsLostPodCaptureAllowsSameNodeEnsureAfterCleanup(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("native Full fixture requires x86 CPU identity; orphan fence recovery is tested above on all Linux architectures")
	}
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Desired.Image)
	require.NoError(t, fixture.driver.Initialize(t.Context(), ""))
	fixture.driver.SetFastletPodUID("replacement-pod")
	fixture.sandboxSpec.Identity.FastletPodUID = "replacement-pod"
	old := fixture.sandboxSpec
	old.Identity.FastletPodUID = "old-pod"
	directory, err := ensureSandboxDir(fixture.stateRoot, old.Identity.SandboxUID)
	require.NoError(t, err)
	process := exec.Command("true")
	require.NoError(t, process.Run())
	state := &SandboxState{Config: old, PID: process.Process.Pid, Phase: PhaseRunning}
	state.Allocation.Network = fastletapi.NetworkAllocation{SlotID: "old-slot", DNSPath: filepath.Join(fixture.stateRoot, "network", "old-pod", "old-slot.resolv.conf")}
	require.NoError(t, saveState(directory, state))
	f := &captureFixture{}
	req := f.transaction().Request
	j, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &old.Identity, Incarnation: "old-vmm", Device: filepath.Join(directory, "missing-device"), Staging: snapshotStagingPath(fixture.stateRoot, "old-pod", req.OperationID, true), Request: req})
	require.NoError(t, err)
	j.record.Phase = captureCutting
	require.NoError(t, j.persist())
	// Recovery inventory must not adopt another Pod. Ensure handles its retired fence.
	require.NoError(t, fixture.driver.RecoverRuntimeResources(t.Context(), nil, nil))
	metadata, err := fixture.driver.EnsureSandbox(t.Context(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Equal(t, fixture.sandboxSpec.Identity, metadata.Config.Identity)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
	require.Len(t, fixture.launcher.started, 1)
	require.NotContains(t, fixture.killCalls, process.Process.Pid)
	require.NoDirExists(t, filepath.Join(directory, "rootfs-capture"))
}

func TestRootfsRuntimeRecoveryAbortsExitedQuarantine(t *testing.T) {
	root := t.TempDir()
	d := &Driver{config: firecrackerConfigForTest(t, root), podUID: "pod"}
	owner := fastletapi.SandboxIdentity{SandboxUID: "exited", FastletPodUID: "pod", Namespace: "test"}
	dir, err := ensureSandboxDir(root, owner.SandboxUID)
	require.NoError(t, err)
	process := exec.Command("true")
	require.NoError(t, process.Run())
	state := &SandboxState{Config: fastletapi.RuntimeSandboxConfig{Identity: owner}, PID: process.Process.Pid, APIAddress: "/absent/socket", Phase: PhaseRunning}
	require.NoError(t, saveState(dir, state))
	fixture := &captureFixture{}
	req := fixture.transaction().Request
	staging := snapshotStagingPath(root, "pod", req.OperationID, true)
	require.NoError(t, os.MkdirAll(staging, 0700))
	j, err := newCaptureJournal(filepath.Join(dir, "rootfs-capture"), captureRecord{Owner: &owner, Incarnation: "dead", Device: "/absent/device", Staging: staging, Request: req})
	require.NoError(t, err)
	require.NoError(t, j.Quarantine(errors.New("resume lost")))
	// No startup recovery, API client, or live device is available.
	require.NoError(t, d.ReconcileSandbox(t.Context(), owner))
	observed, err := d.InspectSandbox(t.Context(), owner.SandboxUID)
	require.NoError(t, err)
	require.False(t, observed.RecoveryRequired)
	require.Equal(t, string(PhaseFailed), observed.Phase)
	require.NoDirExists(t, staging)
	require.NoError(t, checkSandboxCaptureFence(root, owner.SandboxUID))
	require.NoError(t, d.ReconcileSandbox(t.Context(), owner))
	j, err = loadCaptureJournal(j.directory)
	require.NoError(t, err)
	require.Equal(t, captureAborted, j.record.Phase)
}

func TestRootfsDeleteReconcilesCaptureBeforeCleanup(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(t.Context(), ""))
	owner := fixture.sandboxSpec.Identity
	directory, err := ensureSandboxDir(fixture.stateRoot, owner.SandboxUID)
	require.NoError(t, err)
	process := exec.Command("sleep", "60")
	require.NoError(t, process.Start())
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	state := &SandboxState{Config: fixture.sandboxSpec, PID: process.Process.Pid, APIAddress: "/absent/socket", Phase: PhaseRunning}
	require.NoError(t, saveState(directory, state))
	f := &captureFixture{}
	req := f.transaction().Request
	staging := snapshotStagingPath(fixture.stateRoot, owner.FastletPodUID, req.OperationID, true)
	require.NoError(t, os.MkdirAll(staging, 0700))
	j, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &owner, Incarnation: "source", Device: "/absent/device", Staging: staging, Request: req})
	require.NoError(t, err)
	require.NoError(t, j.Quarantine(errors.New("resume response lost")))
	// A failed transport probe is not evidence of death. Retain every resource.
	require.ErrorContains(t, fixture.driver.DeleteSandbox(t.Context(), owner.SandboxUID), "capture fence")
	require.DirExists(t, staging)
	require.Empty(t, fixture.killCalls)
	require.NoError(t, process.Process.Kill())
	_ = process.Wait()
	// A retry after independently proven exit must abort the capture and delete,
	// without depending on a Guest channel or a Fastlet restart.
	require.NoError(t, fixture.driver.DeleteSandbox(t.Context(), owner.SandboxUID))
	require.NoDirExists(t, staging)
	require.NoDirExists(t, directory)
}
