package server

import (
	"context"
	"testing"

	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"

	"github.com/stretchr/testify/require"
)

// TestServiceHealthHostReadyProbe wires the node-readiness outcome into
// Health: the flag and summary follow the probe, and the agent's own OK
// stays independent of the host verdict (a not-yet-labeled node still
// serves pulls).
func TestServiceHealthHostReadyProbe(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	ready := false
	summary := "check pending"
	service := NewService(newFakePuller(), state, stateRoot, WithHostReadyProbe(func() (bool, string) { return ready, summary }))
	health, err := service.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.OK)
	require.NotNil(t, health.HostReady)
	require.False(t, *health.HostReady)
	require.Equal(t, "check pending", health.HostState)

	ready = true
	summary = "9 checks: 9 pass, 0 warn, 0 fail"
	health, err = service.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.OK, "agent health stays green regardless of the host verdict")
	require.NotNil(t, health.HostReady)
	require.True(t, *health.HostReady)
	require.Equal(t, summary, health.HostState)
}

func TestServiceHealthWithoutHostReadyProbe(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	service := NewService(newFakePuller(), state, stateRoot)
	health, err := service.Health(context.Background())
	require.NoError(t, err)
	require.Nil(t, health.HostReady, "no readiness probe configured = readiness manager disabled")
	require.Empty(t, health.HostState)
}
