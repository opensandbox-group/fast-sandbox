package ocitemplate

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestDecodeRequiresImmutablePair(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		name, root, snap string
		valid            bool
	}{
		{"digests", "r.example/ns/t@" + digest, "r.example/ns/t@" + digest, true},
		{"mutable root", "r.example/ns/t:rootfs", "r.example/ns/t@" + digest, false},
		{"missing snapfiles", "r.example/ns/t@" + digest, "", false},
		{"invalid digest", "r.example/ns/t@sha256:bad", "r.example/ns/t@" + digest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"oci": Pair{test.root, test.snap}})
			require.NoError(t, err)
			pair, err := Decode(raw)
			if test.valid {
				require.NoError(t, err)
				require.NotNil(t, pair)
			} else {
				require.Error(t, err)
			}
		})
	}
	pair, err := Decode([]byte(`{"files":{}}`))
	require.NoError(t, err)
	require.Nil(t, pair)
}

// Full registry round trip checks commit ordering, immutable refs and that
// resolving a template never fetches its rootfs or snapshot layer blobs.
func TestPublishResolveMetadataOnly(t *testing.T) {
	var blockReads atomic.Int32
	var published []string
	handler := registrytest.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/") && r.URL.Path != "" {
			// Layer digest is distinct from all JSON config blobs.
			if strings.HasSuffix(r.URL.Path, digest.FromString("layer").String()) {
				blockReads.Add(1)
			}
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			published = append(published, r.URL.Path)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	old := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = old }()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifest"), 0750))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "blobs/sha256"), 0750))
	config := content.NewDescriptorFromBytes(oci.MediaTypeImageConfig, []byte(`{}`))
	layer := content.NewDescriptorFromBytes(oci.MediaTypeImageLayer, []byte("layer"))
	for _, blob := range []struct {
		desc oci.Descriptor
		data []byte
	}{{config, []byte(`{}`)}, {layer, []byte("layer")}} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blobs/sha256", blob.desc.Digest.Encoded()), blob.data, 0640))
	}
	manifest := oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config, Layers: []oci.Descriptor{layer}}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/pair.json"), raw, 0640))
	index := map[string]any{"images": []map[string]string{{"name": "template:x_rootfs", "manifest_file": "pair.json", "manifest_digest": digest.FromBytes(raw).String()}, {"name": "template:x_snapfiles", "manifest_file": "pair.json", "manifest_digest": digest.FromBytes(raw).String()}}}
	raw, err = json.Marshal(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/index.json"), raw, 0640))
	dest := strings.TrimPrefix(server.URL, "https://") + "/ns/template:build"
	ref, sum, err := Push(context.Background(), dir, dest, []byte(`{"machine":{"vcpu":"1","memory":"512Mi"}}`), nil)
	require.NoError(t, err)
	require.Contains(t, ref, "@sha256:"+sum)
	require.Equal(t, []string{"/v2/ns/template/manifests/build_rootfs", "/v2/ns/template/manifests/build_snapfiles"}, published, "only two data images, with snapfiles last")
	repo, _, err := Repository(ref, nil)
	require.NoError(t, err)
	desc, err := repo.Resolve(context.Background(), "build_snapfiles")
	require.NoError(t, err)
	require.Equal(t, "sha256:"+sum, desc.Digest.String())
	snapRaw, err := fetchSmall(context.Background(), repo, desc)
	require.NoError(t, err)
	var snapManifest oci.Manifest
	require.NoError(t, json.Unmarshal(snapRaw, &snapManifest))
	require.Equal(t, "build", snapManifest.Annotations[AnnotationTemplateID])
	require.Len(t, snapManifest.Layers, 1)
	require.Equal(t, oci.MediaTypeImageConfig, snapManifest.Config.MediaType)
	for _, logical := range []string{dest, strings.TrimSuffix(dest, "build") + "build_snapfiles"} {
		_, err = resolveTestPair(t, context.Background(), logical, nil)
		require.NoError(t, err, "resolve paired tags without a third image")
	}
	cached, err := PushForCache(context.Background(), dir, dest, []byte(`{"machine":{"vcpu":"1","memory":"512Mi"}}`), nil)
	require.NoError(t, err)
	require.Equal(t, ref, cached.Reference)
	resolved, err := resolveTestPair(t, context.Background(), ref, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(resolved), string(cached.Metadata))
	require.NotEmpty(t, cached.Manifests)
	for reference, raw := range cached.Manifests {
		require.True(t, strings.HasSuffix(reference, "@"+digest.FromBytes(raw).String()))
	}
	// The same immutable publication can be retried, but not overwritten.
	repeated, _, err := Push(context.Background(), dir, dest, []byte(`{"machine":{"vcpu":"1","memory":"512Mi"}}`), nil)
	require.NoError(t, err)
	require.Equal(t, ref, repeated)
	_, _, err = Push(context.Background(), dir, dest, []byte(`{"platform":{"checkpoint":{"sandboxUID":"new-owner","namespace":"tenant"}}}`), nil)
	require.ErrorContains(t, err, "refusing to overwrite")

	// A separate build can publish changed content into the same repository
	// under its own UUID without altering the original immutable pair.
	nextDestination := strings.TrimSuffix(dest, ":build") + ":8a4fa491-2ea1-4c5f-9f26-682ed926dc22"
	nextRef, _, err := Push(context.Background(), dir, nextDestination, []byte(`{"machine":{"vcpu":"2"}}`), nil)
	require.NoError(t, err)
	require.NotEqual(t, ref, nextRef)
	oldMetadata, err := resolveTestPair(t, context.Background(), ref, nil)
	require.NoError(t, err)
	require.NotContains(t, string(oldMetadata), `"machine"`)
	newMetadata, err := resolveTestPair(t, context.Background(), nextRef, nil)
	require.NoError(t, err)
	require.NotContains(t, string(newMetadata), `"machine"`)

	payload, err := resolveTestPair(t, context.Background(), ref, nil)
	require.NoError(t, err)
	pair, err := Decode(payload)
	require.NoError(t, err)
	require.Contains(t, pair.Rootfs, "@sha256:")
	require.Zero(t, blockReads.Load(), "metadata resolution must not download data layers")
	// An incremental export carries its parent descriptor without a local
	// layer file. Copy it into another repository before publishing the pair.
	sources, err := json.Marshal(map[string]string{layer.Digest.String(): strings.Split(dest, ":build")[0]})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, RemoteSourcesName), sources, 0600))
	require.NoError(t, os.Remove(filepath.Join(dir, "blobs/sha256", layer.Digest.Encoded())))
	checkpoint := strings.TrimPrefix(server.URL, "https://") + "/snapshots/checkpoint:delta"
	checkpointRef, _, err := Push(context.Background(), dir, checkpoint, []byte(`{"machine":{"vcpu":"1"}}`), nil)
	require.NoError(t, err)
	_, err = resolveTestPair(t, context.Background(), checkpointRef, nil)
	require.NoError(t, err)
	copied, _, err := Repository(checkpointRef, nil)
	require.NoError(t, err)
	present, err := copied.Blobs().Exists(context.Background(), layer)
	require.NoError(t, err)
	require.True(t, present)
	// A parent blob missing in both repositories cannot be inherited.
	missing := content.NewDescriptorFromBytes(oci.MediaTypeImageLayer, []byte("missing parent"))
	err = inheritBlob(context.Background(), copied, missing, strings.TrimPrefix(server.URL, "https://")+"/absent/base", nil)
	require.Error(t, err)
	// A corrupted local blob must not publish a new template commit point.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blobs/sha256", layer.Digest.Encoded()), []byte("broken"), 0640))
	_, _, err = Push(context.Background(), dir, strings.TrimSuffix(dest, "build")+"broken", []byte(`{}`), nil)
	require.Error(t, err)
	repo, parsed, err := Repository(strings.TrimSuffix(dest, "build")+"broken", nil)
	require.NoError(t, err)
	_, err = repo.Resolve(context.Background(), parsed.Reference+"_snapfiles")
	require.Error(t, err)
}

