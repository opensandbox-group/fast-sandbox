//go:build linux

package firecracker

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
