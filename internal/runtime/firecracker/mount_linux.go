//go:build linux

package firecracker

import "syscall"

// bindMount bind-mounts source over target (the snapshot spill area into a
// jailed VMM's chroot for the dump window).
func bindMount(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

// bindMountReadOnly bind-mounts source over target read-only. The read-only
// flag needs a separate remount pass: a single MS_BIND|MS_RDONLY call does
// not apply the flag to the bind.
func bindMountReadOnly(source, target string) error {
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return err
	}
	return syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
}

// unmountPath detaches the mount at target.
func unmountPath(target string) error {
	return syscall.Unmount(target, 0)
}