// Resolving both images must share the repository's bearer token without
// downloading any config or layer blobs.
func TestResolveSharesRepositoryAuthAndValidatesPair(t *testing.T) {
	for _, mode := range []string{"valid", "missing-rootfs", "missing-snapfiles", "legacy-metadata"} {
		t.Run(mode, func(t *testing.T) {
			var tokens, blobReads atomic.Int32
			documents := map[string][]byte{}
			heads := map[string]int{}
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"token":"test-token","expires_in":300}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-token" {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:team/template:pull"`, server.URL))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if strings.Contains(r.URL.Path, "/blobs/") {
					blobReads.Add(1)
				}
				raw, ok := documents[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", oci.MediaTypeImageManifest)
				w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
				w.Header().Set("Docker-Content-Digest", digest.FromBytes(raw).String())
				if r.Method == http.MethodHead {
					heads[r.URL.Path]++
					return
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			old := retry.DefaultClient
			retry.DefaultClient = server.Client()
			defer func() { retry.DefaultClient = old }()
			repo := strings.TrimPrefix(server.URL, "https://") + "/team/template"
			config := content.NewDescriptorFromBytes(oci.MediaTypeImageConfig, []byte(`{}`))
			if mode == "legacy-metadata" {
				config.MediaType = "application/vnd.fast-sandbox.template.v1+json"
			}
			root, err := json.Marshal(oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config})
			require.NoError(t, err)
			snap, err := json.Marshal(oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config, Annotations: map[string]string{AnnotationTemplateID: "build"}})
			require.NoError(t, err)
			rootPath := "/v2/team/template/manifests/build_rootfs"
			snapSum := digest.FromBytes(snap).String()
			snapPath := "/v2/team/template/manifests/" + snapSum
			if mode != "missing-rootfs" {
				documents[rootPath] = root
			}
			if mode != "missing-snapfiles" {
				documents[snapPath] = snap
			}
			got, err := resolveTestPair(t, context.Background(), repo+"@"+snapSum, nil)
			switch mode {
			case "valid":
				require.NoError(t, err)
				pair, err := Decode(got)
				require.NoError(t, err)
				require.Equal(t, &Pair{Rootfs: repo + "@" + digest.FromBytes(root).String(), Snapfiles: repo + "@" + snapSum}, pair)
				require.EqualValues(t, 1, tokens.Load(), "both manifest reads must share one token cache")
				require.Equal(t, 1, heads[rootPath])
				require.Equal(t, 1, heads[snapPath])
			case "legacy-metadata":
				require.ErrorContains(t, err, "unexpected template config media type")
				require.Zero(t, heads[rootPath], "reject the old metadata image before resolving the pair")
			default:
				require.Error(t, err)
			}
			require.Zero(t, blobReads.Load(), "resolution must not fetch config or layer blobs")
		})
	}
}

func TestValidatePublishRepository(t *testing.T) {
	for _, reference := range []string{"oci://registry.example.com/team/templates", "oci://localhost:5000/templates", "oci://[::1]:5000/team/templates"} {
		require.NoError(t, ValidatePublishRepository(reference))
	}
	for _, reference := range []string{"registry.example.com/templates", "oci://registry.example.com/templates:tag", "oci://registry.example.com/templates@sha256:" + strings.Repeat("a", 64), "oci://registry.example.com/templates:", "oci://registry.example.com/templates@", "oci://registry.example.com/", "oci://registry.example.com", "oci://user:password@registry.example.com/templates", "oci://registry.example.com/UPPER"} {
		require.Error(t, ValidatePublishRepository(reference), reference)
	}
}

func resolveTestPair(t *testing.T, ctx context.Context, reference string, credentials CredentialFunc) ([]byte, error) {
	t.Helper()
	repo, ref, err := Repository(reference, credentials)
	if err != nil {
		return nil, err
	}
	return ResolveFrom(ctx, ref, repo)
}
