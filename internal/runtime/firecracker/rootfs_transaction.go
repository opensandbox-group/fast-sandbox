package firecracker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fast-sandbox/internal/guestcontrol"
)

// rootfsCaptureBoundary must come from a verifier holding a lifecycle lease, never directly from an API request.
// The verifier checks actual runtime configuration and device ownership, and excludes writers bypassing the target filesystem throughout the window.
type rootfsCaptureBoundary struct {
	IOEngine            string
	Filesystem          string
	FilesystemExclusive bool
}

func (b rootfsCaptureBoundary) validate() error {
	if b.IOEngine != "Sync" && b.IOEngine != "Async" {
		return fmt.Errorf("unsupported rootfs capture IO engine %q", b.IOEngine)
	}
	if b.Filesystem != rootfsFilesystem || !b.FilesystemExclusive {
		return errors.New("rootfs capture requires an exclusive ext4 write boundary")
	}
	return nil
}

// rootfsCaptureLease must be issued by a durable lifecycle coordinator and bind the VM instance, block device, and upper.
// Verify checks the actual engine, qualified VMM/OverlayBD versions, and exclusive device access, rather than trusting request parameters.
// Persist quarantine state; after a crash, reconcile the previous transaction before allowing destruction or another capture.
type rootfsCaptureLease interface {
	Verify(context.Context) (rootfsCaptureBoundary, error)
	Quarantine(error) error
	Release() error
}

type rootfsCaptureVM interface {
	Pause(context.Context) error
	Resume(context.Context) error
}

type rootfsCaptureGuest interface {
	FreezeAndWait(context.Context, guestcontrol.Request) (guestcontrol.Response, error)
	ThawAndWait(context.Context, guestcontrol.Request) (guestcontrol.Response, error)
}

// rootfsCapture handles only capture transactions with an exclusive lease; it does not publish artifacts.
// Flush/Cut must return synchronously; context cancellation must not leave background writes or copies running.
// Cut must fix all upper files within the protected window; failed artifacts must never be published.
type rootfsCapture struct {
	Progress        captureProgress
	Lease           rootfsCaptureLease
	VM              rootfsCaptureVM
	Guest           rootfsCaptureGuest
	Request         guestcontrol.Request
	Flush           func() error
	Cut             func() error
	RecoveryTimeout time.Duration
}

