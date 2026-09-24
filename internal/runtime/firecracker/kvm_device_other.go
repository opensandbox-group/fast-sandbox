//go:build !linux

package firecracker

import (
	"fmt"
	"os"
)

// kvmStat resolves a path with the host stat(2).
func kvmStat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// rebindCharDevice is Linux-only (jailer mode): macOS builds exist for
// compilation and unit tests only, so this stub always fails.
func rebindCharDevice(path string, _ uint64) error {
	return fmt.Errorf("rebinding the jailed KVM device %s is not supported on this platform", path)
}
