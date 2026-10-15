package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestDefaultGuestReportingSupport(t *testing.T) {
	for _, test := range []struct {
		name    string
		modules []string
		want    bool
	}{
		{name: "supported", modules: []string{"virtio_balloon", "page_reporting"}, want: true},
		{name: "missing page reporting", modules: []string{"virtio_balloon"}},
		{name: "missing balloon", modules: []string{"page_reporting"}},
		{name: "unsupported kernel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			modules := t.TempDir()
			for _, module := range test.modules {
				require.NoError(t, os.Mkdir(filepath.Join(modules, module), 0o755))
			}
			script := strings.ReplaceAll(renderFreePageReporting(), "/sys/module", shellQuote(modules))
			command := exec.Command("sh", "-eu")
			command.Stdin = strings.NewReader(script + "\necho SANDBOX_READY\n")
			output, err := command.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Contains(t, string(output), "SANDBOX_READY", "unsupported kernels must remain bootable")
			if !test.want {
				require.Contains(t, string(output), "SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE")
			}
			log := filepath.Join(t.TempDir(), "boot.console.log")
			require.NoError(t, os.WriteFile(log, output, 0o600))
			enabled, err := readFreePageReportingSupport(log)
			require.NoError(t, err)
			require.Equal(t, test.want, enabled)
		})
	}
	init := renderGuestInit(apiv1alpha2.SandboxTemplateSpec{})
	require.Contains(t, init, renderFreePageReporting(), "reporting needs no template configuration")
	require.NotContains(t, init, "damon_reclaim")
}

func TestReadFreePageReportingSupport(t *testing.T) {
	for _, test := range []struct {
		name, log string
		want      bool
	}{
		{name: "serial newlines", log: freePageReportingReadyMarker + "\r\nSANDBOX_READY\r\n", want: true},
		{name: "missing marker", log: "SANDBOX_READY\n"},
		{name: "quoted marker", log: "diagnostic: " + freePageReportingReadyMarker + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "boot.console.log")
			require.NoError(t, os.WriteFile(log, []byte(test.log), 0o600))
			enabled, err := readFreePageReportingSupport(log)
			require.NoError(t, err)
			require.Equal(t, test.want, enabled)
		})
	}
	_, err := readFreePageReportingSupport(filepath.Join(t.TempDir(), "missing.log"))
	require.ErrorContains(t, err, "read guest free-page reporting support")
}

func TestFreePageReportingDeviceConfiguration(t *testing.T) {
	for _, test := range []struct {
		name      string
		rejectAPI bool
	}{
		{name: "enabled by default"},
		{name: "unsupported VMM", rejectAPI: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "fpr-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "api.sock")
			listener, err := net.Listen("unix", socket)
			require.NoError(t, err)
			var mu sync.Mutex
			var paths []string
			var balloon map[string]any
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/balloon" {
					_ = json.NewDecoder(r.Body).Decode(&balloon)
					if test.rejectAPI {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			spec := apiv1alpha2.SandboxTemplateSpec{Machine: apiv1alpha2.MachineSpec{VCPU: "1", Memory: "1Gi"}}
			err = configureVM(&vmm{socket: socket}, "kernel", "rootfs", "args", spec, "")
			mu.Lock()
			defer mu.Unlock()
			if test.rejectAPI {
				require.ErrorContains(t, err, "configure free-page reporting")
				require.Equal(t, []string{"/machine-config", "/balloon"}, paths)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "/balloon", paths[1], "device must be installed before boot")
			require.Equal(t, map[string]any{"amount_mib": float64(0), "deflate_on_oom": true,
				"stats_polling_interval_s": float64(1), "free_page_reporting": true}, balloon)
		})
	}
}
