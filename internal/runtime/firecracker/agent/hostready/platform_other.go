//go:build !linux

package hostready

import (
	"errors"
	"fmt"
	"os"
)

// errNotLinux marks the probes that have no meaning off the Linux nodes
// this runtime targets. The firecracker-runtime binary only executes its
// checks on the node (linux); on a development workstation the standalone
// scripts/firecracker-host-check.sh is the supported entry point.

// openDeviceRW opens a device node read-write via the portable path.
func openDeviceRW(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("device does not exist")
		}
		return err
	}
	return file.Close()
}

// statFS is unsupported off linux (the fs-type magic map is linux-only).
func statFS(string) (FsStat, error) {
	return FsStat{}, errors.New("statfs probes require linux")
}

// kernelRelease returns uname -r via the host command (best effort).
func kernelRelease() (string, error) {
	return "", errors.New("uname probes require linux")
}

// cpuFlags is unsupported off linux.
func cpuFlags() ([]string, error) {
	return nil, errors.New("cpu flag probes require linux")
}

// memAvailable is unsupported off linux.
func memAvailable() (int64, error) {
	return 0, errors.New("meminfo probes require linux")
}

// reflinkProbe is unsupported off linux (FICLONE is a linux ioctl).
func reflinkProbe(string) (bool, error) {
	return false, errors.New("reflink probes require linux")
}
