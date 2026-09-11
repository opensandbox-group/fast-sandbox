//go:build darwin

package firecracker

import "errors"

// bindMount is Linux-only (jailer mode): macOS builds exist for compilation
// and unit tests only, so this stub always fails and the caller falls back
// to the legacy staging dump.
func bindMount(_, _ string) error {
	return errors.New("bind mounts are not supported on this platform")
}

// unmountPath mirrors bindMount's platform stub.
func unmountPath(_ string) error {
	return errors.New("bind mounts are not supported on this platform")
}
