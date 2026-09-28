package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
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

// runDelete tears a sandbox down in strictly reversed create order: kill
// firecracker → umount snapfiles → ublkd del both devices → remove the state
// directory. Each step reports failures but cleanup continues best-effort so
// a partially created or crashed sandbox can still be cleaned up (ADR-009).
func runDelete(ctx context.Context, sandboxID string) error {
	home := global.sandboxHome(sandboxID)
	state, err := readState(home)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sandbox %q not found", sandboxID)
		}
		return err
	}

	var failures []error
	report := func(step string, err error) {
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", step, err))
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", step, err)
		}
	}

	if err := killProcess(state.FirecrackerPID); err != nil {
		report("kill firecracker", err)
	}
	if state.MountPoint != "" {
		report("umount snapfiles", unmount(state.MountPoint))
	}

	ublkd := newUblkdClient(global.ublkdSocket)
	if state.RootfsDevPath != "" || state.RootfsDevID >= 0 {
		report("del rootfs device", ublkd.Del(ctx, state.RootfsDevID))
	}
	if state.SnapfilesDevPath != "" || state.SnapfilesDevID >= 0 {
		report("del snapfiles device", ublkd.Del(ctx, state.SnapfilesDevID))
	}

	report("remove state directory", os.RemoveAll(home))
	report("remove runtime directory", os.RemoveAll(global.sandboxRuntime(sandboxID)))

	if len(failures) > 0 {
		return fmt.Errorf("sandbox %q deleted with %d error(s)", sandboxID, len(failures))
	}
	fmt.Printf("sandbox %s deleted\n", sandboxID)
	return nil
}

// killProcess terminates a firecracker process: a SIGTERM followed by a
// SIGKILL escalation if it does not exit promptly. An already-dead process is
// not an error.
func killProcess(pid int) error {
	if pid <= 0 || !processAlive(pid) {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil && processAlive(pid) {
		return err
	}
	return nil
}
