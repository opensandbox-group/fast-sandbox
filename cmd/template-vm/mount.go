package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// mountReadOnly mounts the snapfiles ext4 device read-only at mountPoint
// (ADR-006). The snapfiles image is never written, so ro protects the shared
// read-only layer.
func mountReadOnly(device, mountPoint string) error {
	return mountExt4(device, mountPoint, "ro")
}

// mountReadWrite mounts an ext4 device writable (build flow: init-script
// injection into the rootfs device and population of the snapfiles device).
func mountReadWrite(device, mountPoint string) error {
	return mountExt4(device, mountPoint, "rw")
}

// mountExt4 mounts device at mountPoint with the given ext4 mount mode.
func mountExt4(device, mountPoint, mode string) error {
	if err := os.MkdirAll(mountPoint, 0o750); err != nil {
		return fmt.Errorf("create mount point %s: %w", mountPoint, err)
	}
	command := exec.Command("mount", "-t", "ext4", "-o", mode, device, mountPoint)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s at %s: %w: %s", device, mountPoint, err, output)
	}
	return nil
}

// mkfsExt4 formats device as ext4 (build flow: the blank snapfiles device).
// -F forces the format on a non-interactive block device.
func mkfsExt4(ctx context.Context, device string) error {
	command := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", device)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", device, err, output)
	}
	return nil
}

func repairExt4(ctx context.Context, device string) error {
	output, err := exec.CommandContext(ctx, "e2fsck", "-fy", device).CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("e2fsck -fy %s: %w: %s", device, err, output)
}

func resizeExt4(ctx context.Context, device string, targetGiB int) error {
	if targetGiB <= 0 {
		return fmt.Errorf("resize2fs target must be positive, got %d GiB", targetGiB)
	}
	output, err := exec.CommandContext(ctx, "resize2fs", device, fmt.Sprintf("%dG", targetGiB)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resize2fs %s to %d GiB: %w: %s", device, targetGiB, err, output)
	}
	return nil
}

func verifyExt4(ctx context.Context, device string) error {
	output, err := exec.CommandContext(ctx, "e2fsck", "-fn", device).CombinedOutput()
	if err != nil {
		return fmt.Errorf("e2fsck -fn %s: %w: %s", device, err, output)
	}
	return nil
}

func ext4SizeBytes(ctx context.Context, device string) (int64, error) {
	output, err := exec.CommandContext(ctx, "dumpe2fs", "-h", device).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("dumpe2fs -h %s: %w: %s", device, err, output)
	}
	return parseExt4Size(string(output))
}

func parseExt4Size(output string) (int64, error) {
	var blockCount, blockSize uint64
	var haveBlockCount, haveBlockSize bool
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Block count":
			parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse ext4 block count: %w", err)
			}
			blockCount, haveBlockCount = parsed, true
		case "Block size":
			parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse ext4 block size: %w", err)
			}
			blockSize, haveBlockSize = parsed, true
		}
	}
	if !haveBlockCount || !haveBlockSize || blockCount == 0 || blockSize == 0 {
		return 0, fmt.Errorf("dumpe2fs output lacks positive block count and block size")
	}
	maxInt64 := ^uint64(0) >> 1
	if blockCount > maxInt64/blockSize {
		return 0, fmt.Errorf("ext4 geometry %d blocks × %d bytes overflows int64", blockCount, blockSize)
	}
	return int64(blockCount * blockSize), nil
}

// syncFilesystems flushes all dirty pages to stable storage. It is called
// before umount in the build flow so the committed upper layer cannot lose
// freshly written snapshot files even if umount ordering changes later.
func syncFilesystems() {
	syscall.Sync()
}

// unmount detaches the filesystem at mountPoint. A not-mounted path is not an
// error (delete is best-effort and idempotent).
func unmount(mountPoint string) error {
	command := exec.Command("umount", mountPoint)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if !isMounted(mountPoint) {
		return nil
	}
	return fmt.Errorf("umount %s: %w: %s", mountPoint, err, output)
}

// isMounted reports whether mountPoint is a mount point by comparing its
// device number with its parent's.
func isMounted(mountPoint string) bool {
	var self, parent syscall.Stat_t
	if err := syscall.Stat(mountPoint, &self); err != nil {
		return false
	}
	if err := syscall.Stat(mountPoint+"/..", &parent); err != nil {
		return false
	}
	return self.Dev != parent.Dev
}

// processAlive reports whether the process with pid is still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
