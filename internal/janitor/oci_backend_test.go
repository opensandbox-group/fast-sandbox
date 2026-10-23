package janitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestOCICleanupKeepsOtherPodsAndRetainsFailedPack(t *testing.T) {
	root := t.TempDir()
	for _, pod := range []string{"old-pod", "replacement-pod"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "oci/pods", pod, "sandboxes/same-sandbox"), 0750))
	}
	for _, name := range []string{".locks", ".manifest-cache"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "oci/pods/old-pod/sandboxes", name), 0750))
	}
	staging := filepath.Join(root, "snapshots/old-pod/snapshot-a")
	require.NoError(t, os.MkdirAll(staging, 0750))
	raw, err := json.Marshal(fastletapi.SandboxIdentity{FastletPodUID: "old-pod"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, ".pack-owner.json"), raw, 0600))
	backend := NewOCIBackend(root)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 3)
	var commands [][]string
	backend.run = func(_ context.Context, args ...string) error { commands = append(commands, args); return nil }
	for _, resource := range resources {
		if resource.ResourceID == "sandbox/old-pod/same-sandbox" {
			require.NoError(t, backend.Cleanup(context.Background(), resource))
		}
	}
	require.Len(t, commands, 1)
	require.Contains(t, commands[0], filepath.Join(root, "oci/pods/old-pod/sandboxes"))
	require.NotContains(t, commands[0], filepath.Join(root, "oci/pods/replacement-pod/sandboxes"))
	for _, resource := range resources {
		if resource.ResourceID != "pack/old-pod/snapshot-a" {
			continue
		}
		backend.run = func(context.Context, ...string) error { return errors.New("device busy") }
		require.ErrorContains(t, backend.Cleanup(context.Background(), resource), "device busy")
		require.DirExists(t, staging)
		require.NoError(t, os.WriteFile(filepath.Join(staging, ".pack-owner.json"), []byte(`{"fastletPodUid":"replacement-pod"}`), 0600))
		require.ErrorContains(t, backend.Cleanup(context.Background(), resource), "ownership")
	}
}

func TestOCISharedSnapfilesOrphanIdentity(t *testing.T) {
	root := t.TempDir()
	for _, pod := range []string{"old-pod", "replacement-pod"} {
		home := filepath.Join(root, "oci/pods", pod, "sandboxes/.snapfiles", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		require.NoError(t, os.MkdirAll(home, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"device":null}`), 0600))
		locks := filepath.Join(filepath.Dir(home), ".locks")
		require.NoError(t, os.MkdirAll(locks, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(locks, "cache.lock"), nil, 0600))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "oci/pods", pod, "runtime"), 0700))
	}
	backend := NewOCIBackend(root)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 2)
	var commands [][]string
	backend.run = func(_ context.Context, args ...string) error {
		commands = append(commands, args)
		return os.RemoveAll(filepath.Join(root, "oci/pods/old-pod/sandboxes/.snapfiles", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	}
	for _, r := range resources {
		if r.FastletPodUID == "old-pod" {
			require.Equal(t, "snapfiles/old-pod", r.ResourceID)
			require.NoError(t, backend.Cleanup(context.Background(), r))
		}
	}
	require.Len(t, commands, 1)
	require.Contains(t, commands[0], "cleanup-snapfiles")
	require.Contains(t, commands[0], filepath.Join(root, "oci/pods/old-pod/sandboxes"))
	require.NoDirExists(t, filepath.Join(root, "oci/pods/old-pod"))
	require.DirExists(t, filepath.Join(root, "oci/pods/replacement-pod/sandboxes/.snapfiles/.locks"))
}

func TestOCISharedSnapfilesCleanupRetriesLockOnlyDirectory(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "oci/pods/old-pod/sandboxes/.snapfiles")
	require.NoError(t, os.MkdirAll(filepath.Join(cache, ".locks"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(cache, ".locks/cache.lock"), nil, 0600))
	backend := NewOCIBackend(root)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 1, "lock-only caches must remain discoverable after an interrupted cleanup")
	backend.run = func(context.Context, ...string) error { return errors.New("device busy") }
	require.ErrorContains(t, backend.Cleanup(context.Background(), resources[0]), "device busy")
	require.DirExists(t, cache)
	backend.run = func(context.Context, ...string) error { return nil }
	require.NoError(t, backend.Cleanup(context.Background(), resources[0]))
	require.NoDirExists(t, filepath.Join(root, "oci/pods/old-pod"))
	require.NoError(t, backend.Cleanup(context.Background(), resources[0]), "cleanup must be idempotent")
}

func TestOCISharedSnapfilesCleanupPreservesRetainedEntries(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "oci/pods/old-pod/sandboxes/.snapfiles")
	require.NoError(t, os.MkdirAll(filepath.Join(cache, ".locks"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "incomplete-cache"), 0700))
	backend := NewOCIBackend(root)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 1)
	backend.run = func(context.Context, ...string) error { return nil }
	require.ErrorContains(t, backend.Cleanup(context.Background(), resources[0]), "retained")
	require.DirExists(t, filepath.Join(cache, "incomplete-cache"))
	require.DirExists(t, filepath.Join(cache, ".locks"))
}
