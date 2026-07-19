package infra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArtifactStoreVerifiesDigestAndReusesReadOnlyBlob(t *testing.T) {
	podRoot := t.TempDir()
	store, err := NewArtifactStore(podRoot, "/host/infra")
	require.NoError(t, err)
	payload := []byte("immutable infra artifact")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
	opens := 0
	open := func() (io.ReadCloser, error) {
		opens++
		return io.NopCloser(bytes.NewReader(payload)), nil
	}

	first, err := store.Stage(context.Background(), digest, true, open)
	require.NoError(t, err)
	require.False(t, first.CacheHit)
	require.Equal(t, 1, opens)
	require.Equal(t, filepath.Join("/host/infra", "blobs", "sha256", digest[len("sha256:"):], "executable"), first.HostPath)
	info, err := os.Stat(first.PodPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0555), info.Mode().Perm())

	second, err := store.Stage(context.Background(), digest, true, open)
	require.NoError(t, err)
	require.True(t, second.CacheHit)
	require.Equal(t, 1, opens, "cache hit must not reopen registry/static source")
}

func TestArtifactStoreSeparatesExecutableAndDataVariants(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	payload := []byte("same immutable bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }

	data, err := store.Stage(context.Background(), digest, false, open)
	require.NoError(t, err)
	executable, err := store.Stage(context.Background(), digest, true, open)
	require.NoError(t, err)
	require.NotEqual(t, data.PodPath, executable.PodPath)
	require.Equal(t, os.FileMode(0444), fileMode(t, data.PodPath))
	require.Equal(t, os.FileMode(0555), fileMode(t, executable.PodPath))
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

func TestArtifactStoreRejectsMismatchAndCorruption(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("expected")))
	_, err = store.Stage(context.Background(), expected, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("wrong")), nil
	})
	require.ErrorIs(t, err, ErrDigestMismatch)

	prepared, err := store.Stage(context.Background(), expected, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("expected")), nil
	})
	require.NoError(t, err)
	require.NoError(t, os.Chmod(prepared.PodPath, 0644))
	require.NoError(t, os.WriteFile(prepared.PodPath, []byte("corrupt"), 0444))
	_, _, err = store.Lookup(context.Background(), expected)
	require.ErrorIs(t, err, ErrArtifactCorrupted)
}

func TestArtifactStoreEnforcesBoundAndCancellation(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	store.SetMaxBytes(4)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("12345")))
	_, err = store.Stage(context.Background(), digest, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("12345")), nil
	})
	require.ErrorIs(t, err, ErrArtifactTooLarge)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.Stage(ctx, digest, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("12345")), nil
	})
	require.ErrorIs(t, err, context.Canceled)
}
