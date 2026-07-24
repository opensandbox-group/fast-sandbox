package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
	boxliteserver "fast-sandbox/internal/runtime/boxlite/server"

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
	delete(incomplete.Capabilities, boxliteprotocol.CapabilityRecovery)
	socketPath, stop = startProbeServer(t, probeBackend{capabilities: incomplete})
	defer stop()
	err = probeCapabilities(context.Background(), socketPath)
	require.ErrorContains(t, err, boxliteprotocol.CapabilityRecovery)
}

func TestListenUnixRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	require.NoError(t, os.WriteFile(path, []byte("do-not-replace"), 0600))
	_, err := listenUnix(path)
	require.ErrorContains(t, err, "refusing to replace non-socket")
}

type probeBackend struct {
	capabilities boxliteprotocol.Capabilities
}

func (b probeBackend) Capabilities(context.Context) boxliteprotocol.Capabilities {
	return b.capabilities
}
func (probeBackend) Ensure(context.Context, boxliteprotocol.EnsureRequest) (boxliteprotocol.Box, error) {
	return boxliteprotocol.Box{}, nil
}
func (probeBackend) Inspect(context.Context, string) (boxliteprotocol.Box, error) {
	return boxliteprotocol.Box{}, nil
}
func (probeBackend) Recover(context.Context, string) (boxliteprotocol.Box, error) {
	return boxliteprotocol.Box{}, nil
}
func (probeBackend) Delete(context.Context, string) error { return nil }
func (probeBackend) List(context.Context, string) ([]boxliteprotocol.Box, error) {
	return nil, nil
}
func (probeBackend) ListImages(context.Context) ([]string, error) { return nil, nil }
func (probeBackend) PullImage(context.Context, string) error      { return nil }

func completeProbeCapabilities(ready bool) boxliteprotocol.Capabilities {
	capabilities := make(map[string]bool, len(boxliteprotocol.RequiredCapabilities))
	for _, capability := range boxliteprotocol.RequiredCapabilities {
		capabilities[capability] = true
	}
	return boxliteprotocol.Capabilities{
		ProtocolVersion: boxliteprotocol.ProtocolVersionV1,
		Ready:           ready,
		Capabilities:    capabilities,
	}
}

func startProbeServer(t *testing.T, backend boxliteserver.Backend) (string, func()) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "runtime.sock")
	listener, err := listenUnix(socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: &boxliteserver.Server{Backend: backend}}
	go func() { _ = server.Serve(listener) }()
	return socketPath, func() {
		_ = server.Close()
		_ = listener.Close()
	}
}
