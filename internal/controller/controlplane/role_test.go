package controlplane

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoleCapabilities(t *testing.T) {
	fastpath, err := ParseRole("fastpath")
	require.NoError(t, err)
	require.True(t, fastpath.RunsFastPath())
	require.False(t, fastpath.RunsControllers())
	require.False(t, fastpath.LeaderElection())

	controller, err := ParseRole("controller")
	require.NoError(t, err)
	require.False(t, controller.RunsFastPath())
	require.True(t, controller.RunsControllers())
	require.True(t, controller.LeaderElection())

	all, err := ParseRole("all")
	require.NoError(t, err)
	require.True(t, all.RunsFastPath())
	require.True(t, all.RunsControllers())
	require.False(t, all.LeaderElection())

	_, err = ParseRole("unknown")
	require.Error(t, err)
}
