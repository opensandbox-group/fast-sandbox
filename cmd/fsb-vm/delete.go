package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <sandbox_id>",
		Short: "Stop a microVM and release its devices and state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd.Context(), args[0])
		},
	}
}

// firecrackerStopGrace is how long Firecracker gets to exit on SIGTERM before
// it is killed.
const firecrackerStopGrace = 3 * time.Second

// runDelete tears a sandbox down in strictly reversed create order and only
// removes its directories once every resource is released. A failed step
// keeps state.json (and the writable upper), so delete can simply be re-run
// (ADR-009). It also cleans up after a create that crashed before writing a
// complete state: teardown derives every resource from the sandbox id.
func runDelete(ctx context.Context, sandboxID string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	release, err := lockSandbox(sandboxID)
	if err != nil {
		return err
	}
	defer release()

	layout := newSandboxLayout(sandboxID)
	state, err := readState(layout.home)
	if err != nil {
		if !pathExists(layout.home) && !pathExists(layout.runtime) {
			return fmt.Errorf("sandbox %q not found", sandboxID)
		}
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: ignoring unreadable state of sandbox %s: %v\n", sandboxID, err)
		}
		state = sandboxState{SandboxID: sandboxID}
	}

	ublkd := newUblkdClient(global.ublkdSocket)
	if err := releaseSandbox(ctx, ublkd, layout, &state); err != nil {
		return fmt.Errorf("delete sandbox %q is incomplete; state kept, re-run delete to retry: %w", sandboxID, err)
	}
	if err := removeSandboxDirs(layout); err != nil {
		return err
	}
	fmt.Printf("sandbox %s deleted\n", sandboxID)
	return nil
}

// deviceReleaser is the part of the ublkd client teardown needs.
type deviceReleaser interface {
	List(ctx context.Context) ([]ublkdDevice, error)
	Del(ctx context.Context, devID int) error
}

// releaseSandbox frees every resource of a sandbox, shared by delete and by
// the create rollback. Resources are identified by the sandbox layout, not by
// recorded numbers alone: Firecracker by its verified process identity (or,
// if the PID was never recorded, by its unique --api-sock argument) and the
// ublk devices by the config path ublkd lists them under. Each released
// resource is cleared from state as it goes; the returned error lists the
// steps that still hold resources.
func releaseSandbox(ctx context.Context, ublkd deviceReleaser, layout sandboxLayout, state *sandboxState) error {
	var failures []error
	persist := func() {
		// The home may already be gone (a create that crashed before its
		// first write); there is nothing to record into then.
		if !pathExists(layout.home) {
			return
		}
		if err := writeState(layout.home, state); err != nil {
			failures = append(failures, err)
		}
	}
	state.Phase = phaseDeleting
	persist()

	// The VMM holds both devices open; nothing below is safe while it runs.
	if err := stopFirecracker(state.Firecracker, layout.apiSocket); err != nil {
		return errors.Join(append(failures, fmt.Errorf("stop firecracker: %w", err))...)
	}
	state.Firecracker = nil
	persist()

	if err := os.Remove(layout.rootfsLink); err != nil && !os.IsNotExist(err) {
		failures = append(failures, fmt.Errorf("remove rootfs drive link: %w", err))
	}
	unmountErr := unmount(layout.mountPoint)
	if unmountErr != nil {
		failures = append(failures, fmt.Errorf("umount snapfiles: %w", unmountErr))
	}

	devices, err := ublkd.List(ctx)
	if err != nil {
		return errors.Join(append(failures, fmt.Errorf("list ublkd devices: %w", err))...)
	}
	if err := releaseOwnedDevices(ctx, ublkd, devices, layout.rootfsConfig); err != nil {
		failures = append(failures, fmt.Errorf("release rootfs device: %w", err))
	} else {
		state.Rootfs = nil
	}
	// The snapfiles device stays attached while its filesystem is mounted.
	if unmountErr == nil {
		if err := releaseOwnedDevices(ctx, ublkd, devices, layout.snapfilesConfig); err != nil {
			failures = append(failures, fmt.Errorf("release snapfiles device: %w", err))
		} else {
			state.Snapfiles = nil
		}
	}
	persist()
	return errors.Join(failures...)
}

// stopFirecracker terminates the sandbox's VMM. The recorded identity is
// trusted only while it still matches the live process; otherwise (never
// recorded, or stale) procfs is searched for the process serving apiSocket,
// which no other sandbox can use.
func stopFirecracker(recorded *processRecord, apiSocket string) error {
	target := recorded
	if !target.alive() {
		found, err := findProcessByArgs("--api-sock", apiSocket)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		target = found
	}
	if target == nil {
		return nil
	}
	return terminateProcess(*target, firecrackerStopGrace)
}

// releaseOwnedDevices detaches every device ublkd lists under config. A
// recorded dev_id is never deleted blindly: once released, the id can be
// reused by another sandbox's device.
func releaseOwnedDevices(ctx context.Context, ublkd deviceReleaser, devices []ublkdDevice, config string) error {
	var failures []error
	for _, device := range devices {
		if device.Config != config {
			continue
		}
		if err := ublkd.Del(ctx, device.DevID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// removeSandboxDirs deletes the runtime directory, then the home holding
// state.json, so an interrupted removal still leaves the record behind.
func removeSandboxDirs(layout sandboxLayout) error {
	if err := os.RemoveAll(layout.runtime); err != nil {
		return fmt.Errorf("remove runtime directory %s: %w", layout.runtime, err)
	}
	if err := os.RemoveAll(layout.home); err != nil {
		return fmt.Errorf("remove state directory %s: %w", layout.home, err)
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
