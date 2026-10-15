package main

import (
	"fmt"
	"os"
	"strings"
)

const freePageReportingReadyMarker = "SANDBOX_FREE_PAGE_REPORTING_READY"

// Reporting is configured on every new golden VM. Custom kernels without
// support remain usable, but must not advertise an active reclamation policy.
func renderFreePageReporting() string {
	return `
if [ -d /sys/module/virtio_balloon ] && [ -d /sys/module/page_reporting ]; then
  echo ` + freePageReportingReadyMarker + `
else
  echo "SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE missing_guest_kernel_support"
fi
`
}

func readFreePageReportingSupport(bootLog string) (bool, error) {
	payload, err := os.ReadFile(bootLog)
	if err != nil {
		return false, fmt.Errorf("read guest free-page reporting support: %w", err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.TrimSpace(line) == freePageReportingReadyMarker {
			return true, nil
		}
	}
	return false, nil
}
