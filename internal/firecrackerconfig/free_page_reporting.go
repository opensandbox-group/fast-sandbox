package firecrackerconfig

import (
	"fmt"
	"os"
	"strings"
)

const (
	// FreePageReportingReadyMarker is emitted on the guest serial console when
	// built-in virtio_balloon and page_reporting kernel modules are present.
	FreePageReportingReadyMarker = "SANDBOX_FREE_PAGE_REPORTING_READY"

	// FreePageReportingUnavailableMarker is emitted when either kernel module
	// is missing; custom kernels remain bootable without active reporting.
	FreePageReportingUnavailableMarker = "SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE"

	// FreePageReportingUnavailableWarning is logged when the guest did not
	// advertise FPR kernel support during template build.
	FreePageReportingUnavailableWarning = "guest free-page reporting unavailable; snapshot will retain legacy memory behavior"

	pathMachineConfig = "/machine-config"
	pathBalloon       = "/balloon"
	pathEntropy       = "/entropy"
	pathBootSource    = "/boot-source"
	pathVsock         = "/vsock"
)

// BuildBootSpec describes the pre-boot device configuration for a template
// build VM shared by sandboxtemplate-builder and fsb-vm build.
type BuildBootSpec struct {
	KernelPath    string
	BootArgs      string
	VCPUCount     int
	MemoryMiB     int64
	SMT           *bool
	CPUTemplate   string
	RootfsDriveID string
	RootfsPath    string
	TapName       string
	GuestIface    string
	GuestMAC      string
	VsockUDSPath  string
}

// FreePageReportingBalloon returns the zero-target Firecracker PUT /balloon
// configuration that enables free-page reporting with stats polling disabled.
func FreePageReportingBalloon() map[string]any {
	return map[string]any{
		"amount_mib":               0,
		"deflate_on_oom":           true,
		"stats_polling_interval_s": 0,
		"free_page_reporting":      true,
	}
}

// BuildBoot returns the ordered pre-boot Firecracker PUT operations for a
// golden template build VM, including zero-target FPR balloon and virtio-rng
// entropy devices.
func BuildBoot(spec BuildBootSpec) []Operation {
	machine := map[string]any{
		"vcpu_count":   spec.VCPUCount,
		"mem_size_mib": spec.MemoryMiB,
	}
	if spec.SMT != nil {
		machine["smt"] = *spec.SMT
	}
	if spec.CPUTemplate != "" {
		machine["cpu_template"] = spec.CPUTemplate
	}
	ops := []Operation{
		{pathMachineConfig, machine},
		{pathBalloon, FreePageReportingBalloon()},
		{pathEntropy, map[string]any{}},
		{pathBootSource, map[string]any{"kernel_image_path": spec.KernelPath, "boot_args": spec.BootArgs}},
		{"/drives/" + spec.RootfsDriveID, map[string]any{
			"drive_id":       spec.RootfsDriveID,
			"path_on_host":   spec.RootfsPath,
			"is_root_device": true,
			"is_read_only":   false,
		}},
	}
	if spec.TapName != "" {
		iface := spec.GuestIface
		if iface == "" {
			iface = "eth0"
		}
		ops = append(ops, Operation{
			"/network-interfaces/" + iface,
			map[string]any{"iface_id": iface, "host_dev_name": spec.TapName, "guest_mac": spec.GuestMAC},
		})
	}
	if spec.VsockUDSPath != "" {
		ops = append(ops, Operation{pathVsock, map[string]any{"guest_cid": 3, "uds_path": spec.VsockUDSPath}})
	}
	return ops
}

// WrapBuildOpError adds consistent context for a failed BuildBoot PUT call.
func WrapBuildOpError(path string, err error) error {
	switch path {
	case pathMachineConfig:
		return fmt.Errorf("configure machine: %w", err)
	case pathBalloon:
		return fmt.Errorf("configure free-page reporting: %w", err)
	case pathEntropy:
		return fmt.Errorf("attach entropy device: %w", err)
	case pathBootSource:
		return fmt.Errorf("configure boot source: %w", err)
	default:
		if strings.HasPrefix(path, "/drives/") {
			return fmt.Errorf("attach rootfs drive: %w", err)
		}
		return err
	}
}

// RenderFreePageReportingCheck returns the guest shell snippet that probes
// /sys/module for built-in virtio_balloon and page_reporting support and emits
// the console marker read after boot.
func RenderFreePageReportingCheck() string {
	return `
if [ -d /sys/module/virtio_balloon ] && [ -d /sys/module/page_reporting ]; then
  echo ` + FreePageReportingReadyMarker + `
else
  echo "` + FreePageReportingUnavailableMarker + ` missing_guest_kernel_support"
fi
`
}

// ReadFreePageReportingSupport inspects a guest serial console log and returns
// true when FreePageReportingReadyMarker appears on its own line.
func ReadFreePageReportingSupport(bootLog string) (bool, error) {
	payload, err := os.ReadFile(bootLog)
	if err != nil {
		return false, fmt.Errorf("read guest free-page reporting support: %w", err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.TrimSpace(line) == FreePageReportingReadyMarker {
			return true, nil
		}
	}
	return false, nil
}
