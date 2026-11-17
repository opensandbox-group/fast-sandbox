package firecracker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
			err := configureRootfsVM(t.Context(), c, &fastletnetwork.Slot{GuestTap: "tap0"}, m, "/kernel", rootfsDeviceName, false)
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

// Both storage layouts must preserve the drive name in vmstate so the
// corresponding Full restore can find it without source-path aliases.
func TestRootfsBootDriveContract(t *testing.T) {
	for _, name := range []string{rootfsImageName, rootfsDeviceName} {
		for _, jailed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/jailed=%t", name, jailed), func(t *testing.T) {
				var drive map[string]any
				socket := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/drives/rootfs" {
						require.NoError(t, json.NewDecoder(r.Body).Decode(&drive))
					}
					w.WriteHeader(http.StatusNoContent)
				})
				client := NewClient(socket)
				defer client.Close()
				manifest := artifacts.VMManifest{Machine: artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 256, RootfsDriveID: "rootfs"}, Kernel: artifacts.VMKernel{Cmdline: "root=/dev/vda rw"}, GuestNetwork: &artifacts.VMGuestNetwork{Iface: "eth0", MAC: "02:00:00:00:00:01", IP: "172.30.0.3", Gateway: "172.30.0.1", Netmask: "255.255.255.0", MTU: 1500}}
				require.NoError(t, configureRootfsVM(t.Context(), client, &fastletnetwork.Slot{GuestTap: "tap0"}, manifest, "/kernel", name, jailed))
				require.Equal(t, name, drive["path_on_host"])
			})
		}
	}
}

func TestRootfsNativeDirectBootKeepsFullLayout(t *testing.T) {
	directory := t.TempDir()
	disk := filepath.Join(directory, rootfsImageName)
	require.NoError(t, os.WriteFile(disk, []byte("native-rootfs"), 0600))
	driver := &Driver{}
	path, jail, socket, err := driver.prepareRootfsBootInstance("source", directory, disk, "/kernel", rootfsImageName)
	require.NoError(t, err)
	require.Equal(t, disk, path)
	require.Empty(t, jail)
	require.Equal(t, filepath.Join(directory, "api.sock"), socket)
	require.NoFileExists(t, filepath.Join(directory, rootfsDeviceName))
}

func TestNativeRootfsBootImportsTemplateCarry(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(t.Context(), ""))
	mover := &fakeTemplateCapableMover{fakeStateMover: &fakeStateMover{}}
	fixture.driver.SetStateMover(mover)
	cache := filepath.Dir(cachedManifestPath(fixture.stateRoot, fixture.sandboxSpec.Desired.Image))
	require.NoError(t, os.MkdirAll(cache, 0700))
	disk, kernel := []byte("native-disk"), []byte("kernel")
	require.NoError(t, os.WriteFile(filepath.Join(cache, rootfsImageName), disk, 0600))
	fixture.driver.config.KernelPath = filepath.Join(t.TempDir(), "kernel")
	require.NoError(t, os.WriteFile(fixture.driver.config.KernelPath, kernel, 0600))
	manifest := newMetadataTestVMManifest()
	manifest.Compatibility.CPUArch = artifacts.HostCPUArch()
	manifest.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly", StateFiles: map[string]artifacts.ManifestFile{}}
	manifest.Kernel.Cmdline = "root=/dev/vda rw"
	manifest.Kernel.SHA256 = artifacts.SHA256Of(kernel)
	manifest.Files = map[string]artifacts.ManifestFile{"rootfs.ext4": {SHA256: artifacts.SHA256Of(disk), SizeBytes: int64(len(disk))}}
	for _, name := range []string{vaultBundleName, policyBundleName} {
		content := []byte(`{"version":1}`)
		require.NoError(t, os.WriteFile(filepath.Join(cache, name), content, 0600))
		manifest.Platform.StateFiles[name] = artifacts.ManifestFile{SHA256: artifacts.SHA256Of(content), SizeBytes: int64(len(content))}
	}
	raw, err := artifacts.MarshalVMManifest(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cache, snapshotManifestName), raw, 0600))
	_, err = fixture.driver.EnsureSandbox(t.Context(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	_, imports := mover.snapshotTemplate()
	require.Len(t, imports, 2, "native cold boot must import the verified vault and policy without vmstate")
	require.NoFileExists(t, filepath.Join(cache, vmstateSnapshotName))
	require.NoFileExists(t, filepath.Join(cache, memorySnapshotName))
}
