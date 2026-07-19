package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/boxlitewire"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

func TestBoxLiteDriverSidecarContract(t *testing.T) {
	userProcessStartedAt := time.Unix(1700000001, 123)
	credential, err := fastletnetwork.GenerateLocalForwardCredential()
	require.NoError(t, err)
	testAccess := fastletnetwork.AccessDescriptor{
		Kind: fastletnetwork.AccessKindLocalForward, Address: "127.0.0.1:21000", Credential: credential,
	}
	var mu sync.Mutex
	var ensured boxLiteEnsureRequest
	var pulled string
	deleted := false
	recovered := false
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/capabilities":
			capabilities := make(map[string]bool, len(requiredBoxLiteSidecarCapabilities))
			for _, capability := range requiredBoxLiteSidecarCapabilities {
				capabilities[capability] = true
			}
			writeBoxLiteTestJSON(t, writer, boxLiteCapabilities{ProtocolVersion: "v1", Ready: true, Capabilities: capabilities})
		case request.Method == http.MethodPut && request.URL.Path == "/v1/boxes/uid-a":
			require.NoError(t, json.NewDecoder(request.Body).Decode(&ensured))
			writeBoxLiteTestJSON(t, writer, boxLiteBox{
				Sandbox: ensured.Sandbox, BoxID: "box-a", PID: 42, Phase: "running", CreatedAt: 1700000000,
				Access: testAccess, UserProcessStartedAt: userProcessStartedAt, UserProcessStartSource: api.UserProcessStartRuntimeDirect,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/boxes/uid-a":
			writeBoxLiteTestJSON(t, writer, boxLiteBox{
				Sandbox: ensured.Sandbox, BoxID: "box-a", PID: 42, Phase: "running", CreatedAt: 1700000000,
				Access: testAccess,
			})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/boxes/uid-a":
			mu.Lock()
			recovered = true
			mu.Unlock()
			writeBoxLiteTestJSON(t, writer, boxLiteBox{
				Sandbox: ensured.Sandbox, BoxID: "box-a", PID: 42, Phase: "running", CreatedAt: 1700000000,
				Access: testAccess,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/boxes":
			require.Equal(t, "tenant-a", request.URL.Query().Get("namespace"))
			writeBoxLiteTestJSON(t, writer, boxLiteListResponse{Boxes: []boxLiteBox{{
				Sandbox: ensured.Sandbox, BoxID: "box-a", PID: 42, Phase: "running", CreatedAt: 1700000000,
				Access: testAccess,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/images":
			writeBoxLiteTestJSON(t, writer, boxLiteImagesResponse{Images: []string{"alpine:latest"}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/images/pull":
			var input boxLitePullRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&input))
			mu.Lock()
			pulled = input.Image
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/boxes/uid-a":
			mu.Lock()
			deleted = true
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	})
	driver := newBoxLiteTestDriver(t, handler)
	driver.SetNamespace("tenant-a")

	report := driver.ProbeCapabilities(context.Background())
	require.True(t, report.Ready(), report.Message)

	spec := &api.SandboxSpec{
		SandboxID: "uid-a", ClaimUID: "uid-a", ClaimNamespace: "tenant-a", ClaimName: "sandbox-a", FastletPodUID: "pod-a",
		InstanceGeneration: 1, AssignmentAttempt: 2, RouteGeneration: 3, Image: "alpine:latest",
		CPU: "1", Memory: "256Mi", PIDs: 128, RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		InfraProfile: "minimal", InfraProfileHash: "infra-hash",
	}
	metadata, err := driver.EnsureSandbox(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, "box-a", metadata.ContainerID)
	require.Equal(t, 42, metadata.PID)
	require.True(t, userProcessStartedAt.Equal(metadata.UserProcessStartedAt))
	require.Equal(t, api.UserProcessStartRuntimeDirect, metadata.UserProcessStartSource)
	require.Equal(t, uint32(19090), ensured.TunnelGuestPort)
	require.Equal(t, "tenant-a", ensured.Namespace)

	access, err := driver.GetAccessDescriptor("uid-a")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:21000", access.Address)

	inspected, err := driver.InspectSandbox(context.Background(), "uid-a")
	require.NoError(t, err)
	require.Equal(t, metadata.SandboxID, inspected.SandboxID)
	managed, err := driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.NoError(t, driver.RecoverRuntimeResources(context.Background(), managed))
	mu.Lock()
	require.True(t, recovered)
	mu.Unlock()

	images, err := driver.ListImages(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"alpine:latest"}, images)
	require.NoError(t, driver.PullImage(context.Background(), "python:3.12"))
	mu.Lock()
	require.Equal(t, "python:3.12", pulled)
	mu.Unlock()

	require.NoError(t, driver.DeleteSandbox(context.Background(), "uid-a"))
	mu.Lock()
	require.True(t, deleted)
	mu.Unlock()
	_, err = driver.GetAccessDescriptor("uid-a")
	require.ErrorIs(t, err, ErrNetworkUnavailable)
}

func TestBoxLiteDriverCapabilityAndIdentityFailClosed(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/capabilities":
			writeBoxLiteTestJSON(t, writer, boxLiteCapabilities{
				ProtocolVersion: "v1", Ready: true, Capabilities: map[string]bool{"owner-fence-v1": true},
			})
		case "/v1/boxes/uid-conflict":
			writer.WriteHeader(http.StatusConflict)
			writeBoxLiteTestJSON(t, writer, boxLiteErrorResponse{Code: boxlitewire.ErrorImmutableSpecConflict, Message: "resource hash changed"})
		default:
			http.NotFound(writer, request)
		}
	})
	driver := newBoxLiteTestDriver(t, handler)
	report := driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "BoxLiteSidecarCapabilityMissing", report.Reason)
	require.Contains(t, report.Missing, "local-forward-v1")

	_, err := driver.EnsureSandbox(context.Background(), &api.SandboxSpec{
		SandboxID: "uid-conflict", FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1,
	})
	require.ErrorIs(t, err, ErrSandboxProfileMismatch)
}

func newBoxLiteTestDriver(t *testing.T, handler http.Handler) *BoxLiteDriver {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "boxlite.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Errorf("serve BoxLite test sidecar: %v", serveErr)
		}
	}()
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeBoxLite)
	require.NoError(t, err)
	profile.BoxLite.ControlSocket = socketPath
	driver := newBoxLiteDriver(profile)
	require.NoError(t, driver.Initialize(context.Background(), ""))
	t.Cleanup(func() {
		require.NoError(t, driver.Close())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, server.Shutdown(shutdownCtx))
	})
	return driver
}

func writeBoxLiteTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(writer).Encode(value))
}
