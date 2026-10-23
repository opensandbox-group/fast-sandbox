package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

var agentSocketCounter int64

// testAgentSocketPath returns a short Unix socket path (sun_path is
// bounded) under the OS temp root.
func testAgentSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), "fast-sandbox-driver-"+strconv.Itoa(int(atomic.AddInt64(&agentSocketCounter, 1)))+".sock")
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// fakeAgentServer speaks the runtime-agent protocol over a Unix socket and
// records the decoded requests.
type fakeAgentServer struct {
	mu        sync.Mutex
	socket    string
	requests  []string
	responses map[string]func(w http.ResponseWriter, payload []byte)
}

func newFakeAgentServer(t *testing.T) *fakeAgentServer {
	t.Helper()
	server := &fakeAgentServer{socket: testAgentSocketPath(t), responses: make(map[string]func(w http.ResponseWriter, payload []byte))}
	listener, err := net.Listen("unix", server.socket)
	require.NoError(t, err)
	httpServer := &http.Server{Handler: server}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return server
}

func (s *fakeAgentServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	payload, _ := io.ReadAll(request.Body)
	s.mu.Lock()
	s.requests = append(s.requests, request.URL.Path+" "+string(payload))
	handler := s.responses[request.URL.Path]
	s.mu.Unlock()
	if handler != nil {
		handler(writer, payload)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *fakeAgentServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func TestAgentClientPinImageRequestAndResponse(t *testing.T) {
	server := newFakeAgentServer(t)
	server.responses[agentprotocol.RoutePinImage] = func(writer http.ResponseWriter, _ []byte) {
		_ = json.NewEncoder(writer).Encode(agentprotocol.PinImageResponse{ManifestDigest: "sha256:abc", Ready: true})
	}
	client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
	require.NoError(t, err)

	digest, err := client.PinImage(context.Background(), "req-1", "registry.example.com/sandbox:v1")
	require.NoError(t, err)
	require.Equal(t, "sha256:abc", digest)

	recorded := server.recorded()
	require.Len(t, recorded, 1)
	var request agentprotocol.PinImageRequest
	require.NoError(t, json.Unmarshal([]byte(recorded[0][len(agentprotocol.RoutePinImage)+1:]), &request))
	require.Equal(t, "req-1", request.RequestID)
	require.Equal(t, "pod-1", request.PodUID)
	require.Equal(t, "tenant-a", request.Namespace)
	require.Equal(t, "registry.example.com/sandbox:v1", request.Image)
}

func TestAgentClientNotFoundMapsToImageNotReady(t *testing.T) {
	server := newFakeAgentServer(t)
	server.responses[agentprotocol.RoutePinImage] = func(writer http.ResponseWriter, _ []byte) {
		writer.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(writer).Encode(agentprotocol.ErrorResponse{Code: agentprotocol.ErrorNotFound, Message: `image "x" not ready`})
	}
	client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
	require.NoError(t, err)
	_, err = client.PinImage(context.Background(), "req-1", "x")
	require.ErrorIs(t, err, runtimecontract.ErrImageNotReady)
}

func TestAgentClientAuthFailuresMapToInvalidConfig(t *testing.T) {
	cases := []struct {
		status int
		code   agentprotocol.ErrorCode
	}{
		{http.StatusForbidden, agentprotocol.ErrorUnauthorized},
		{http.StatusConflict, agentprotocol.ErrorConflict},
	}
	for _, test := range cases {
		server := newFakeAgentServer(t)
		server.responses[agentprotocol.RouteUnpinImage] = func(writer http.ResponseWriter, _ []byte) {
			writer.WriteHeader(test.status)
			_ = json.NewEncoder(writer).Encode(agentprotocol.ErrorResponse{Code: test.code, Message: "ownership mismatch"})
		}
		client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
		require.NoError(t, err)
		err = client.UnpinImage(context.Background(), "req-1", "x")
		require.ErrorIs(t, err, runtimecontract.ErrInvalidConfig)
	}
}

func TestAgentClientUnreachableSocket(t *testing.T) {
	client, err := NewAgentClient(testAgentSocketPath(t), "tenant-a", "pod-1")
	require.NoError(t, err)
	err = client.Health(context.Background())
	require.ErrorIs(t, err, errAgentUnreachable)
}

func TestAgentClientHealthNotOK(t *testing.T) {
	server := newFakeAgentServer(t)
	server.responses[agentprotocol.RouteHealth] = func(writer http.ResponseWriter, _ []byte) {
		_ = json.NewEncoder(writer).Encode(agentprotocol.HealthResponse{OK: false})
	}
	client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
	require.NoError(t, err)
	err = client.Health(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errAgentUnreachable)
}

func TestAgentClientLeaseDevicesBuildsRequestFromSpec(t *testing.T) {
	server := newFakeAgentServer(t)
	server.responses[agentprotocol.RouteLeaseDevices] = func(writer http.ResponseWriter, _ []byte) {
		_ = json.NewEncoder(writer).Encode(agentprotocol.LeaseDevicesResponse{
			LeaseID: "lease-1", RootfsDev: "/cache/rootfs.img", MemDev: "/cache/memory.snap",
			ManifestDigest: "sha256:abc",
		})
	}
	client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
	require.NoError(t, err)

	lease, err := client.LeaseDevices(context.Background(), "req-1", &fastletapi.RuntimeSandboxConfig{
		Desired: fastletapi.SandboxDesiredSpec{Image: "img"}, Limits: fastletapi.SandboxRuntimeLimits{Memory: "2Gi"},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "sandbox-1"},
	})
	require.NoError(t, err)
	require.Equal(t, "lease-1", lease.LeaseID)
	require.Equal(t, "/cache/rootfs.img", lease.RootfsDev)

	recorded := server.recorded()
	require.Len(t, recorded, 1)
	var request agentprotocol.LeaseDevicesRequest
	require.NoError(t, json.Unmarshal([]byte(recorded[0][len(agentprotocol.RouteLeaseDevices)+1:]), &request))
	require.Equal(t, "sandbox-1", request.SandboxID)
	require.Equal(t, "img", request.Image)
	require.Equal(t, 2048, request.MemSizeMiB)

	_, err = client.LeaseDevices(context.Background(), "req-2", nil)
	require.ErrorIs(t, err, runtimecontract.ErrInvalidConfig)
}

func TestAgentClientListLeasesAndCompatibility(t *testing.T) {
	server := newFakeAgentServer(t)
	server.responses[agentprotocol.RouteListLeases] = func(writer http.ResponseWriter, _ []byte) {
		_ = json.NewEncoder(writer).Encode(agentprotocol.ListLeasesResponse{Leases: []agentprotocol.Lease{{
			LeaseID: "lease-1", SandboxID: "sandbox-1", Image: "img", PodUID: "pod-1", RootfsDev: "/r",
		}}})
	}
	server.responses[agentprotocol.RouteCompatibility] = func(writer http.ResponseWriter, _ []byte) {
		_ = json.NewEncoder(writer).Encode(agentprotocol.CompatibilityResponse{CompatibilityClass: "native-stage-1"})
	}
	client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
	require.NoError(t, err)

	leases, err := client.ListLeases(context.Background())
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, "lease-1", leases[0].LeaseID)

	compatibility, err := client.Compatibility(context.Background())
	require.NoError(t, err)
	require.Equal(t, "native-stage-1", compatibility)
}

func TestAgentClientTimeoutBounded(t *testing.T) {
	server := newFakeAgentServer(t)
	server.responses[agentprotocol.RouteHealth] = func(writer http.ResponseWriter, _ []byte) {
		time.Sleep(5 * time.Second)
		writer.WriteHeader(http.StatusNoContent)
	}
	client, err := NewAgentClient(server.socket, "tenant-a", "pod-1")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = client.Health(ctx)
	require.Error(t, err)
	require.Less(t, time.Since(started), 2*time.Second)
}