func (t rootfsCapture) run(ctx context.Context) (err error) {
	if t.Lease == nil || t.VM == nil || t.Guest == nil || t.Flush == nil || t.Cut == nil || t.RecoveryTimeout <= 0 {
		return errors.New("incomplete rootfs capture transaction")
	}
	if t.Request.Protocol != guestcontrol.Protocol || t.Request.Epoch == "" || t.Request.Generation == 0 || t.Request.OperationID == "" || len(t.Request.OperationID) > 128 || t.Request.LeaseMillis <= 0 || t.Request.LeaseMillis > int64((24*time.Hour)/time.Millisecond) {
		return errors.New("invalid rootfs capture operation identity or lease")
	}
	freezeAttempted, pauseAttempted := false, false
	sourceUncertain := false
	defer func() {
		// Unexpected panics still require source recovery and quarantine; preserve the original panic rather than converting it into publishable success.
		if failure := recover(); failure != nil {
			sourceUncertain = true
			err = errors.Join(err, errors.New("capture panicked"))
			defer func() { panic(failure) }()
		}
		err = t.recoverSource(ctx, freezeAttempted, pauseAttempted, sourceUncertain, err)
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	boundary, verifyErr := t.Lease.Verify(ctx)
	if verifyErr != nil {
		return fmt.Errorf("qualify rootfs capture: %w", verifyErr)
	}
	if err = boundary.validate(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Use the host monotonic time before sending as a conservative lease boundary to avoid acknowledging Pause after freeze expires.
	freezeDeadline := time.Now().Add(time.Duration(t.Request.LeaseMillis) * time.Millisecond)
	if err = t.record(captureFreezing); err != nil {
		return err
	}
	freezeAttempted = true
	response, freezeErr := t.Guest.FreezeAndWait(ctx, t.Request)
	err = freezeErr
	if err == nil {
		err = t.checkGuestResponse(response, true)
		if errors.Is(err, errRootfsGuestIdentity) {
			sourceUncertain = true
		}
	}
	if err != nil {
		return fmt.Errorf("freeze rootfs source: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(freezeDeadline) {
		return errors.New("freeze lease expired before pause")
	}
	if err = t.record(capturePausing); err != nil {
		return err
	}
	pauseAttempted = true
	if err = t.VM.Pause(ctx); err != nil {
		sourceUncertain = true
		return fmt.Errorf("pause rootfs source: %w", err)
	}
	if !time.Now().Before(freezeDeadline) {
		return errors.New("freeze lease expired before pause acknowledgement")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = t.record(captureFlushing); err != nil {
		return err
	}
	if err = t.Flush(); err != nil {
		return fmt.Errorf("flush rootfs source: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = t.record(captureCutting); err != nil {
		return err
	}
	if err = t.Cut(); err != nil {
		return fmt.Errorf("capture rootfs upper: %w", err)
	}
	return ctx.Err()
}

var errRootfsGuestIdentity = errors.New("guest capture identity mismatch")

// A nil return does not prove protection or cleanup; verify terminal state for the same epoch/generation.
func (t rootfsCapture) checkGuestResponse(r guestcontrol.Response, acquiring bool) error {
	if r.Protocol != guestcontrol.Protocol || r.Epoch != t.Request.Epoch || r.Generation != t.Request.Generation {
		return errRootfsGuestIdentity
	}
	if r.Error != "" || r.Failure != "" || r.CleanupPending {
		return fmt.Errorf("guest capture protection uncertain: error=%q failure=%q cleanupPending=%t", r.Error, r.Failure, r.CleanupPending)
	}
	if acquiring {
		if r.State != guestcontrol.Held || r.MaxLeaseMillis < t.Request.LeaseMillis {
			return errors.New("guest did not confirm the requested freeze lease")
		}
	} else if r.State != guestcontrol.Released && r.State != guestcontrol.Expired {
		return errors.New("guest did not confirm filesystem cleanup")
	}
	return nil
}

// Unit experiments without a durable coordinator may omit Progress; production entry points must provide it.
func (t rootfsCapture) record(phase capturePhase) error {
	if t.Progress == nil {
		return nil
	}
	if err := t.Progress.Record(phase); err != nil {
		return fmt.Errorf("persist capture %s: %w", phase, err)
	}
	return nil
}

func (t rootfsCapture) quarantine(cause error) error {
	// Production Progress and Lease share the same journal. Persist once and
	// preserve any error; only transactions without Progress use the lease.
	if t.Progress != nil {
		return t.Progress.Quarantine(cause)
	}
	return t.Lease.Quarantine(cause)
}

func (t rootfsCapture) recoverSource(ctx context.Context, freezeAttempted, pauseAttempted, sourceUncertain bool, err error) error {
	// Cleanup does not inherit a canceled request; Resume can begin only after synchronous capture work has actually returned.
	var recoveryErr error
	progressErr := t.record(captureRecovering)
	if pauseAttempted {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.RecoveryTimeout)
		recoveryErr = t.VM.Resume(recovery)
		cancel()
		if recoveryErr != nil {
			recoveryErr = fmt.Errorf("resume rootfs source: %w", recoveryErr)
		}
	}
	if freezeAttempted && recoveryErr == nil {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.RecoveryTimeout)
		response, thawErr := t.Guest.ThawAndWait(recovery, t.Request)
		recoveryErr = thawErr
		if recoveryErr == nil {
			recoveryErr = t.checkGuestResponse(response, false)
		}
		cancel()
		if recoveryErr != nil {
			recoveryErr = fmt.Errorf("thaw rootfs source: %w", recoveryErr)
		}
	}
	recoveryErr = errors.Join(recoveryErr, progressErr)
	if recoveryErr == nil && !sourceUncertain {
		recoveryErr = t.record(captureRecovered)
	}
	if recoveryErr != nil || sourceUncertain {
		if sourceUncertain {
			recoveryErr = errors.Join(recoveryErr, errors.New("source operation outcome requires reconciliation"))
		}
		err = errors.Join(err, recoveryErr)
		// Do not release the lease when recovery is uncertain or send thaw before confirming that the Guest is running.
		err = errors.Join(err, t.quarantine(err))
		return err
	}
	if releaseErr := t.Lease.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("release capture lease: %w", releaseErr))
		err = errors.Join(err, t.quarantine(err))
	}
	return err
}
