package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
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
				code := 204
				if r.URL.Path == fail {
					code = 500
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
			})}}
			err := configureRootfsBoot(t.Context(), client, sandboxLayout{runtime: "/run/test"}, rootfsBootManifest(), noneProvider{}, "/kernel")
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
	require.NoError(t, probeRootfsHelper(ctx, filepath.Join(dir, "guest-control.sock"), 1234))
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
