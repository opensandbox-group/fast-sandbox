package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileStateStoreRoundTripAndDelete(t *testing.T) {
	store := NewFileStateStore(t.TempDir())
	slot := &Slot{Version: 1, ID: "slot-1", OwnerPodUID: "pod-1", Phase: SlotPhaseClean}
	require.NoError(t, store.Save(context.Background(), slot))

	loaded, err := store.LoadAll(context.Background())
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, slot, loaded[0])

	info, err := os.Stat(filepath.Join(store.Root(), "slot-1.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoError(t, store.Delete(context.Background(), slot.ID))
	loaded, err = store.LoadAll(context.Background())
	require.NoError(t, err)
	require.Empty(t, loaded)
}

func TestFileStateStoreRejectsCorruptState(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "slot-1.json"), []byte("{"), 0o600))
	_, err := NewFileStateStore(root).LoadAll(context.Background())
	require.Error(t, err)
}
