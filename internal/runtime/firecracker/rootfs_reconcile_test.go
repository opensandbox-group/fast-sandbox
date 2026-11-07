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
}

func (f *recoveryFixture) VerifyIdentity(context.Context, captureRecord) error {
	f.calls = append(f.calls, "identity")
	if f.identityError {
		return errors.New("different VMM incarnation")
	}
	return nil
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
