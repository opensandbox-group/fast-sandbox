package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/retry"

	"fast-sandbox/internal/ocitemplate"
)

func TestPushPackedPairWithCheckpointAnnotations(t *testing.T) {
	layout, _, err := createStagedArtifactLayout(t.TempDir(), "checkpoint")
	require.NoError(t, err)
	parent, err := layout.installBytes([]byte("parent layer"), ocitemplate.OverlayBDLayerMediaType)
	require.NoError(t, err)
	delta, err := layout.installBytes([]byte("new delta layer"), ocitemplate.OverlayBDLayerMediaType)
	require.NoError(t, err)
	snap, err := layout.installBytes([]byte("snapshot layer"), ocitemplate.OverlayBDLayerMediaType)
	require.NoError(t, err)
	names, err := parseOutput("snapshot:checkpoint-1")
	require.NoError(t, err)
	_, err = writeTemplatePair(layout, names, []oci.Descriptor{parent, delta}, snap, "checkpoint", nil, 0)
	require.NoError(t, err)
	server := httptest.NewTLSServer(registrytest.New())
	defer server.Close()
	previous := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = previous }()
	metadata, auth := filepath.Join(t.TempDir(), "manifest.json"), filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(metadata, []byte(`{"machine":{"vcpu":"1"},"platform":{"checkpoint":{"sandboxUID":"sandbox-1","namespace":"tenant"}}}`), 0600))
	require.NoError(t, os.WriteFile(auth, []byte(`{"auths":{}}`), 0600))
	cmd := newPushCommand()
	cmd.SetArgs([]string{"--artifact-dir", layout.root, "--destination", strings.TrimPrefix(server.URL, "https://") + "/checkpoint/test:checkpoint-1", "--metadata-file", metadata, "--auth-file", auth})
	require.NoError(t, cmd.Execute())
	// Resolve through the default paired-tag convention, then inspect exactly
	// the snapfiles manifest returned as the checkpoint publication point.
	ref := strings.TrimPrefix(server.URL, "https://") + "/checkpoint/test:checkpoint-1"
	repository, reference, err := ocitemplate.Repository(ref, nil)
	require.NoError(t, err)
	payload, err := ocitemplate.ResolveFrom(context.Background(), reference, repository)
	require.NoError(t, err)
	pair, err := ocitemplate.Decode(payload)
	require.NoError(t, err)
	repo, parsed, err := ocitemplate.Repository(pair.Snapfiles, nil)
	require.NoError(t, err)
	descriptor, err := repo.Resolve(context.Background(), parsed.Reference)
	require.NoError(t, err)
	stream, err := repo.Manifests().Fetch(context.Background(), descriptor)
	require.NoError(t, err)
	defer stream.Close()
	var manifest oci.Manifest
	require.NoError(t, json.NewDecoder(stream).Decode(&manifest))
	require.Equal(t, "sandbox-1", manifest.Annotations[ocitemplate.AnnotationSandboxUID])
	require.Equal(t, "tenant", manifest.Annotations[ocitemplate.AnnotationNamespace])
	require.Equal(t, "checkpoint-1", manifest.Annotations[ocitemplate.AnnotationCheckpointID])
	require.Equal(t, []oci.Descriptor{snap}, manifest.Layers)
}

func TestPushPlainHTTP(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "anonymous", true: "basic-auth"}[private], func(t *testing.T) {
			previous := global
			flag := rootCmd.PersistentFlags().Lookup("plain-http")
			changed := flag.Changed
			t.Cleanup(func() { global = previous; flag.Changed = changed })
			layout, _, err := createStagedArtifactLayout(t.TempDir(), "http")
			require.NoError(t, err)
			layer, err := layout.installBytes([]byte("snapshot data"), ocitemplate.OverlayBDLayerMediaType)
			require.NoError(t, err)
			names, err := parseOutput("snapshot:http-test")
			require.NoError(t, err)
			_, err = writeTemplatePair(layout, names, []oci.Descriptor{layer}, layer, "checkpoint", nil, 0)
			require.NoError(t, err)
			handler := registrytest.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if private {
					username, password, ok := r.BasicAuth()
					if !ok || username != "writer" || password != "secret" {
						w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			config := map[string]any{"auths": map[string]any{}}
			if private {
				config["auths"] = map[string]any{host: authEntry{Username: "writer", Password: "secret"}}
			}
			raw, err := json.Marshal(config)
			require.NoError(t, err)
			metadata, auth := filepath.Join(t.TempDir(), "manifest.json"), filepath.Join(t.TempDir(), "auth.json")
			require.NoError(t, os.WriteFile(metadata, []byte(`{}`), 0600))
			require.NoError(t, os.WriteFile(auth, raw, 0600))
			args := []string{"--artifact-dir", layout.root, "--destination", host + "/snapshots/test:http-test", "--metadata-file", metadata, "--auth-file", auth}
			// HTTP must be explicitly selected, even for a local registry.
			global.plainHTTP = false
			command := newPushCommand()
			command.SetArgs(args)
			require.Error(t, command.Execute())
			require.NoError(t, rootCmd.PersistentFlags().Set("plain-http", "true"))
			command = newPushCommand()
			command.SetArgs(args)
			require.NoError(t, command.Execute())
		})
	}
}
