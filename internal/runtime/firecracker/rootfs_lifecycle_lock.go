package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Keep the lifecycle lock outside the Sandbox directory and never delete its inode, avoiding duplicate locks after delete/replace.
// It provides mutual exclusion only, not durable journaling, runtime identity verification, or dead-process resource reconciliation.
func lockSandboxLifecycle(ctx context.Context, stateRoot, id string) (func(), error) {
	if err := validateSandboxID(id); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(stateRoot) {
		return nil, errors.New("lifecycle lock requires absolute state root")
	}
	directory := filepath.Join(stateRoot, "lifecycle-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(directory, id+".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "lifecycle-lock")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("lifecycle lock must be a regular file")
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("lock sandbox lifecycle: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Unreconciled capture intent blocks create/replace/delete, Full capture, and generic Resume.
// The reconciler must hold the same underlying lock and confirm source recovery before lifting this gate.
func lockSandboxMutation(ctx context.Context, stateRoot, id string) (func(), error) {
	release, err := lockSandboxLifecycle(ctx, stateRoot, id)
	if err != nil {
		return nil, err
	}
	if err := checkSandboxCaptureFence(stateRoot, id); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func checkSandboxCaptureFence(stateRoot, id string) error {
	directory := filepath.Join(stateRoot, sandboxStateDir, id, "rootfs-capture")
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	journal, err := loadCaptureJournal(directory)
	if err == nil && journal.record.Phase != captureRecovered && journal.record.Phase != captureAborted {
		err = fmt.Errorf("rootfs capture requires reconciliation (%s)", journal.record.Phase)
	}
	if err != nil {
		return fmt.Errorf("sandbox %s capture fence: %w", id, err)
	}
	return nil
}
