package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
)

// TestServiceHealthP2PProbe wires the peer-distribution state into Health:
// P2PUp follows the probe, agent OK stays independent of it.
func TestServiceHealthP2PProbe(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	probed := true
	service := NewService(newFakePuller(), state, stateRoot, WithP2PProbe(func() bool { return probed }))
	health, err := service.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.OK)
	require.True(t, health.P2PUp, "Health must reflect the P2P probe")

	probed = false
	health, err = service.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.OK, "agent health stays green when the P2P provider is down")
	require.False(t, health.P2PUp)
}

func TestServiceHealthWithoutP2PProbe(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	service := NewService(newFakePuller(), state, stateRoot)
	health, err := service.Health(context.Background())
	require.NoError(t, err)
	require.False(t, health.P2PUp, "no P2P probe configured = direct-S3 mode")
}
