//go:build !linux

package firecracker

import (
	"fmt"
	"os/exec"
)

func startCommandInCgroup(command *exec.Cmd, cgroupPath string) error {
	if cgroupPath != "" {
		return fmt.Errorf("cgroup process placement is only supported on Linux")
	}
	return command.Start()
}
