package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStateRootStorageRejectsNonXFS(t *testing.T) {
	for _, fsType := range []string{"ext4", "btrfs", "tmpfs", "overlay", "unknown"} {
		t.Run(fsType, func(t *testing.T) {
			root := t.TempDir()
			called := false
			err := probeStateRootStorage(context.Background(), root, fsType, func(context.Context, string, string) error {
				called = true
				return nil
			})
			require.ErrorContains(t, err, "must use XFS with reflink=1")
			require.ErrorContains(t, err, fsType)
			require.False(t, called, "reject filesystem before attempting a clone")
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestStateRootStorageProbeCleansUp(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "clone failure"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cloneErr := errors.New("reflink not supported")
			called := false
			err := probeStateRootStorage(context.Background(), root, "xfs", func(_ context.Context, source, target string) error {
				called = true
				payload, err := os.ReadFile(source)
				require.NoError(t, err)
				require.NotEmpty(t, payload)
				require.Equal(t, filepath.Dir(source), filepath.Dir(target))
				// A failed cp can leave a partial target too.
				require.NoError(t, os.WriteFile(target, payload, 0o600))
				if fail {
					return cloneErr
				}
				return nil
			})
			require.True(t, called)
			if fail {
				require.ErrorIs(t, err, cloneErr)
				require.ErrorContains(t, err, "install GNU cp (coreutils)")
			} else {
				require.NoError(t, err)
			}
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Empty(t, entries, "startup probes must not leave files in StateRoot")
		})
	}
}

func TestInitializeStorageFailureDoesNotStartRuntime(t *testing.T) {
	fixture := newDriverFixture(t)
	want := errors.New("StateRoot must use XFS with reflink=1")
	checks := 0
	fixture.driver.checkStorage = func(_ context.Context, root string) error {
		checks++
		require.Equal(t, fixture.stateRoot, root)
		return want
	}
	require.ErrorIs(t, fixture.driver.Initialize(context.Background(), ""), want)
	require.False(t, fixture.driver.initialized)
	require.Nil(t, fixture.driver.gcStop)
	require.Nil(t, fixture.driver.gcTrigger)
	// A failed check must not poison initialization or be cached as success.
	require.ErrorIs(t, fixture.driver.Initialize(context.Background(), ""), want)
	require.Equal(t, 2, checks)
	fixture.driver.checkStorage = func(context.Context, string) error { return nil }
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	require.True(t, fixture.driver.initialized)
	t.Cleanup(func() { _ = fixture.driver.Close() })
}
