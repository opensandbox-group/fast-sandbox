//go:build linux

package firecracker

import "syscall"

// bindMount bind-mounts source over target (the snapshot spill area into a
// jailed VMM's chroot for the dump window).
func bindMount(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

// unmountPath detaches the mount at target.
func unmountPath(target string) error {
	return syscall.Unmount(target, 0)
}
