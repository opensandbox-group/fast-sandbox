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

func TestCheckpointCacheUnavailableKeepsManifestCache(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			setupManifestCache(t)
			raw := cacheTestManifest(t, "blob")
			reference := "registry.invalid/checkpoint@" + digest.FromBytes(raw).String()
			metadata, _ := json.Marshal(map[string]any{"oci": ocitemplate.Pair{Rootfs: reference, Snapfiles: reference}})
			result := &ocitemplate.PublishedCheckpoint{Reference: ocitemplate.Prefix + reference, Metadata: metadata, Manifests: map[string]json.RawMessage{reference: raw}}
			dir := t.TempDir()
			path := filepath.Join(dir, "blobs/sha256", digest.FromString("blob").Encoded())
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, []byte("blob"), 0600))
			client := acquireClient(t, func(*http.Request) (*http.Response, error) {
				return fenceReply(code, `{"ok":false,"error":"cache unavailable"}`), nil
			})
			require.ErrorContains(t, cacheCheckpoint(context.Background(), result, dockerConfig{}, dir, client), "cache unavailable")
			_, err := resolveImage(context.Background(), reference, dockerConfig{})
			require.NoError(t, err, "metadata remains usable independently of blob cache import")
		})
	}
}
