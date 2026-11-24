package firecrackerconfig

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderAndReadFreePageReportingCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modules []string
		want    bool
	}{
		{name: "supported", modules: []string{"virtio_balloon", "page_reporting"}, want: true},
		{name: "missing page reporting", modules: []string{"virtio_balloon"}},
		{name: "missing balloon", modules: []string{"page_reporting"}},
		{name: "unsupported kernel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modules := t.TempDir()
			for _, module := range tc.modules {
				require.NoError(t, os.Mkdir(filepath.Join(modules, module), 0o755))
			}
			quoted := "'" + strings.ReplaceAll(modules, "'", `'\''`) + "'"
			script := strings.ReplaceAll(RenderFreePageReportingCheck(), "/sys/module", quoted)
			command := exec.Command("sh", "-eu")
			command.Stdin = strings.NewReader(script + "\necho SANDBOX_READY\n")
			output, err := command.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Contains(t, string(output), "SANDBOX_READY", "unsupported kernels must remain bootable")
			if !tc.want {
				require.Contains(t, string(output), FreePageReportingUnavailableMarker)
			}
			logPath := filepath.Join(t.TempDir(), "boot.console.log")
			require.NoError(t, os.WriteFile(logPath, output, 0o600))
			enabled, err := ReadFreePageReportingSupport(logPath)
			require.NoError(t, err)
			require.Equal(t, tc.want, enabled)
		})
	}
}

func TestReadFreePageReportingSupport(t *testing.T) {
	for _, tc := range []struct {
		name, log string
		want      bool
	}{
		{name: "serial newlines", log: FreePageReportingReadyMarker + "\r\nSANDBOX_READY\r\n", want: true},
		{name: "missing marker", log: "SANDBOX_READY\n"},
		{name: "quoted marker", log: "diagnostic: " + FreePageReportingReadyMarker + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "serial.log")
			require.NoError(t, os.WriteFile(logPath, []byte(tc.log), 0o600))
			got, err := ReadFreePageReportingSupport(logPath)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	_, err := ReadFreePageReportingSupport(filepath.Join(t.TempDir(), "missing.log"))
	require.ErrorContains(t, err, "read guest free-page reporting support")
}

func TestBuildBootAndErrorWrapping(t *testing.T) {
	ops := BuildBoot(BuildBootSpec{
		KernelPath:    "/tmp/kernel",
		BootArgs:      "console=ttyS0",
		VCPUCount:     2,
		MemoryMiB:     1024,
		SMT:           new(false),
		CPUTemplate:   "T2",
		RootfsDriveID: "rootfs",
		RootfsPath:    "rootfs.dev",
		TapName:       "fc-build-tap",
		GuestMAC:      "02:00:00:00:00:01",
		VsockUDSPath:  "guest-control.sock",
	})
	paths := make([]string, 0, len(ops))
	for _, op := range ops {
		paths = append(paths, op.Path)
	}
	require.Equal(t, []string{
		"/machine-config",
		"/balloon",
		"/entropy",
		"/boot-source",
		"/drives/rootfs",
		"/network-interfaces/eth0",
		"/vsock",
	}, paths)
	require.Equal(t, map[string]any{
		"vcpu_count":   2,
		"mem_size_mib": int64(1024),
		"smt":          false,
		"cpu_template": "T2",
	}, ops[0].Body)
	require.Equal(t, FreePageReportingBalloon(), ops[1].Body)
	require.Equal(t, map[string]any{}, ops[2].Body)

	baseErr := errors.New("boom")
	require.ErrorContains(t, WrapBuildOpError("/balloon", baseErr), "configure free-page reporting")
	require.ErrorContains(t, WrapBuildOpError("/entropy", baseErr), "attach entropy device")
	require.ErrorContains(t, WrapBuildOpError("/drives/rootfs", baseErr), "attach rootfs drive")
}
