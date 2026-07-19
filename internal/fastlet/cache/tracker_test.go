package cache

import (
	"context"
	"testing"

	"fast-sandbox/internal/api"

	"github.com/stretchr/testify/require"
)

type imageSource struct{ images []string }

func (s *imageSource) ListImages(context.Context) ([]string, error) {
	return append([]string(nil), s.images...), nil
}

func TestTrackerUsesEpochRevisionAndUnchangedCursor(t *testing.T) {
	source := &imageSource{images: []string{
		"docker.io/library/alpine:latest", "alpine:latest", "sha256:deadbeef",
		"import-2026-07-19@sha256:abc", "registry.k8s.io/pause:3.9", "docker.io/fast-sandbox/fastlet:dev",
	}}
	tracker := NewTracker(source, "boot-a", 100)
	first, err := tracker.Snapshot(context.Background(), api.CacheCursor{})
	require.NoError(t, err)
	require.True(t, first.Full)
	require.True(t, first.Complete)
	require.Equal(t, uint64(1), first.Revision)
	require.Equal(t, []string{"alpine:latest"}, first.Images)

	unchanged, err := tracker.Snapshot(context.Background(), api.CacheCursor{Epoch: first.Epoch, Revision: first.Revision})
	require.NoError(t, err)
	require.False(t, unchanged.Full)
	require.Empty(t, unchanged.Images)

	source.images = append(source.images, "docker.io/library/ubuntu:24.04")
	changed, err := tracker.Snapshot(context.Background(), api.CacheCursor{Epoch: first.Epoch, Revision: first.Revision})
	require.NoError(t, err)
	require.True(t, changed.Full)
	require.Equal(t, uint64(2), changed.Revision)
	require.Equal(t, []string{"alpine:latest", "ubuntu:24.04"}, changed.Images)
}

func TestTrackerFailsCacheAffinityClosedWhenInventoryExceedsLimit(t *testing.T) {
	tracker := NewTracker(&imageSource{images: []string{"a:1", "b:1"}}, "boot-a", 1)
	snapshot, err := tracker.Snapshot(context.Background(), api.CacheCursor{})
	require.NoError(t, err)
	require.True(t, snapshot.Full)
	require.False(t, snapshot.Complete)
	require.Empty(t, snapshot.Images)
}

func TestNormalizeReferenceMatchesDockerHubShorthand(t *testing.T) {
	require.Equal(t, "alpine:latest", NormalizeReference("docker.io/library/alpine:latest"))
	require.Equal(t, "example.com/team/image:v1", NormalizeReference("https://EXAMPLE.COM/team/image:v1"))
}
