package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
)

type rootfsRoundTrip func(*http.Request) (*http.Response, error)

func (f rootfsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func rootfsBootManifest() artifacts.VMManifest {
	m := testVMManifest()
	m.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly"}
	m.Kernel.Cmdline = "console=ttyS0 root=/dev/vda rw init=/init"
	m.GuestControl = &artifacts.VMGuestControl{Protocol: "fsb-freeze-v1", Transport: "vsock", Port: 1234, HelperVersion: "1"}
	return m
}
func TestRootfsBootNeverLoadsMemory(t *testing.T) {
	for _, fail := range []string{"", "/boot-source", "/machine-config", "/drives/rootfs", "/vsock", "/entropy", "/actions"} {
		t.Run("failure="+fail, func(t *testing.T) {
			var calls []string
			client := &firecrackerClient{httpClient: &http.Client{Transport: rootfsRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.Path)
				if r.URL.Path == "/vsock" {
					var config map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&config))
					require.Equal(t, "guest-control.sock", config["uds_path"])
				}
				code := 204
				if r.URL.Path == fail {
					code = 500
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
			})}}
			err := configureRootfsBoot(t.Context(), client, sandboxLayout{runtime: "/" + strings.Repeat("r", 89)}, rootfsBootManifest(), noneProvider{}, "/kernel")
			want := []string{"/boot-source", "/machine-config", "/drives/rootfs", "/vsock", "/entropy", "/actions"}
			if fail != "" {
				require.Error(t, err)
				for i, p := range want {
					if p == fail {
						want = want[:i+1]
						break
					}
				}
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, want, calls)
		})
	}
}

// The external lab provides a dedicated, started but unconfigured VMM and a rootfs.dev link; this test neither creates nor destroys the VM.
func TestRootfsColdBootIntegration(t *testing.T) {
	if os.Getenv("FSB_TEST_ROOTFS_BOOT") != "1" {
		t.Skip("requires disposable unconfigured VMM")
	}
	dir := os.Getenv("FSB_TEST_BOOT_RUNTIME")
	require.NotEmpty(t, dir)
	kernel := os.Getenv("FSB_TEST_BOOT_KERNEL")
	require.NotEmpty(t, kernel)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	client := newFirecrackerClient(filepath.Join(dir, "api.sock"))
	defer client.close()
	require.NoError(t, client.waitReady(ctx))
	version, err := client.version(ctx)
	require.NoError(t, err)
	require.Equal(t, "1.16.1", version)
	m := rootfsBootManifest()
	m.Machine.MemoryMiB = 256
	m.Machine.VCPUCount = 2
	m.Machine.RootfsSizeMiB = 1024
	layout := sandboxLayout{runtime: dir}
	require.NoError(t, configureRootfsBoot(ctx, client, layout, m, noneProvider{}, kernel))
	bootCount := os.Getenv("FSB_TEST_BOOT_COUNT")
	if bootCount == "" {
		bootCount = "2"
	}
	require.NoError(t, waitSerialFor(filepath.Join(dir, "console.log"), "FSB_ROOT_BOOT "+bootCount, 20*time.Second))
	versionID, err := probeRootfsHelper(ctx, filepath.Join(dir, "guest-control.sock"), 1234)
	require.NoError(t, err)
	require.NotEmpty(t, versionID)
	t.Log(fmt.Sprintf("cold boot completed via rootfsOnly API path; Firecracker %s", version))
}

func TestFirecrackerVersionField(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{{`{"firecracker_version":"1.16.1"}`, true}, {`{"version":"1.16.1"}`, false}, {`{}`, false}} {
		client := &firecrackerClient{httpClient: &http.Client{Transport: rootfsRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
		})}}
		got, err := client.version(t.Context())
		if tc.ok {
			require.NoError(t, err)
			require.Equal(t, "1.16.1", got)
		} else {
			require.Error(t, err)
		}
	}
}

// Exercise real VMM socket binding and Full restore without the OCI publisher.
func TestRootfsSocketFullRestoreIntegration(t *testing.T) {
	if os.Getenv("FSB_TEST_SOCKET_RESTORE") != "1" {
		t.Skip("requires Linux KVM and FC_BINARY/FC_KERNEL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "fsb-sock-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// The API pathname fits, while an absolute control pathname would not.
	source := filepath.Join(root, strings.Repeat("s", 90-len(root)-1))
	clone := filepath.Join(root, "clone")
	for _, dir := range []string{source, clone} {
		require.NoError(t, os.MkdirAll(dir, 0700))
		disk, e := os.Create(filepath.Join(dir, rootfsDriveLink))
		require.NoError(t, e)
		require.NoError(t, disk.Truncate(16<<20))
		require.NoError(t, disk.Close())
	}
	launch := func(dir string) *firecrackerClient {
		c := exec.CommandContext(ctx, os.Getenv("FC_BINARY"), "--api-sock", filepath.Join(dir, "api.sock"))
		c.Dir = dir
		require.NoError(t, c.Start())
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		client := newFirecrackerClient(filepath.Join(dir, "api.sock"))
		t.Cleanup(client.close)
		require.NoError(t, client.waitReady(ctx))
		return client
	}
	a := launch(source)
	m := rootfsBootManifest()
	m.Machine.MemoryMiB = 128
	m.Machine.VCPUCount = 1
	// No application boot is asserted: this test isolates snapshot device paths.
	require.NoError(t, configureRootfsBoot(ctx, a, sandboxLayout{runtime: source}, m, noneProvider{}, os.Getenv("FC_KERNEL")))
	require.NoError(t, a.pause(ctx))
	vmstate, memory := filepath.Join(root, "vmstate"), filepath.Join(root, "memory")
	require.NoError(t, a.createSnapshot(ctx, snapshotCreateRequest{SnapshotType: "Full", SnapshotPath: vmstate, MemFilePath: memory}))
	require.NoError(t, a.resume(ctx))
	b := launch(clone)
	require.NoError(t, b.loadSnapshot(ctx, snapshotLoadRequest{SnapshotPath: vmstate, MemBackend: snapshotMemBackend{BackendType: "File", BackendPath: memory}}))
	require.NoError(t, b.resume(ctx))
	for _, dir := range []string{source, clone} {
		info, e := os.Stat(filepath.Join(dir, "guest-control.sock"))
		require.NoError(t, e)
		require.NotZero(t, info.Mode()&os.ModeSocket)
	}
}
