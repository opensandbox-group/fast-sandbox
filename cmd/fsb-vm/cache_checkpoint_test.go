package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"fast-sandbox/internal/ocitemplate"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func TestCheckpointPublicationCacheReusedOfflineAndEvicted(t *testing.T) {
	setupManifestCache(t)
	root, snap := cacheTestManifest(t, "inherited"), cacheTestManifest(t, "new checkpoint")
	registry := newManifestCacheRegistry(t, root)
	registry.byDigest[digest.FromBytes(snap).String()] = snap
	rootRef, snapRef := registry.reference(digest.FromBytes(root).String()), registry.reference(digest.FromBytes(snap).String())
	metadata, err := json.Marshal(map[string]any{"oci": ocitemplate.Pair{Rootfs: rootRef, Snapfiles: snapRef}})
	require.NoError(t, err)
	result := &ocitemplate.PublishedCheckpoint{Reference: ocitemplate.Prefix + snapRef, Metadata: metadata, Manifests: map[string]json.RawMessage{rootRef: root, snapRef: snap}}
	artifacts := t.TempDir()
	blob := filepath.Join(artifacts, "blobs/sha256", digest.FromString("new checkpoint").Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(blob), 0700))
	require.NoError(t, os.WriteFile(blob, []byte("new checkpoint"), 0600))
	calls := 0
	client := acquireClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "/v1/cache/import", req.URL.Path)
		var body struct {
			Path, URL, Digest string
			Size              int64
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		require.Equal(t, blob, body.Path)
		require.Equal(t, digest.FromString("new checkpoint").String(), body.Digest)
		require.Equal(t, int64(14), body.Size)
		return fenceReply(200, `{"ok":true}`), nil
	})
	require.NoError(t, cacheCheckpoint(context.Background(), result, dockerConfig{}, artifacts, client))
	require.Equal(t, 1, calls, "only new local layers are imported")
	require.Zero(t, registry.requestCount(), "publication seed must not fetch remote metadata")
	require.NoError(t, os.RemoveAll(artifacts), "pack staging can be deleted")
	registry.mu.Lock()
	registry.deny = true
	registry.mu.Unlock()
	var template templateFile
	template.Template.Reference = result.Reference
	require.NoError(t, resolveTemplate(context.Background(), &template, dockerConfig{}))
	for _, ref := range []string{template.Template.Rootfs, template.Template.Snapfiles} {
		_, err := resolveImage(context.Background(), ref, dockerConfig{})
		require.NoError(t, err)
	}
	require.Zero(t, registry.requestCount(), "resume metadata works with registry denied")
	// Existing metadata GC removes the imported entries. Resolve follows the
	// normal remote miss path rather than depending on deleted pack staging.
	require.NoError(t, trimManifestCache(manifestCacheRoot(), 0, 0))
	registry.mu.Lock()
	registry.deny = false
	registry.mu.Unlock()
	_, err = resolveImage(context.Background(), snapRef, dockerConfig{})
	require.NoError(t, err)
	require.Positive(t, registry.requestCount())
}

func TestCheckpointCacheRejectsWrongPublishedDigest(t *testing.T) {
	setupManifestCache(t)
	root, snap := cacheTestManifest(t, "root"), cacheTestManifest(t, "snap")
	rootRef := "registry.invalid/team/checkpoint@" + digest.FromBytes(root).String()
	snapRef := "registry.invalid/team/checkpoint@" + digest.FromBytes(snap).String()
	metadata, _ := json.Marshal(map[string]any{"oci": ocitemplate.Pair{Rootfs: rootRef, Snapfiles: snapRef}})
	result := &ocitemplate.PublishedCheckpoint{Reference: ocitemplate.Prefix + snapRef, Metadata: metadata, Manifests: map[string]json.RawMessage{rootRef: snap, snapRef: snap}}
	client := acquireClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("must not import invalid metadata")
		return nil, nil
	})
	require.ErrorContains(t, cacheCheckpoint(context.Background(), result, dockerConfig{}, t.TempDir(), client), "digest mismatch")
}
