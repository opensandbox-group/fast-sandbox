package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/firecrackerconfig"
)

func TestDefaultGuestReportingInitWiring(t *testing.T) {
	init := renderGuestInit(apiv1alpha2.SandboxTemplateSpec{})
	require.Contains(t, init, firecrackerconfig.RenderFreePageReportingCheck(), "reporting needs no template configuration")
	require.NotContains(t, init, "damon_reclaim")
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
				"stats_polling_interval_s": float64(0), "free_page_reporting": true}, balloon)
		})
	}
}
