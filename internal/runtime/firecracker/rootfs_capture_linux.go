package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// captureRootfsUpper connects the transaction to the actual block-device flush and existing upper capture function.
// The lease must resolve and verify the device and path, rather than accepting them directly from requests, and remain valid until return.
// Staging must be private to this operation. Only a nil return permits subsequent packing and publication.
func captureRootfsUpper(ctx context.Context, tx rootfsCapture, device *os.File, ociRoot, sandboxID, staging string) error {
	tx.Flush = func() error { return flushCapturedBlock(device) }
	tx.Cut = func() error { return captureOCIUpper(ociRoot, sandboxID, staging) }
	return tx.run(ctx)
}

// flushCapturedBlock flushes an open block device; it does not drain the VMM.
// Callers must first freeze the Guest, pause vCPUs, and verify that neither the VMM nor the backend can submit writes beyond this boundary.
// Sync and Async both rely on a verified Guest filesystem completion boundary; this function neither submits nor consumes VMM queue entries.
// The descriptor must identify the source VM lease's device and remain open throughout capture.
//
// This call intentionally has no context wrapper: canceling a goroutine cannot cancel fsync.
// Callers must hold the lifecycle fence until the call returns, even if capture has timed out.
// Backend sync errors must propagate; unpatched OverlayBD versions that swallow these errors do not qualify.
func flushCapturedBlock(device *os.File) error {
	if device == nil {
		return fmt.Errorf("capture flush requires an open block device")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(device.Fd()), &stat); err != nil {
		return fmt.Errorf("stat capture device: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFBLK {
		return fmt.Errorf("capture flush requires a block device")
	}
	if err := unix.Fsync(int(device.Fd())); err != nil {
		return fmt.Errorf("flush capture block device: %w", err)
	}
	return nil
}

// captureNativeIdentity requires the independently qualified XFS file backend.
func captureNativeIdentity(file *os.File) (uint64, uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, 0, fmt.Errorf("native capture requires a regular disk image")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return 0, 0, err
	}
	if fs.Type != unix.XFS_SUPER_MAGIC {
		return 0, 0, fmt.Errorf("native capture currently requires qualified XFS reflink storage")
	}
	return uint64(stat.Dev), stat.Ino, nil
}

// Clone the held inode, never a reopened pathname. There is no full-copy fallback
// inside the protected window; the source lease remains held until ioctl returns.
func cutNativeRootfs(source *os.File, staging string) (resultErr error) {
	if _, _, err := captureNativeIdentity(source); err != nil {
		return err
	}
	target := filepath.Join(staging, publishedRootfsName)
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, out.Close()) }()
	if err := unix.IoctlFileClone(int(out.Fd()), int(source.Fd())); err != nil {
		return fmt.Errorf("native reflink cut: %w", err)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return syncCaptureDirectory(staging)
}

func captureNativeRootfs(ctx context.Context, tx rootfsCapture, source *os.File, staging string) error {
	tx.Flush = func() error {
		if _, _, err := captureNativeIdentity(source); err != nil {
			return err
		}
		return source.Sync()
	}
	tx.Cut = func() error { return cutNativeRootfs(source, staging) }
	return tx.run(ctx)
}
