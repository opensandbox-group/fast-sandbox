package fastletproxy

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	dataplane "fast-sandbox/internal/dataplane/contract"
)

func testRoute(generation int64) Route {
	return Route{
		RouteKey: RouteKey{SandboxUID: "uid-a", RouteGeneration: generation},
		RouteSpec: RouteSpec{
			Namespace: "default", FastletPodUID: "pod-a", AssignmentAttempt: generation,
			Access: dataplane.AccessDescriptor{Kind: dataplane.AccessKindDirectIP, Address: "10.42.0.2"},
			State:  RouteReady,
		},
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

func TestStoreSnapshotsRouteValues(t *testing.T) {
	store := NewStore()
	route := testRoute(1)
	route.Components = map[string]dataplane.ComponentRoute{
		"execd": {Protocol: "HTTP", Port: 44772},
	}
	_, err := store.Apply(route)
	require.NoError(t, err)

	route.Access.Address = "10.42.0.99"
	route.Components["execd"] = dataplane.ComponentRoute{Protocol: "HTTP", Port: 8080}
	stored, err := store.Lookup(route.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, "10.42.0.2", stored.Access.Address)
	require.Equal(t, uint32(44772), stored.Components["execd"].Port)

	stored.Components["execd"] = dataplane.ComponentRoute{Protocol: "HTTP", Port: 9090}
	storedAgain, err := store.Lookup(route.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, uint32(44772), storedAgain.Components["execd"].Port)
}

func TestStoreConcurrentLookupAndRouteTransitions(t *testing.T) {
	store := NewStore()
	stable := testRoute(1)
	stable.SandboxUID = "stable"
	_, err := store.Apply(stable)
	require.NoError(t, err)

	const iterations = 1000
	errorsChannel := make(chan error, 1)
	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				route, lookupErr := store.Lookup("stable")
				if lookupErr != nil {
					select {
					case errorsChannel <- lookupErr:
					default:
					}
					return
				}
				if route.RouteGeneration != 1 {
					select {
					case errorsChannel <- ErrRouteConflict:
					default:
					}
					return
				}
			}
		}()
	}
	for generation := int64(1); generation <= iterations; generation++ {
		route := testRoute(generation)
		route.SandboxUID = "changing"
		_, err = store.Apply(route)
		require.NoError(t, err)
		_, err = store.MarkDraining(route.SandboxUID, generation)
		require.NoError(t, err)
		_, err = store.Delete(route.SandboxUID, generation)
		require.NoError(t, err)
	}
	readers.Wait()
	select {
	case err := <-errorsChannel:
		require.NoError(t, err)
	default:
	}
}

func TestStoreRecoverySuspensionPreservesGenerationFence(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-apply", true: "published"}[installed], func(t *testing.T) {
			s := NewStore()
			route := testRoute(1)
			if installed {
				_, err := s.Apply(route)
				require.NoError(t, err)
			}
			revision := s.Snapshot().Revision
			_, err := s.SetSuspended("uid-a", 1, revision, true)
			require.NoError(t, err)
			_, err = s.Apply(route) // A late ordinary publication cannot lift suspension.
			require.NoError(t, err)
			_, err = s.Lookup("uid-a")
			require.ErrorIs(t, err, ErrRouteDraining)
			_, err = s.SetSuspended("uid-a", 1, revision, false)
			require.ErrorIs(t, err, ErrRouteConflict)
			_, err = s.SetSuspended("uid-a", 1, s.Snapshot().Revision, false)
			require.NoError(t, err)
			restored, err := s.Lookup("uid-a")
			require.NoError(t, err)
			require.Equal(t, route, restored)
			lateRevision := s.Snapshot().Revision
			_, err = s.SetSuspended("uid-a", 1, lateRevision, true)
			require.NoError(t, err)
			_, err = s.SetSuspended("uid-a", 1, lateRevision, false)
			require.ErrorIs(t, err, ErrRouteConflict, "late resume cannot undo a newer suspension")
			_, err = s.Delete("uid-a", 1)
			require.NoError(t, err)
			_, err = s.SetSuspended("uid-a", 1, s.Snapshot().Revision, false)
			require.ErrorIs(t, err, ErrRouteStale)
			_, err = s.Apply(route)
			require.ErrorIs(t, err, ErrRouteStale, "permanent deletion remains irreversible")
			_, err = s.Apply(testRoute(2))
			require.NoError(t, err)
		})
	}
}

func TestStoreSuspensionRejectsOlderAbsentOwner(t *testing.T) {
	s := NewStore()
	_, err := s.SetSuspended("uid-a", 2, s.Snapshot().Revision, true)
	require.NoError(t, err)
	_, err = s.SetSuspended("uid-a", 1, s.Snapshot().Revision, true)
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = s.SetSuspended("uid-a", 1, s.Snapshot().Revision, false)
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = s.Apply(testRoute(1))
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = s.Apply(testRoute(2))
	require.NoError(t, err)
	_, err = s.SetSuspended("uid-a", 3, s.Snapshot().Revision, false)
	require.NoError(t, err)
	_, err = s.Apply(testRoute(3))
	require.NoError(t, err)
	route, err := s.Lookup("uid-a")
	require.NoError(t, err)
	require.EqualValues(t, 3, route.RouteGeneration)
}

func TestStoreNewOwnerCannotReactivatePreviousRoute(t *testing.T) {
	s := NewStore()
	_, err := s.Apply(testRoute(1))
	require.NoError(t, err)
	_, err = s.SetSuspended("uid-a", 2, s.Snapshot().Revision, true)
	require.NoError(t, err)
	_, err = s.Lookup("uid-a")
	require.ErrorIs(t, err, ErrRouteDraining)
	_, err = s.SetSuspended("uid-a", 2, s.Snapshot().Revision, false)
	require.NoError(t, err)
	_, err = s.Lookup("uid-a")
	require.ErrorIs(t, err, ErrRouteDraining, "resume for a new generation must not activate the previous destination")
	_, err = s.Apply(testRoute(2))
	require.NoError(t, err)
	route, err := s.Lookup("uid-a")
	require.NoError(t, err)
	require.EqualValues(t, 2, route.RouteGeneration)
}

func TestStoreResumeBeforeApplyFencesOlderGeneration(t *testing.T) {
	s := NewStore()
	_, err := s.SetSuspended("uid-a", 2, s.Snapshot().Revision, true)
	require.NoError(t, err)
	_, err = s.SetSuspended("uid-a", 2, s.Snapshot().Revision, false)
	require.NoError(t, err)
	_, err = s.Apply(testRoute(1))
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = s.Apply(testRoute(2))
	require.NoError(t, err)
}
