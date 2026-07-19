package fastletproxy

import (
	"testing"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"github.com/stretchr/testify/require"
)

func testRoute(generation int64) Route {
	return Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-a",
		AssignmentAttempt: generation, RouteGeneration: generation,
		Access: fastletnetwork.AccessDescriptor{Kind: fastletnetwork.AccessKindDirectIP, Address: "10.42.0.2"},
		State:  RouteReady,
	}
}

func TestStoreGenerationFencingAndTombstone(t *testing.T) {
	store := NewStore()
	_, err := store.Apply(testRoute(1))
	require.NoError(t, err)
	_, err = store.Apply(testRoute(1))
	require.NoError(t, err)

	conflict := testRoute(1)
	conflict.Access.Address = "10.42.0.3"
	_, err = store.Apply(conflict)
	require.ErrorIs(t, err, ErrRouteConflict)

	_, err = store.Apply(testRoute(2))
	require.NoError(t, err)
	_, err = store.Delete("uid-a", 1)
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = store.Delete("uid-a", 2)
	require.NoError(t, err)
	_, err = store.Apply(testRoute(2))
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = store.Apply(testRoute(3))
	require.NoError(t, err)
}

func TestStoreDrainingRejectsLookup(t *testing.T) {
	store := NewStore()
	_, err := store.Apply(testRoute(1))
	require.NoError(t, err)
	_, err = store.MarkDraining("uid-a", 1)
	require.NoError(t, err)
	_, err = store.Lookup("uid-a")
	require.ErrorIs(t, err, ErrRouteDraining)
}
