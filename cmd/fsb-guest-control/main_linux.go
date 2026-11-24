//go:build linux && (amd64 || arm64)

// fsb-guest-control is an experimental Guest service. It does not inject itself into production templates or enable RootfsOnly snapshot creation.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"fast-sandbox/internal/guestcontrol"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	mount := flag.String("mount", "", "explicit mount to protect")
	port := flag.Uint("port", 0, "private vsock port, required")
	lease := flag.Duration("max-lease", 30*time.Second, "maximum protection lease")
	flag.Parse()
	if *mount == "" || *port == 0 || uint64(*port) >= uint64(unix.VMADDR_PORT_ANY) || *lease < time.Millisecond || *lease > time.Minute {
		return errors.New("mount, valid port, and max-lease in [1ms,1m] required")
	}
	if os.Geteuid() != 0 {
		return errors.New("guest control requires root")
	}
	// Keep the crash marker on tmpfs and never remove it automatically: a restarted process cannot determine whether its predecessor left the filesystem frozen.
	var st unix.Statfs_t
	if err := unix.Statfs("/run", &st); err != nil {
		return err
	}
	if st.Type != unix.TMPFS_MAGIC {
		return errors.New("/run must be tmpfs")
	}
	guard, err := os.OpenFile("/run/fsb-guest-control.started", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("guest control already started or needs recovery: %w", err)
	}
	if err = guard.Close(); err != nil {
		return err
	}
	fs, err := guestcontrol.OpenFilesystem(*mount)
	if err != nil {
		return err
	}
	// Keep the descriptor open until process exit, including while workers are still pending.
	manager, err := guestcontrol.NewManager(fs, *lease)
	if err != nil {
		return err
	}
	service, err := guestcontrol.NewService(manager)
	if err != nil {
		return err
	}
	return service.ServeVSock(uint32(*port))
}
