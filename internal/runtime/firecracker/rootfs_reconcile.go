package firecracker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// captureRecoveryTarget is constructed by a caller holding the lifecycle lock to verify process incarnation, device, and upper ownership.
// VerifyIdentity must complete before any VM/Guest command; comparing reusable PIDs alone is insufficient.
type captureRecoveryTarget interface {
	VerifyIdentity(context.Context, captureRecord) error
	VMState(context.Context) (string, error)
	Resume(context.Context) error
}

// reconcileCapture restores only the source; it neither accepts an incomplete transaction's cut nor publishes leftover files.
// Callers must retain records and quarantined resources until failed artifacts are cleaned up, then release the lifecycle lock.
func reconcileCapture(ctx context.Context, j *captureJournal, target captureRecoveryTarget, guest rootfsCaptureGuest, timeout time.Duration) (err error) {
	if j == nil || target == nil || guest == nil || timeout <= 0 {
		return errors.New("incomplete capture recovery")
	}
	if j.record.Phase == captureRecovered {
		return nil
	}
	if j.record.Phase == captureQuarantined {
		return errors.New("capture quarantined; explicit reconciliation required")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, j.Quarantine(err))
		}
	}()
	if err = target.VerifyIdentity(ctx, j.record); err != nil {
		return fmt.Errorf("verify recovery identity: %w", err)
	}
	if err = j.Record(captureRecovering); err != nil {
		return err
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	state, e := target.VMState(recovery)
	if e != nil {
		return e
	}
	switch state {
	case vmPaused:
		if err = target.Resume(recovery); err != nil {
			return err
		}
	case vmRunning:
	default:
		return fmt.Errorf("cannot recover VM in state %s", state)
	}
	// Send a terminal token even for a prepared record to prevent a delayed freeze with a lost response from taking effect.
	response, e := guest.ThawAndWait(recovery, j.record.Request)
	if e != nil {
		return e
	}
	tx := rootfsCapture{Request: j.record.Request}
	if err = tx.checkGuestResponse(response, false); err != nil {
		return err
	}
	return j.Record(captureRecovered)
}
