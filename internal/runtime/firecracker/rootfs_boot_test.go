package firecracker

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

func TestRootfsDriverBootAPIsAndFailure(t *testing.T) {
	want := []string{"/boot-source", "/machine-config", "/drives/rootfs", "/network-interfaces/eth0", "/vsock", "/entropy"}
	for failure := -1; failure < len(want); failure++ {
		t.Run(string(rune('A'+failure+1)), func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			var machine map[string]any
			var vsock map[string]any
			socket := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.URL.Path)
				if r.URL.Path == "/vsock" {
					_ = json.NewDecoder(r.Body).Decode(&vsock)
				}
				if r.URL.Path == "/machine-config" {
					_ = json.NewDecoder(r.Body).Decode(&machine)
				}
				if failure >= 0 && r.URL.Path == want[failure] {
					w.WriteHeader(500)
				} else {
					w.WriteHeader(204)
				}
			})
			c := NewClient(socket)
			defer c.Close()
			m := artifacts.VMManifest{Machine: artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 256, RootfsDriveID: "rootfs"}, Kernel: artifacts.VMKernel{Version: "6.1", Cmdline: "root=/dev/vda rw"}, GuestNetwork: &artifacts.VMGuestNetwork{Iface: "eth0", MAC: "02:00:00:00:00:01", IP: "172.30.0.3", Gateway: "172.30.0.1", Netmask: "255.255.255.0", MTU: 1500}, GuestControl: &artifacts.VMGuestControl{Port: 1234}}
			err := configureRootfsVM(t.Context(), c, &fastletnetwork.Slot{GuestTap: "tap0"}, m, "/kernel", false)
			if failure < 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if failure < 0 {
				require.Equal(t, want, calls)
			} else {
				require.Equal(t, want[:failure+1], calls)
			}
			if vsock != nil {
				require.Equal(t, "guest-control.sock", vsock["uds_path"])
			}
			require.NotContains(t, machine, "cpu_template")
		})
	}
}
