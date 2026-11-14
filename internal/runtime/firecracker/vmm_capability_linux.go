//go:build linux

package firecracker

import (
	"fmt"
	"os"
)

// assertVMMCapabilities reads /proc/<pid>/status of the launched VMM and
// verifies the fixed capability drop (FSEP-0003). It runs after the API
// socket is ready, so the process is the surviving exec chain
// (jailer → helper → firecracker, one PID) and a missing status means the
// VMM died.
func assertVMMCapabilities(pid int) error {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return fmt.Errorf("read the VMM process status: %w", err)
	}
	status, err := parseVMMCapabilityStatus(string(content))
	if err != nil {
		return err
	}
	return status.verifyCapabilitiesDropped()
}
