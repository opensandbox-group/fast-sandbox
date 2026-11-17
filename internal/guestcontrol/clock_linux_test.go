//go:build linux && (amd64 || arm64)

package guestcontrol

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSetGuestClockAppliesRealtime invokes the real clock_settime, which
// moves the TEST HOST's realtime clock (CAP_SYS_TIME): opt-in, disposable
// guests only. Ordinary unit tests cover settime via the setTimeClock seam.
func TestSetGuestClockAppliesRealtime(t *testing.T) {
	if os.Getenv("FSB_TEST_GUEST_CLOCK") != "1" {
		t.Skip("opt-in: moves the host realtime clock; run only on a disposable guest (FSB_TEST_GUEST_CLOCK=1)")
	}
	// Capture the true wall clock before touching it: time.Since(start) is
	// monotonic, so start.Add(time.Since(start)) is "now" even while the
	// wall clock is shifted, and the cleanup must not trust a plain read.
	start := time.Now()
	err := setGuestClock(start.Round(0))
	if err != nil {
		requirePermissionError(t, err)
		return
	}
	t.Cleanup(func() {
		if err := setGuestClock(start.Add(time.Since(start))); err != nil {
			t.Errorf("failed to restore the host clock: %v", err)
		}
	})

	// time.Since is monotonic and cannot see a CLOCK_REALTIME adjustment,
	// so move the wall clock a known offset and require a wall-clock read
	// back to follow: a no-op success must fail loudly.
	if err = setGuestClock(start.Round(0).Add(-time.Hour)); err != nil {
		t.Fatalf("clock_settime failed: %v", err)
	}
	moved := time.Now().Round(0)
	if skew := moved.Sub(start.Round(0).Add(-time.Hour)); skew.Abs() > 5*time.Second {
		t.Fatalf("clock_settime did not move CLOCK_REALTIME: read %v, want ≈%v (skew %v)", moved, start.Round(0).Add(-time.Hour), skew)
	}
}

// requirePermissionError accepts EPERM as the expected outcome of an
// unprivileged run: the failure must come from the permission check, not a
// broken call site.
func requirePermissionError(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "operation not permitted") {
		return
	}
	t.Fatalf("unexpected clock_settime failure: %v", err)
}
