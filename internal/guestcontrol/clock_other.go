//go:build !linux || (!amd64 && !arm64)

package guestcontrol

import (
	"errors"
	"time"
)

// setGuestClock exists only on 64-bit Linux guests; the fallback keeps
// every other platform — including 32-bit Linux — compiling.
func setGuestClock(at time.Time) error {
	return errors.New("setting the guest clock is unsupported on this platform")
}
