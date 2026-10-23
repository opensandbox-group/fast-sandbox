package infra

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestProbeServiceHTTPAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Empty(t, request.Header.Get("X-EXECD-ACCESS-TOKEN"))
		if request.URL.Path == "/health" {
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	require.NoError(t, probeService(context.Background(), parsed.Host, infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeHTTP, Path: "/health", Timeout: time.Second,
	}))

	_, portValue, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portValue)
	require.NoError(t, err)
	server.Close()
	err = probeService(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeTCP, Timeout: 30 * time.Millisecond,
	})
	require.Error(t, err)
}

func TestReadinessProbeUsesFastExponentialBackoff(t *testing.T) {
	attempts := 0
	dial := func(context.Context, uint32) (net.Conn, error) {
		attempts++
		return nil, errors.New("not ready")
	}
	started := time.Now()
	err := probeServiceWithDialer(context.Background(), 44772, infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeTCP, Timeout: 25 * time.Millisecond,
	}, dial, nil)
	require.Error(t, err)
	require.GreaterOrEqual(t, attempts, 4)
	require.Less(t, time.Since(started), 100*time.Millisecond)
}

func TestHostProcessServiceIsProbedOnPodLoopbackNotSandboxAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	port := uint32(listener.Addr().(*net.TCPAddr).Port)

	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	component := apiv1alpha2.InfraComponent{
		Name:     "egress",
		Delivery: apiv1alpha2.InfraDeliveryHostProcess,
		Process: apiv1alpha2.InfraProcess{
			Command:     []string{"/bin/egress"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{TCPConnect: &apiv1alpha2.InfraTCPConnect{}, TimeoutSeconds: 5},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: int32(port)},
	}
	plan, err := infracatalog.Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)
	manager, err := NewManagerWithConfig(ManagerConfig{
		Plan: plan, RuntimeProfile: runtimeProfile, Store: store, Resolver: &testResolver{},
		SandboxInitPath: filepath.Join(root, "sandbox-init"),
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.RuntimeSandboxConfig{
		Desired:  fastletapi.SandboxDesiredSpec{InfraRevision: manager.Revision()},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	_, err = manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)

	sandboxDialCalls := 0
	instance, err := manager.InitializeInstanceWithDialer(context.Background(), spec, func(context.Context, uint32) (net.Conn, error) {
		sandboxDialCalls++
		return nil, errors.New("sandbox dial must not be used for host-process services")
	})
	require.NoError(t, err)
	require.Equal(t, 0, sandboxDialCalls, "host-process readiness must probe Pod loopback, never the Sandbox address")
	require.Len(t, instance.Diagnostics, 1)
	require.Equal(t, "Ready", instance.Diagnostics[0].State)
}

func TestComponentReadinessFailureIsReportedAndFailsDataPlaneReady(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeContainer)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.RuntimeSandboxConfig{
		Desired:  fastletapi.SandboxDesiredSpec{InfraRevision: manager.Revision()},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	_, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	instance, err := manager.InitializeInstanceWithDialer(context.Background(), spec, func(context.Context, uint32) (net.Conn, error) {
		return nil, errors.New("not listening")
	})
	require.Error(t, err)
	require.Len(t, instance.Diagnostics, 1)
	require.Equal(t, "Failed", instance.Diagnostics[0].State)
}
