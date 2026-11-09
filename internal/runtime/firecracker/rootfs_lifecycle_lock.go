package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	runtimecontract "fast-sandbox/internal/runtime/contract"
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

// Ensure may retire a lost Pod's fence only after node-level cleanup has
// removed its resources. It never adopts, signals, or deletes another Pod's VM.
func (d *Driver) lockSandboxEnsure(ctx context.Context, stateRoot, id string) (func(), error) {
	release, err := lockSandboxLifecycle(ctx, stateRoot, id)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(stateRoot, sandboxStateDir, id)
	state, readErr := loadState(directory)
	if readErr == nil && d.podUID != "" && state.Config.Identity.FastletPodUID != d.podUID {
		err = d.reconcileOrphanedCapture(state, directory)
	}
	if err == nil {
		err = checkSandboxCaptureFence(stateRoot, id)
	}
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (d *Driver) reconcileOrphanedCapture(state *SandboxState, directory string) error {
	path := filepath.Join(directory, "rootfs-capture")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	journal, err := loadCaptureJournal(path)
	if err != nil {
		return err
	}
	if journal.record.Phase == captureRecovered {
		return nil
	}
	if journal.record.Owner == nil || !runtimecontract.SameRuntimeIdentity(*journal.record.Owner, state.Config.Identity) {
		return fmt.Errorf("capture fence: %w", errCaptureIdentityMismatch)
	}
	owner := state.Config.Identity
	network := state.Allocation.Network
	// Network records are shared across Pods. Janitor removes a slot record
	// only AFTER the host process cleaner confirms the old VMM is gone.
	// A Pod-local PID must never be interpreted in a replacement Pod.
	if network.SlotID == "" || filepath.Base(network.SlotID) != network.SlotID || !filepath.IsAbs(network.DNSPath) ||
		filepath.Base(network.DNSPath) != network.SlotID+".resolv.conf" || filepath.Base(filepath.Dir(network.DNSPath)) != owner.FastletPodUID {
		return errors.New("capture fence: previous Pod network cleanup identity is unavailable")
	}
	expected := snapshotStagingPath(d.config.StateRoot, owner.FastletPodUID, journal.record.Request.OperationID, true)
	if expected != journal.record.Staging {
		return errors.New("capture fence: previous Pod staging ownership mismatch")
	}
	paths := []string{
		filepath.Join(filepath.Dir(network.DNSPath), network.SlotID+".json"), network.DNSPath,
		filepath.Join(ociRootForPod(d.config.StateRoot, owner.FastletPodUID), "sandboxes", owner.SandboxUID), expected,
	}
	// fsb-vm removes the old Pod's OCI directory only after device teardown.
	// Jailer nodes and dangling rootfs links may remain; ordinary stale
	// cleanup removes those after the terminal journal and cleared PID persist.
	for _, resource := range paths {
		if _, err := os.Lstat(resource); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("capture fence: previous Pod resource cleanup pending: %s", resource), err)
		}
	}
	if journal.record.Phase != captureAborted {
		if err := journal.Record(captureAborted); err != nil {
			return err
		}
	}
	// Also repeat this after a crash between the terminal journal and meta.json.
	// Never let ordinary stale cleanup signal a PID that may have been reused.
	state.PID, state.APIAddress = 0, ""
	state.Phase = PhaseFailed
	state.FailureMessage = "previous Pod capture resources reclaimed; interrupted artifact discarded"
	return saveState(directory, state)
}
