//go:build linux && (amd64 || arm64)

package guestcontrol

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// setGuestClock applies the reading via clock_settime(CLOCK_REALTIME); this
// is how a resumed RTC-less guest learns the wall time it missed.
func setGuestClock(at time.Time) error {
	timespec := unix.Timespec{Sec: at.Unix(), Nsec: int64(at.Nanosecond())}
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &timespec); err != nil {
		return fmt.Errorf("clock_settime CLOCK_REALTIME: %w", err)
	}
	return nil
}
