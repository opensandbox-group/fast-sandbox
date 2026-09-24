//go:build linux

package firecracker

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// kvmStat resolves a path with the host stat(2).
func kvmStat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// rebindCharDevice replaces path with a character device bound to rdev,
// keeping the jailer's layout (owned by root, mode 0600): the launch pins
// the jailed uid/gid to 0 (launcher.go buildArgv), so no chown is needed.
// mknod requires CAP_MKNOD, which the Fastlet Pod already carries for the
// loop device provisioning.
func rebindCharDevice(path string, rdev uint64) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale jailed KVM device %s: %w", path, err)
	}
	if err := unix.Mknod(path, unix.S_IFCHR|0o600, int(rdev)); err != nil {
		return fmt.Errorf("mknod jailed KVM device %s: %w (requires CAP_MKNOD)", path, err)
	}
	return nil
}
