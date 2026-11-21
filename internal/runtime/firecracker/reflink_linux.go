//go:build linux

package firecracker

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// reflinkOnly attempts a metadata-only CoW clone of source to target via the
// Linux FICLONE ioctl, avoiding a per-create subprocess fork/exec. It never
// falls back to a full copy: callers that need consistency (e.g. the snapshot
// rootfs clone) decide where a non-atomic full copy may run.
func reflinkOnly(source, target string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reflink source %s is not a regular file", source)
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		_ = dst.Close()
		_ = os.Remove(target)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}
