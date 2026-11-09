package firecracker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recoveryFixture struct {
	*captureFixture
	state         string
	identityError bool
	probeError    error
}

func (f *recoveryFixture) VerifyIdentity(ctx context.Context, _ captureRecord) error {
	f.calls = append(f.calls, "identity")
	if f.identityError {
		return errCaptureIdentityMismatch
	}
	return errors.Join(ctx.Err(), f.probeError)
}
func (f *recoveryFixture) VMState(context.Context) (string, error) {
	f.calls = append(f.calls, "state")
	return f.state, nil
}
func TestCaptureRestartRecovery(t *testing.T) {
	for _, phase := range []capturePhase{capturePrepared, captureFreezing, capturePausing, captureFlushing, captureCutting, captureRecovering} {
		for _, scenario := range []string{"running", "paused", "identity-mismatch", "resume-error", "thaw-error"} {
			t.Run(string(phase)+"/"+scenario, func(t *testing.T) {
				f := &recoveryFixture{captureFixture: &captureFixture{}, state: "Running"}
				tx := f.transaction()
				dir := filepath.Join(t.TempDir(), "capture")
				j, e := newCaptureJournal(dir, captureRecord{Incarnation: "boot-1", Device: "/dev/ublkb0", Staging: "/tmp/cut", Request: tx.Request})
				require.NoError(t, e)
				j.record.Phase = phase
				require.NoError(t, j.persist())
				j, e = loadCaptureJournal(dir)
				require.NoError(t, e)
				if scenario == "paused" || scenario == "resume-error" {
					f.state = "Paused"
				}
				if scenario == "identity-mismatch" {
					f.identityError = true
				}
				if scenario == "resume-error" {
					f.fail = "resume"
				}
				if scenario == "thaw-error" {
					f.fail = "thaw"
				}
				e = reconcileCapture(t.Context(), j, f, f, time.Second)
				saved, readErr := loadCaptureJournal(dir)
				require.NoError(t, readErr)
				if scenario == "running" || scenario == "paused" {
					require.NoError(t, e)
					require.Equal(t, captureRecovered, saved.record.Phase)
				} else {
					require.Error(t, e)
					require.Equal(t, captureQuarantined, saved.record.Phase)
				}
				if scenario == "identity-mismatch" {
					require.Equal(t, []string{"identity"}, f.calls)
				}
				if scenario == "resume-error" {
					require.NotContains(t, f.calls, "thaw")
				}
				require.NotContains(t, f.calls, "cut")
			})
		}
	}
}

func TestCaptureRecoveryRetriesQuarantineAfterReload(t *testing.T) {
	for _, failure := range []string{"resume", "thaw"} {
		t.Run(failure, func(t *testing.T) {
			f := &recoveryFixture{captureFixture: &captureFixture{fail: failure}, state: vmPaused}
			j, err := newCaptureJournal(filepath.Join(t.TempDir(), "capture"), captureRecord{Incarnation: "boot-1", Device: "/dev/test", Staging: "/tmp/cut", Request: f.transaction().Request})
			require.NoError(t, err)
			require.Error(t, reconcileCapture(t.Context(), j, f, f, time.Second))
			j, err = loadCaptureJournal(j.directory)
			require.NoError(t, err)
			require.Equal(t, captureQuarantined, j.record.Phase)
			f.fail, f.calls = "", nil
			// Quarantine still rejects a replacement, even when transport works.
			f.identityError = true
			require.ErrorIs(t, reconcileCapture(t.Context(), j, f, f, time.Second), errCaptureIdentityMismatch)
			require.Equal(t, []string{"identity"}, f.calls)
			f.identityError, f.calls = false, nil
			require.NoError(t, reconcileCapture(t.Context(), j, f, f, time.Second))
			require.Equal(t, []string{"identity", "state", "resume", "thaw"}, f.calls)
			saved, err := loadCaptureJournal(j.directory)
			require.NoError(t, err)
			require.Equal(t, captureRecovered, saved.record.Phase)
		})
	}
}

func TestCaptureRecoveryProbeFailureIsRetryable(t *testing.T) {
	for _, phase := range []capturePhase{capturePrepared, captureQuarantined} {
		for _, failure := range []string{"canceled", "deadline", "transport"} {
			t.Run(string(phase)+"/"+failure, func(t *testing.T) {
				f := &recoveryFixture{captureFixture: &captureFixture{}, state: vmRunning}
				j, err := newCaptureJournal(filepath.Join(t.TempDir(), "capture"), captureRecord{Incarnation: "boot-1", Device: "/dev/test", Staging: "/tmp/cut", Request: f.transaction().Request})
				require.NoError(t, err)
				j.record.Phase = phase
				require.NoError(t, j.persist())
				ctx := t.Context()
				if failure == "canceled" {
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = canceled
				} else if failure == "deadline" {
					f.probeError = context.DeadlineExceeded
				} else {
					f.probeError = errors.New("connection refused")
				}
				require.Error(t, reconcileCapture(ctx, j, f, f, time.Second))
				require.Equal(t, []string{"identity"}, f.calls)
				j, err = loadCaptureJournal(j.directory)
				require.NoError(t, err)
				require.Equal(t, phase, j.record.Phase)
				f.probeError, f.calls = nil, nil
				require.NoError(t, reconcileCapture(t.Context(), j, f, f, time.Second))
				require.Equal(t, []string{"identity", "state", "thaw"}, f.calls)
			})
		}
	}
}
