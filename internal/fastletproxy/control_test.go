package fastletproxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"github.com/stretchr/testify/require"
)

func TestUnixControlApplySnapshotWatchAndDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "control.sock")
	store := NewStore()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- (&ControlServer{Store: store, SocketPath: socket}).Serve(ctx) }()
	client := NewControlClient(socket)
	require.Eventually(t, func() bool {
		_, err := client.Snapshot(context.Background())
		return err == nil
	}, time.Second, 10*time.Millisecond)
	route := Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-a", AssignmentAttempt: 1, RouteGeneration: 1,
		Access: fastletnetwork.AccessDescriptor{Kind: fastletnetwork.AccessKindDirectIP, Address: "10.42.0.2"}, State: RouteReady,
	}
	require.NoError(t, client.Apply(context.Background(), route))
	snapshot, err := client.Snapshot(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.Routes, 1)

	events := make(chan Event, 1)
	watchContext, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go func() {
		_ = client.Watch(watchContext, func(event Event) error {
			events <- event
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, client.MarkDraining(context.Background(), route.SandboxUID, route.RouteGeneration))
	select {
	case event := <-events:
		require.Equal(t, EventDraining, event.Type)
	case <-time.After(time.Second):
		t.Fatal("route watch did not receive draining event")
	}
	require.NoError(t, client.Delete(context.Background(), route.SandboxUID, route.RouteGeneration))
	snapshot, err = client.Snapshot(context.Background())
	require.NoError(t, err)
	require.Empty(t, snapshot.Routes)
	cancel()
	select {
	case err := <-serverErrors:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("control server did not stop")
	}
}
