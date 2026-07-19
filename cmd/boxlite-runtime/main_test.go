package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"fast-sandbox/internal/boxlitesidecar"
	"fast-sandbox/internal/boxlitewire"

	"github.com/stretchr/testify/require"
)

func TestProbeCapabilitiesRequiresReadyCompleteBackend(t *testing.T) {
	ready := probeBackend{capabilities: completeProbeCapabilities(true)}
	socketPath, stop := startProbeServer(t, ready)
	defer stop()
	require.NoError(t, probeCapabilities(context.Background(), socketPath))

	notReady := completeProbeCapabilities(false)
	notReady.Reason = "ResourceLimitsUnavailable"
	socketPath, stop = startProbeServer(t, probeBackend{capabilities: notReady})
	defer stop()
	err := probeCapabilities(context.Background(), socketPath)
	require.ErrorContains(t, err, "ResourceLimitsUnavailable")

	incomplete := completeProbeCapabilities(true)
	delete(incomplete.Capabilities, boxlitewire.CapabilityRecovery)
	socketPath, stop = startProbeServer(t, probeBackend{capabilities: incomplete})
	defer stop()
	err = probeCapabilities(context.Background(), socketPath)
	require.ErrorContains(t, err, boxlitewire.CapabilityRecovery)
}

func TestListenUnixRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	require.NoError(t, os.WriteFile(path, []byte("do-not-replace"), 0600))
	_, err := listenUnix(path)
	require.ErrorContains(t, err, "refusing to replace non-socket")
}

type probeBackend struct {
	capabilities boxlitewire.Capabilities
}

func (b probeBackend) Capabilities(context.Context) boxlitewire.Capabilities {
	return b.capabilities
}
func (probeBackend) Ensure(context.Context, boxlitewire.EnsureRequest) (boxlitewire.Box, error) {
	return boxlitewire.Box{}, nil
}
func (probeBackend) Inspect(context.Context, string) (boxlitewire.Box, error) {
	return boxlitewire.Box{}, nil
}
func (probeBackend) Recover(context.Context, string) (boxlitewire.Box, error) {
	return boxlitewire.Box{}, nil
}
func (probeBackend) Delete(context.Context, string) error { return nil }
func (probeBackend) List(context.Context, string) ([]boxlitewire.Box, error) {
	return nil, nil
}
func (probeBackend) ListImages(context.Context) ([]string, error) { return nil, nil }
func (probeBackend) PullImage(context.Context, string) error      { return nil }

func completeProbeCapabilities(ready bool) boxlitewire.Capabilities {
	capabilities := make(map[string]bool, len(boxlitewire.RequiredCapabilities))
	for _, capability := range boxlitewire.RequiredCapabilities {
		capabilities[capability] = true
	}
	return boxlitewire.Capabilities{
		ProtocolVersion: boxlitewire.ProtocolVersionV1,
		Ready:           ready,
		Capabilities:    capabilities,
	}
}

func startProbeServer(t *testing.T, backend boxlitesidecar.Backend) (string, func()) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "runtime.sock")
	listener, err := listenUnix(socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: &boxlitesidecar.Server{Backend: backend}}
	go func() { _ = server.Serve(listener) }()
	return socketPath, func() {
		_ = server.Close()
		_ = listener.Close()
	}
}
