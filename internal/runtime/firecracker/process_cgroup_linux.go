//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// startCommandInCgroup uses clone3(CLONE_INTO_CGROUP) through Go's
// UseCgroupFD support. The child is never observable in the Fastlet cgroup.
func startCommandInCgroup(command *exec.Cmd, cgroupPath string) error {
	if cgroupPath == "" {
		return command.Start()
	}
	cgroup, err := os.Open(cgroupPath)
	if err != nil {
		return fmt.Errorf("open target cgroup %q: %w", cgroupPath, err)
	}
	defer cgroup.Close()
	command.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    int(cgroup.Fd()),
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start process in cgroup %q: %w", cgroupPath, err)
	}
	return nil
}
