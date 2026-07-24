package infra

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

func TestProbeServiceHTTPAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
	}, nil))

	_, portValue, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portValue)
	require.NoError(t, err)
	server.Close()
	err = probeService(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeTCP, Timeout: 30 * time.Millisecond, Interval: 5 * time.Millisecond,
	}, nil)
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
		Type: infracatalog.ProbeTCP, Timeout: 25 * time.Millisecond, Interval: 100 * time.Millisecond,
	}, nil, dial, nil)
	require.Error(t, err)
	require.GreaterOrEqual(t, attempts, 4, "a configured 100ms interval must be capped instead of imposing a fixed 100ms sleep")
	require.Less(t, time.Since(started), 100*time.Millisecond)
}

func TestOptionalServiceFailureIsReturnedAsDiagnostic(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "optional-component")
	payload := []byte("optional-component")
	require.NoError(t, os.WriteFile(artifactPath, payload, 0555))
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	profile := infracatalog.Profile{
		Name: "optional-test", Version: "v1", Configured: true,
		AllowedRuntimes: []apiv1alpha1.RuntimeName{apiv1alpha1.RuntimeContainer},
		Components: []infracatalog.Component{{
			Name: "optional", Required: false,
			Artifact: infracatalog.Artifact{
				SourceType: infracatalog.SourceStatic, Reference: "file://" + artifactPath,
				Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(payload)), Executable: true,
			},
			ContainerPath: "/.fast/infra/optional",
			DeliveryModes: []runtimecatalog.InfraDeliveryMode{runtimecatalog.InfraDeliveryBindMount},
			Activation:    infracatalog.Activation{Mode: infracatalog.ActivationEntrypointSupervisor, Command: "/.fast/infra/optional", RestartPolicy: infracatalog.RestartNever},
			Services: []infracatalog.Service{{
				Name: "optional", Port: uint32(port), Transport: "http",
				Readiness: infracatalog.ReadinessProbe{Type: infracatalog.ProbeTCP, Timeout: 25 * time.Millisecond, Interval: 5 * time.Millisecond},
			}},
		}},
	}
	catalog, err := infracatalog.New([]infracatalog.Profile{profile})
	require.NoError(t, err)
	resolvedProfile, err := catalog.Resolve(profile.Name)
	require.NoError(t, err)
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	manager, err := NewManagerWithConfig(ManagerConfig{
		Catalog: catalog, RuntimeProfile: runtimeProfile, ProfileName: profile.Name,
		Store: store, Resolver: NewPlatformResolver([]string{root}), SandboxInitPath: sandboxInit,
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.SandboxSpec{
		SandboxID: "uid-optional", InstanceGeneration: 1, AssignmentAttempt: 1,
		InfraProfile: profile.Name, InfraProfileHash: resolvedProfile.ProfileHash,
	}
	_, err = manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	instance, err := manager.InitializeInstance(context.Background(), spec, "127.0.0.1")
	require.NoError(t, err, "optional component failure must not gate Sandbox readiness")
	require.Len(t, instance.Diagnostics, 1)
	require.Equal(t, "Failed", instance.Diagnostics[0].State)
	require.False(t, instance.Diagnostics[0].Required)
}

func TestInitializeServiceUsesInternalCredentialForReadiness(t *testing.T) {
	const token = "instance-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Fast-Sandbox-Infra-Token") != token {
			http.Error(writer, "missing internal credential", http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, portValue, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portValue)
	require.NoError(t, err)

	manager := &Manager{}
	require.NoError(t, manager.initializeService(context.Background(), &fastletapi.SandboxSpec{SandboxID: "uid-a"}, host, ServiceEndpoint{
		Component: "component", Name: "service", Port: uint32(port), Required: true,
		Readiness: infracatalog.ReadinessProbe{Type: infracatalog.ProbeHTTP, Path: "/", Timeout: time.Second},
	}, map[string]string{"X-Fast-Sandbox-Infra-Token": token}))
}
