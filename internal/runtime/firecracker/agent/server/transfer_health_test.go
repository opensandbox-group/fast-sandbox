package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
)

// TestServiceHealthReportsTransferFormat: fastlets learn the agent's
// transfer mode from Health; a service without the option (tests, older
// wiring) reports none, which fastlets treat as legacy.
func TestServiceHealthReportsTransferFormat(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	health, err := NewService(newFakePuller(), state, stateRoot).Health(context.Background())
	require.NoError(t, err)
	require.Empty(t, health.TransferFormat)

	for _, format := range []string{agentpull.TransferFormatLegacy, agentpull.TransferFormatChunked} {
		service := NewService(newFakePuller(), state, stateRoot, WithTransferFormat(format))
		health, err := service.Health(context.Background())
		require.NoError(t, err)
		require.Equal(t, format, health.TransferFormat)
		require.Equal(t, format == agentpull.TransferFormatChunked, service.chunked())
	}
}
