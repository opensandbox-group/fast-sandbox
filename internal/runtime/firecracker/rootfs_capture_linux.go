package firecracker

import (
	"context"
	"fmt"
	"os"

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
