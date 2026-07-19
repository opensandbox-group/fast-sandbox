package janitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"

	"github.com/stretchr/testify/require"
)

type recordingNetworkDriver struct {
	destroyed []string
}

func (*recordingNetworkDriver) Prepare(context.Context, *fastletnetwork.Slot) error  { return nil }
func (*recordingNetworkDriver) Validate(context.Context, *fastletnetwork.Slot) error { return nil }
func (d *recordingNetworkDriver) Destroy(_ context.Context, slot *fastletnetwork.Slot) error {
	d.destroyed = append(d.destroyed, slot.ID)
	return nil
}

func TestLinuxNetworkBackendCleanupIsIdentityFencedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	podUID := "pod-a"
	store := fastletnetwork.NewFileStateStore(filepath.Join(root, podUID))
	slot := &fastletnetwork.Slot{
		Version: 1, ID: "slot-a", OwnerPodUID: podUID, Phase: fastletnetwork.SlotPhaseBound,
		Owner: fastletnetwork.Owner{
			SandboxUID: "sandbox-a", SandboxName: "sandbox-a", SandboxNamespace: "default",
			InstanceGeneration: 1, AssignmentAttempt: 1,
		},
		CreatedAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, store.Save(context.Background(), slot))
	driver := &recordingNetworkDriver{}
	backend := NewLinuxNetworkBackend(root, driver)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 1)
	require.Equal(t, BackendLinuxNetwork, resources[0].Backend)

	stale := resources[0]
	stale.AssignmentAttempt = 2
	require.Error(t, backend.Cleanup(context.Background(), stale))
	require.Empty(t, driver.destroyed)

	require.NoError(t, backend.Cleanup(context.Background(), resources[0]))
	require.Equal(t, []string{"slot-a"}, driver.destroyed)
	_, err = os.Stat(filepath.Join(root, podUID, "slot-a.json"))
	require.True(t, os.IsNotExist(err))

	require.NoError(t, backend.Cleanup(context.Background(), resources[0]))
	require.Equal(t, []string{"slot-a"}, driver.destroyed)
}

func TestCleanupFIFOs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"container-a-stdout", "container-a-stderr", "other"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("test"), 0o600))
	}
	require.NoError(t, cleanupFIFOs(root, "container-a"))
	_, err := os.Stat(filepath.Join(root, "container-a-stdout"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "container-a-stderr"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "other"))
	require.NoError(t, err)
}
