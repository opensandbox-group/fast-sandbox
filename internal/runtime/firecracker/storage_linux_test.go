//go:build linux

package firecracker

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStateRootStorageXFS(t *testing.T) {
	root := os.Getenv("FAST_SANDBOX_XFS_TEST_ROOT")
	if root == "" {
		t.Skip("set FAST_SANDBOX_XFS_TEST_ROOT to an XFS mount for a real clone check")
	}
	dir, err := os.MkdirTemp(root, ".startup-storage-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fixture := newDriverFixture(t)
	profile := fixture.driver.profile
	config := *profile.Firecracker
	config.StateRoot = dir
	profile.Firecracker = &config
	driver, err := New(profile)
	require.NoError(t, err)
	require.NoError(t, driver.Initialize(context.Background(), ""))
	t.Cleanup(func() { _ = driver.Close() })
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "real clone check must clean up its probe")
}

func TestStateRootStorageXFSMissingCP(t *testing.T) {
	root := os.Getenv("FAST_SANDBOX_XFS_TEST_ROOT")
	if root == "" {
		t.Skip("set FAST_SANDBOX_XFS_TEST_ROOT to an XFS mount for a real clone check")
	}
	dir, err := os.MkdirTemp(root, ".startup-storage-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("PATH", t.TempDir())
	require.ErrorContains(t, checkStateRootStorage(context.Background(), dir), "install GNU cp (coreutils)")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStateRootStorageRealFilesystem(t *testing.T) {
	root := t.TempDir()
	fsType, err := stateRootFilesystemType(root)
	require.NoError(t, err)
	if fsType == "xfs" {
		t.Skip("non-XFS rejection test requires a non-XFS temporary directory")
	}
	// Exercise the production New/Initialize wiring, not an injected check.
	fixture := newDriverFixture(t)
	driver, err := New(fixture.driver.profile)
	require.NoError(t, err)
	require.ErrorContains(t, driver.Initialize(context.Background(), ""), "must use XFS with reflink=1")
	require.False(t, driver.initialized)
}
