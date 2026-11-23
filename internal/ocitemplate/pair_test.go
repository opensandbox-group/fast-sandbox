package ocitemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func localPairFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifest"), 0750))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "blobs/sha256"), 0750))
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	config := content.NewDescriptorFromBytes(oci.MediaTypeImageConfig, cfg)
	layer := content.NewDescriptorFromBytes(oci.MediaTypeImageLayer, []byte("snapshot data"))
	for _, b := range []struct {
		desc oci.Descriptor
		raw  []byte
	}{{config, cfg}, {layer, []byte("snapshot data")}} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blobs/sha256", b.desc.Digest.Encoded()), b.raw, 0640))
	}
	raw, err := json.Marshal(oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config, Layers: []oci.Descriptor{layer}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/pair.json"), raw, 0640))
	// Use distinct disk manifests while sharing their blobs, as real rootfs
	// and snapfiles images do.
	var snap oci.Manifest
	require.NoError(t, json.Unmarshal(raw, &snap))
	snap.Annotations = map[string]string{"fixture": "snapfiles"}
	snapRaw, err := json.Marshal(snap)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/snapfiles.json"), snapRaw, 0640))
	// Reversed input order must not change the required publication order.
	index := map[string]any{"images": []map[string]string{
		{"name": "test:v1_snapfiles", "manifest_file": "snapfiles.json", "manifest_digest": digest.FromBytes(snapRaw).String()},
		{"name": "test:v1_rootfs", "manifest_file": "pair.json", "manifest_digest": digest.FromBytes(raw).String()},
	}}
	raw, err = json.Marshal(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/index.json"), raw, 0640))
	return dir
}

func TestCheckpointOwnerLivesOnSnapfilesManifest(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewTLSServer(registrytest.New())
	defer server.Close()
	previous := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = previous }()
	dir := localPairFixture(t)
	destination := strings.TrimPrefix(server.URL, "https://") + "/checkpoints/sandbox:checkpoint-1"
	metadata := []byte(`{"machine":{"vcpu":"1","memory":"512Mi"},"guestNetwork":{"ip":"172.30.0.3"},"actionBindings":[{"action":"exec"}],"platform":{"checkpoint":{"sandboxUID":"owner","namespace":"tenant"}}}`)
	ref, sum, err := Push(ctx, dir, destination, metadata, nil)
	require.NoError(t, err)
	repo, _, err := Repository(ref, nil)
	require.NoError(t, err)
	desc, err := repo.Resolve(ctx, "checkpoint-1_snapfiles")
	require.NoError(t, err)
	require.Equal(t, "sha256:"+sum, desc.Digest.String())
	raw, err := fetchSmall(ctx, repo, desc)
	require.NoError(t, err)
	var manifest oci.Manifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.Equal(t, "owner", manifest.Annotations[AnnotationSandboxUID])
	require.Equal(t, "tenant", manifest.Annotations[AnnotationNamespace])
	require.Equal(t, "checkpoint-1", manifest.Annotations[AnnotationCheckpointID])
	require.NotContains(t, manifest.Annotations, "io.fast-sandbox.metadata")
	got, err := resolveTestPair(t, ctx, ref, nil)
	require.NoError(t, err)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &doc))
	require.JSONEq(t, `{"sandboxUID":"owner","namespace":"tenant","id":"checkpoint-1"}`, string(doc["checkpoint"]))
	for _, key := range []string{"machine", "guestNetwork", "actionBindings"} {
		require.NotContains(t, doc, key, "platform metadata is read only from snapfiles")
	}
	pair, err := Decode(got)
	require.NoError(t, err)
	require.Equal(t, strings.TrimPrefix(ref, Prefix), pair.Snapfiles)
	for _, mode := range []string{"missing-uid", "missing-namespace", "missing-id", "mismatched-id", "bad-pair-id"} {
		t.Run(mode, func(t *testing.T) {
			var tampered oci.Manifest
			require.NoError(t, json.Unmarshal(raw, &tampered))
			switch mode {
			case "missing-uid":
				delete(tampered.Annotations, AnnotationSandboxUID)
			case "missing-namespace":
				delete(tampered.Annotations, AnnotationNamespace)
			case "missing-id":
				delete(tampered.Annotations, AnnotationCheckpointID)
			case "mismatched-id":
				tampered.Annotations[AnnotationCheckpointID] = "other"
			case "bad-pair-id":
				tampered.Annotations[AnnotationTemplateID] = "../../other"
			}
			payload, err := json.Marshal(tampered)
			require.NoError(t, err)
			desc := content.NewDescriptorFromBytes(oci.MediaTypeImageManifest, payload)
			require.NoError(t, repo.Manifests().Push(ctx, desc, bytes.NewReader(payload)))
			_, err = resolveTestPair(t, ctx, repo.Reference.Registry+"/"+repo.Reference.Repository+"@"+desc.Digest.String(), nil)
			require.Error(t, err)
		})
	}
}

func TestPairResolutionRequiresRootfsAndDoesNotDownloadLayers(t *testing.T) {
	var missingRoot atomic.Bool
	handler := registrytest.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if missingRoot.Load() && strings.HasSuffix(r.URL.Path, "/manifests/v1_rootfs") {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	previous := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = previous }()
	ref, _, err := Push(context.Background(), localPairFixture(t), strings.TrimPrefix(server.URL, "https://")+"/pair/test:v1", []byte(`{}`), nil)
	require.NoError(t, err)
	missingRoot.Store(true)
	_, err = resolveTestPair(t, context.Background(), ref, nil)
	require.Error(t, err, "a snapfiles manifest cannot be ready without its same-ID rootfs")
}

func TestPairMetadataRejectsInvalidIDsAndOwners(t *testing.T) {
	for _, id := range []string{"", "../other", "sha256:abc", strings.Repeat("a", 119)} {
		_, err := pairAnnotations(id, []byte(`{}`))
		require.Error(t, err, id)
	}
	for _, raw := range []string{`null`, `[]`, `{"platform":{"checkpoint":{}}}`, `{"platform":{"checkpoint":{"sandboxUID":"owner"}}}`, `{"platform":{"checkpoint":{"sandboxUID":"owner","namespace":" "}}}`} {
		_, err := pairAnnotations("valid-id", []byte(raw))
		require.Error(t, err, raw)
	}
	_, err := pairAnnotations(strings.Repeat("a", 118), []byte(`{}`))
	require.NoError(t, err)
}
