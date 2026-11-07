package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// startFakeFirecracker serves a scripted API on a Unix socket and returns the
// socket path plus the handler that recorded calls. The socket lives under a
// short directory because macOS rejects Unix socket paths over 104 bytes.
func startFakeFirecracker(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socketDir, err := os.MkdirTemp("", "fcapi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "api.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: handler}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return socketPath
}

func TestClientVersion(t *testing.T) {
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/version", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"firecracker_version":"1.15.1"}`))
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)

	version, err := client.Version(context.Background())
	require.NoError(t, err)
	require.Equal(t, "1.15.1", version)
}

func TestClientVersionRejectsMissingVersion(t *testing.T) {
	for _, body := range []string{`{}`, `{"firecracker_version":""}`, `{"version":"1.15.1"}`} {
		t.Run(body, func(t *testing.T) {
			socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			client := NewClient(socketPath)
			t.Cleanup(client.Close)
			version, err := client.Version(context.Background())
			require.ErrorContains(t, err, "missing firecracker_version")
			require.Empty(t, version)
		})
	}
}

func TestClientLifecycleCalls(t *testing.T) {
	var calls []string
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/machine-config", "/boot-source", "/drives/root", "/network-interfaces/eth0", "/actions":
			w.WriteHeader(http.StatusNoContent)
		case "/":
			_, _ = w.Write([]byte(`{"state":"Running"}`))
		default:
			http.NotFound(w, r)
		}
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)
	ctx := context.Background()

	require.NoError(t, client.ConfigureMachine(ctx, MachineConfigRequest{VCPUs: 2, MemSizeMiB: 512}))
	require.NoError(t, client.ConfigureBootSource(ctx, BootSourceRequest{KernelImagePath: "/opt/firecracker/vmlinux.bin", BootArgs: "console=ttyS0"}))
	require.NoError(t, client.AttachDrive(ctx, DriveRequest{DriveID: "root", PathOnHost: "/rootfs.img", IsRootDevice: true, IsReadOnly: false}))
	require.NoError(t, client.AttachNetworkInterface(ctx, NetworkInterfaceRequest{IfaceID: "eth0", HostDevName: "fc-tap", GuestMAC: "02:00:00:00:00:01"}))
	require.NoError(t, client.Start(ctx))
	state, err := client.VMState(ctx)
	require.NoError(t, err)
	require.Equal(t, "Running", state)

	require.Equal(t, []string{
		"PUT /machine-config", "PUT /boot-source", "PUT /drives/root", "PUT /network-interfaces/eth0",
		"PUT /actions", "GET /",
	}, calls)
}

func TestClientMapsErrors(t *testing.T) {
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid machine configuration"}`))
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)

	err := client.ConfigureMachine(context.Background(), MachineConfigRequest{VCPUs: 0})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid machine configuration")
}

func TestClientLoadSnapshot(t *testing.T) {
	var received SnapshotLoadRequest
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/snapshot/load", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusNoContent)
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)

	err := client.LoadSnapshot(context.Background(), SnapshotLoadRequest{
		SnapshotPath: "/cache/images/abc/vmstate.snap",
		MemBackend:   SnapshotMemBackend{BackendType: "File", BackendPath: "/cache/images/abc/memory.snap"},
		ResumeVM:     false,
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotLoadRequest{
		SnapshotPath: "/cache/images/abc/vmstate.snap",
		MemBackend:   SnapshotMemBackend{BackendType: "File", BackendPath: "/cache/images/abc/memory.snap"},
		ResumeVM:     false,
	}, received)
}

func TestClientPauseAndCreateSnapshot(t *testing.T) {
	var calls []string
	var createRequest SnapshotCreateRequest
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/vm":
			require.Equal(t, http.MethodPatch, r.Method)
			var payload map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			require.Equal(t, "Paused", payload["state"])
			w.WriteHeader(http.StatusNoContent)
		case "/snapshot/create":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&createRequest))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)
	ctx := context.Background()

	require.NoError(t, client.Pause(ctx))
	require.NoError(t, client.CreateSnapshot(ctx, SnapshotCreateRequest{
		SnapshotType: "Full", SnapshotPath: "/tmp/vmstate.snap", MemFilePath: "/tmp/memory.snap",
	}))
	require.Equal(t, []string{"PATCH /vm", "PUT /snapshot/create"}, calls)
	require.Equal(t, "Full", createRequest.SnapshotType)
	require.Equal(t, "/tmp/vmstate.snap", createRequest.SnapshotPath)
	require.Equal(t, "/tmp/memory.snap", createRequest.MemFilePath)
}

func TestClientResumeAfterLoad(t *testing.T) {
	var calls []string
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		require.Equal(t, http.MethodPatch, r.Method)
		require.Equal(t, "/vm", r.URL.Path)
		var payload map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "Resumed", payload["state"])
		w.WriteHeader(http.StatusNoContent)
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)

	require.NoError(t, client.Resume(context.Background()))
	require.Equal(t, []string{"PATCH /vm"}, calls)
}

func TestClientNotReady(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "missing.sock")
	client := NewClient(socketPath)
	t.Cleanup(client.Close)

	_, err := client.Version(context.Background())
	require.Error(t, err)
	_, err = os.Stat(socketPath)
	require.Error(t, err)
}

func TestPathEscape(t *testing.T) {
	require.Equal(t, "root", pathEscape("root"))
	require.Equal(t, "a%2Fb", pathEscape("a/b"))
}

func TestClientConfigureVSock(t *testing.T) {
	var received VSockRequest
	calls := 0
	socketPath := startFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/vsock", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusNoContent)
	})
	client := NewClient(socketPath)
	t.Cleanup(client.Close)
	for _, cid := range []uint32{0, 1, 2, ^uint32(0)} {
		require.Error(t, client.ConfigureVSock(context.Background(), VSockRequest{GuestCID: cid, UDSPath: "control.sock"}))
	}
	require.Error(t, client.ConfigureVSock(context.Background(), VSockRequest{GuestCID: 3}))
	require.NoError(t, client.ConfigureVSock(context.Background(), VSockRequest{GuestCID: 3, UDSPath: "control.sock"}))
	require.Equal(t, 1, calls)
	require.Equal(t, VSockRequest{GuestCID: 3, UDSPath: "control.sock"}, received)
}
