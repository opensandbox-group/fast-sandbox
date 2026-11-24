package ocitemplate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestDeletePairPreservesOtherSnapshotsAndSharedBlobs(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	tags := map[string]string{}
	failSnap := false
	var deletedBlobs int
	handler := registrytest.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/blobs/") {
			deletedBlobs++
		}
		const prefix = "/v2/snapshots/test/manifests/"
		name := strings.TrimPrefix(r.URL.Path, prefix)
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, prefix) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			tags[name] = response.Header().Get("Docker-Content-Digest")
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix) {
			if failSnap && tags["first_snapfiles"] == name {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// The small in-memory registry does not unlink aliases on digest
			// deletion. Model the OCI requirement explicitly in this fixture.
			for tag, sum := range tags {
				if sum == name {
					handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, prefix+tag, nil))
					delete(tags, tag)
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	previous := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = previous }()
	repository := strings.TrimPrefix(server.URL, "https://") + "/snapshots/test"
	dir := localPairFixture(t)
	first, _, err := Push(ctx, dir, repository+":first", []byte(`{}`), nil)
	require.NoError(t, err)
	second, _, err := Push(ctx, dir, repository+":second", []byte(`{}`), nil)
	require.NoError(t, err)
	repo, _, err := Repository(first, nil)
	require.NoError(t, err)
	root, err := repo.Resolve(ctx, "first_rootfs")
	require.NoError(t, err)
	otherRoot, err := repo.Resolve(ctx, "second_rootfs")
	require.NoError(t, err)
	require.NotEqual(t, root.Digest, otherRoot.Digest, "pair ownership must distinguish manifests with identical layers")
	require.ErrorContains(t, DeletePair(ctx, first, "second", nil), "pair ID mismatch")
	_, err = resolveTestPair(t, ctx, first, nil)
	require.NoError(t, err, "validation must precede all deletions")

	mu.Lock()
	failSnap = true
	mu.Unlock()
	require.Error(t, DeletePair(ctx, first, "first", nil))
	_, err = repo.Resolve(ctx, "first_rootfs")
	require.True(t, errors.Is(err, errdef.ErrNotFound))
	_, err = repo.Resolve(ctx, "first_snapfiles")
	require.NoError(t, err, "publication point remains for retry after partial deletion")
	mu.Lock()
	failSnap = false
	mu.Unlock()
	require.NoError(t, DeletePair(ctx, first, "first", nil))
	require.NoError(t, DeletePair(ctx, first, "first", nil), "retry after complete deletion is idempotent")
	_, err = repo.Resolve(ctx, "first_snapfiles")
	require.ErrorIs(t, err, errdef.ErrNotFound)
	replacement, _, err := Push(ctx, dir, repository+":first", []byte(`{"platform":{"checkpoint":{"sandboxUID":"replacement","namespace":"test","id":"first"}}}`), nil)
	require.NoError(t, err)
	require.NotEqual(t, first, replacement)
	require.NoError(t, DeletePair(ctx, first, "first", nil))
	_, err = resolveTestPair(t, ctx, replacement, nil)
	require.NoError(t, err, "a stale deletion retry must not remove a replacement's rootfs")
	_, err = resolveTestPair(t, ctx, second, nil)
	require.NoError(t, err)
	mu.Lock()
	require.Zero(t, deletedBlobs, "shared config/layer blobs belong to registry GC")
	mu.Unlock()
}
