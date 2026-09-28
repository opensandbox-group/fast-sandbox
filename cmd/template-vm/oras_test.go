package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// sha256Digest returns the sha256 digest of payload for test descriptors.
func sha256Digest(payload []byte) digest.Digest {
	sum := sha256.Sum256(payload)
	return digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:]))
}

// testLayer builds a layer descriptor whose digest matches a deterministic
// payload derived from the mediaType.
func testLayer(mediaType string, annotations map[string]string) ocispec.Descriptor {
	payload := []byte("blob for " + mediaType)
	return ocispec.Descriptor{
		MediaType:   mediaType,
		Digest:      sha256Digest(payload),
		Size:        int64(len(payload)),
		Annotations: annotations,
	}
}

// pushBytes stores payload in the in-memory store and returns its descriptor.
func pushBytes(t *testing.T, store *memory.Store, mediaType string, payload []byte) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: sha256Digest(payload), Size: int64(len(payload))}
	if err := store.Push(context.Background(), desc, bytes.NewReader(payload)); err != nil {
		t.Fatalf("push %s: %v", desc.Digest, err)
	}
	return desc
}

func TestCanonicalRegistryHost(t *testing.T) {
	for _, alias := range []string{"docker.io", "index.docker.io", "registry.hub.docker.com", "registry-1.docker.io"} {
		if got := canonicalRegistryHost(alias); got != "registry-1.docker.io" {
			t.Errorf("canonicalRegistryHost(%q) = %q, want registry-1.docker.io", alias, got)
		}
	}
	if got := canonicalRegistryHost("ghcr.io"); got != "ghcr.io" {
		t.Errorf("canonicalRegistryHost(ghcr.io) = %q", got)
	}
}

func TestRepoBlobURL(t *testing.T) {
	ref := registry.Reference{Registry: "registry.hub.docker.com", Repository: "overlaybd/redis", Reference: "6.2.1_obd"}
	if got := repoBlobURL(ref, false); got != "https://registry-1.docker.io/v2/overlaybd/redis/blobs" {
		t.Errorf("https repoBlobURL = %q", got)
	}
	if got := repoBlobURL(ref, true); got != "http://registry-1.docker.io/v2/overlaybd/redis/blobs" {
		t.Errorf("plain-http repoBlobURL = %q", got)
	}
}

func TestParseRegistryReference(t *testing.T) {
	ref, err := parseRegistryReference("registry.hub.docker.com/overlaybd/redis:6.2.1_obd")
	if err != nil {
		t.Fatalf("parse tag reference: %v", err)
	}
	if ref.Registry != "registry-1.docker.io" || ref.Repository != "overlaybd/redis" || ref.Reference != "6.2.1_obd" {
		t.Errorf("unexpected reference: %+v", ref)
	}

	digestRef := "ghcr.io/acme/app@sha256:" + strings.Repeat("ab", 32)
	ref, err = parseRegistryReference(digestRef)
	if err != nil {
		t.Fatalf("parse digest reference: %v", err)
	}
	if ref.Reference != "sha256:"+strings.Repeat("ab", 32) {
		t.Errorf("digest reference = %q", ref.Reference)
	}

	if _, err := parseRegistryReference("redis"); err == nil {
		t.Error("bare name without registry must fail")
	}
	if _, err := parseRegistryReference("ghcr.io/acme/app"); err == nil {
		t.Error("reference without tag or digest must fail")
	}
}

func TestAuthEntryCredential(t *testing.T) {
	credential, err := authEntry{Username: "alice", Password: "s3cret"}.credential()
	if err != nil || credential.Username != "alice" || credential.Password != "s3cret" {
		t.Errorf("username/password entry: %+v, %v", credential, err)
	}

	token := base64.StdEncoding.EncodeToString([]byte("bob:pw"))
	credential, err = authEntry{Auth: token}.credential()
	if err != nil || credential.Username != "bob" || credential.Password != "pw" {
		t.Errorf("base64 auth entry: %+v, %v", credential, err)
	}

	credential, err = authEntry{}.credential()
	if err != nil || credential != (auth.EmptyCredential) {
		t.Errorf("empty entry should yield EmptyCredential: %+v, %v", credential, err)
	}

	if _, err := (authEntry{Auth: "!!not-base64!!"}).credential(); err == nil {
		t.Error("invalid base64 must fail")
	}
	noColon := base64.StdEncoding.EncodeToString([]byte("nocolon"))
	if _, err := (authEntry{Auth: noColon}).credential(); err == nil {
		t.Error("token without ':' must fail")
	}
}

func TestCredentialFor(t *testing.T) {
	ref := registry.Reference{Registry: "registry-1.docker.io", Repository: "overlaybd/redis", Reference: "tag"}

	// The standard docker-login key for Docker Hub matches a Hub reference.
	config := dockerConfig{Auths: map[string]authEntry{
		"https://index.docker.io/v1/": {Username: "hub", Password: "pw"},
	}}
	credential, found, err := credentialFor(config, ref)
	if err != nil || !found || credential.Username != "hub" {
		t.Errorf("docker hub login key: %+v found=%v err=%v", credential, found, err)
	}

	// Longest repository prefix wins over the bare registry entry.
	config = dockerConfig{Auths: map[string]authEntry{
		"ghcr.io":        {Username: "org", Password: "pw"},
		"ghcr.io/acme":   {Username: "acme", Password: "pw"},
		"quay.io/ignore": {Username: "no", Password: "pw"},
	}}
	ghcrRef := registry.Reference{Registry: "ghcr.io", Repository: "acme/app", Reference: "tag"}
	credential, found, err = credentialFor(config, ghcrRef)
	if err != nil || !found || credential.Username != "acme" {
		t.Errorf("longest prefix: %+v found=%v err=%v", credential, found, err)
	}

	// Registry-level entry still matches a sibling repository.
	credential, found, err = credentialFor(config, registry.Reference{Registry: "ghcr.io", Repository: "other/app", Reference: "tag"})
	if err != nil || !found || credential.Username != "org" {
		t.Errorf("registry-level fallback: %+v found=%v err=%v", credential, found, err)
	}

	// No matching entry resolves to anonymous access.
	credential, found, err = credentialFor(config, ref)
	if err != nil || found || credential != auth.EmptyCredential {
		t.Errorf("anonymous: %+v found=%v err=%v", credential, found, err)
	}
}

func TestSelectPlatformManifest(t *testing.T) {
	matching := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    sha256Digest([]byte("linux")),
		Size:      5,
		Platform:  &ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH},
	}
	index := ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: ocispec.MediaTypeImageManifest, Digest: sha256Digest([]byte("win")), Size: 3,
			Platform: &ocispec.Platform{OS: "windows", Architecture: runtime.GOARCH}},
		matching,
	}}
	selected, err := selectPlatformManifest(index)
	if err != nil || selected.Digest != matching.Digest {
		t.Errorf("select linux/%s: %v, %v", runtime.GOARCH, selected.Digest, err)
	}

	index.Manifests = index.Manifests[:1]
	if _, err := selectPlatformManifest(index); err == nil || !strings.Contains(err.Error(), "windows") {
		t.Errorf("missing platform error should list available platforms: %v", err)
	}
}

func TestFetchManifestJSONDirectAndIndex(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// A tag pointing straight at a manifest is returned unchanged.
	manifestPayload := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `"}`)
	manifestDesc := pushBytes(t, store, ocispec.MediaTypeImageManifest, manifestPayload)
	if err := store.Tag(ctx, manifestDesc, "direct"); err != nil {
		t.Fatalf("tag direct: %v", err)
	}
	desc, raw, err := fetchManifestJSON(ctx, store, "direct")
	if err != nil {
		t.Fatalf("fetch direct manifest: %v", err)
	}
	if desc.Digest != manifestDesc.Digest || !bytes.Equal(raw, manifestPayload) {
		t.Errorf("direct manifest round-trip: %s", desc.Digest)
	}

	// A tag pointing at an index traverses to the linux/<arch> child manifest.
	childPayload := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `","child":true}`)
	childDesc := pushBytes(t, store, ocispec.MediaTypeImageManifest, childPayload)
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageManifest, Digest: childDesc.Digest, Size: childDesc.Size, Platform: &ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}}},
	}
	indexPayload, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	indexDesc := pushBytes(t, store, ocispec.MediaTypeImageIndex, indexPayload)
	if err := store.Tag(ctx, indexDesc, "indexed"); err != nil {
		t.Fatalf("tag indexed: %v", err)
	}
	desc, raw, err = fetchManifestJSON(ctx, store, "indexed")
	if err != nil {
		t.Fatalf("fetch through index: %v", err)
	}
	if desc.Digest != childDesc.Digest || !bytes.Equal(raw, childPayload) {
		t.Errorf("index traversal: got %s, want child %s", desc.Digest, childDesc.Digest)
	}
}

func TestResolveBuildSourceSelectsPlatformBeforeClassification(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	config := testLayer(ocispec.MediaTypeImageConfig, nil)

	selectedManifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{testLayer(ocispec.MediaTypeImageLayerGzip, nil)},
	}
	selectedRaw, err := json.Marshal(selectedManifest)
	if err != nil {
		t.Fatal(err)
	}
	selectedDesc := pushBytes(t, store, ocispec.MediaTypeImageManifest, selectedRaw)

	otherManifest := selectedManifest
	otherManifest.Layers = []ocispec.Descriptor{testLayer(overlaybdZfileLayerMediaType, nil)}
	otherRaw, err := json.Marshal(otherManifest)
	if err != nil {
		t.Fatal(err)
	}
	otherDesc := pushBytes(t, store, ocispec.MediaTypeImageManifest, otherRaw)
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: otherDesc.Digest, Size: otherDesc.Size, Platform: &ocispec.Platform{OS: "linux", Architecture: "not-" + runtime.GOARCH}},
			{MediaType: ocispec.MediaTypeImageManifest, Digest: selectedDesc.Digest, Size: selectedDesc.Size, Platform: &ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}},
		},
	}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	indexDesc := pushBytes(t, store, ocispec.MediaTypeImageIndex, indexRaw)
	if err := store.Tag(ctx, indexDesc, "latest"); err != nil {
		t.Fatal(err)
	}

	ref := registry.Reference{Registry: "registry.example.com", Repository: "team/app", Reference: "latest"}
	resolved, err := resolveBuildSource(ctx, store, ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ManifestDesc.Digest != selectedDesc.Digest {
		t.Fatalf("selected manifest = %s, want %s", resolved.ManifestDesc.Digest, selectedDesc.Digest)
	}
	kind, err := classifyBuildSource(resolved.Manifest, ref.String())
	if err != nil || kind != buildSourceOCI {
		t.Fatalf("selected source kind = %d, %v", kind, err)
	}
}

func TestClassifyBuildSource(t *testing.T) {
	native := testLayer(overlaybdZfileLayerMediaType, nil)
	standard := testLayer(ocispec.MediaTypeImageLayerGzip, nil)

	kind, err := classifyBuildSource(ocispec.Manifest{Layers: []ocispec.Descriptor{native}}, "img")
	if err != nil || kind != buildSourceNative {
		t.Fatalf("native classification = %d, %v", kind, err)
	}
	kind, err = classifyBuildSource(ocispec.Manifest{Layers: []ocispec.Descriptor{standard}}, "img")
	if err != nil || kind != buildSourceOCI {
		t.Fatalf("OCI classification = %d, %v", kind, err)
	}
	if _, err := classifyBuildSource(ocispec.Manifest{Layers: []ocispec.Descriptor{native, standard}}, "img"); err == nil || !strings.Contains(err.Error(), "mixes") {
		t.Fatalf("mixed source error = %v", err)
	}
	versionedTar := standard
	versionedTar.Annotations = map[string]string{overlaybdVersionAnnotation: "0.6.0"}
	if _, err := classifyBuildSource(ocispec.Manifest{Layers: []ocispec.Descriptor{versionedTar}}, "img"); err == nil || !strings.Contains(err.Error(), "tar-wrapped") {
		t.Fatalf("versioned tar error = %v", err)
	}
}

func TestValidateManifestDescriptors(t *testing.T) {
	validConfig := testLayer(ocispec.MediaTypeImageConfig, nil)
	validLayer := testLayer(overlaybdZfileLayerMediaType, nil)
	if err := validateManifestDescriptors(ocispec.Manifest{Config: validConfig, Layers: []ocispec.Descriptor{validLayer}}, "img"); err != nil {
		t.Fatal(err)
	}

	invalidLayer := validLayer
	invalidLayer.Digest = digest.Digest("sha256:../escape")
	if err := validateManifestDescriptors(ocispec.Manifest{Config: validConfig, Layers: []ocispec.Descriptor{invalidLayer}}, "img"); err == nil || !strings.Contains(err.Error(), "invalid digest") {
		t.Fatalf("malformed layer digest error = %v", err)
	}

	negativeConfig := validConfig
	negativeConfig.Size = -1
	if err := validateManifestDescriptors(ocispec.Manifest{Config: negativeConfig, Layers: []ocispec.Descriptor{validLayer}}, "img"); err == nil || !strings.Contains(err.Error(), "negative size") {
		t.Fatalf("negative config size error = %v", err)
	}
}

func TestValidateOverlayBDSource(t *testing.T) {
	zfileLayer := testLayer(overlaybdZfileLayerMediaType, nil)
	lsmtLayer := testLayer(overlaybdPlainLayerMediaType, nil)

	manifest := ocispec.Manifest{Layers: []ocispec.Descriptor{zfileLayer, lsmtLayer}}
	if err := validateOverlayBDSource(manifest, "img"); err != nil {
		t.Errorf("native overlaybd layers must pass: %v", err)
	}

	// mediaType-lying: tar mediaType with the blob-digest annotation equal to
	// the layer digest is a sealed overlaybd blob.
	lying := testLayer(ocispec.MediaTypeImageLayerGzip, nil)
	lying.Annotations = map[string]string{overlaybdBlobDigestAnnot: lying.Digest.String()}
	if err := validateOverlayBDSource(ocispec.Manifest{Layers: []ocispec.Descriptor{lying}}, "img"); err != nil {
		t.Errorf("mediaType-lying native layer must pass: %v", err)
	}

	cases := []struct {
		name     string
		manifest ocispec.Manifest
		want     string
	}{
		{
			name:     "turbo artifactType",
			manifest: ocispec.Manifest{ArtifactType: turboArtifactType, Layers: []ocispec.Descriptor{zfileLayer}},
			want:     "turbo-OCI",
		},
		{
			name:     "no layers",
			manifest: ocispec.Manifest{},
			want:     "no layers",
		},
		{
			name: "turbo annotation",
			manifest: ocispec.Manifest{Layers: []ocispec.Descriptor{
				testLayer(ocispec.MediaTypeImageLayerGzip, map[string]string{turboAnnotationPrefix + "foo": "bar"}),
			}},
			want: "turbo-OCI",
		},
		{
			name: "turbo version annotation",
			manifest: ocispec.Manifest{Layers: []ocispec.Descriptor{
				testLayer(ocispec.MediaTypeImageLayerGzip, map[string]string{overlaybdVersionAnnotation: "0.6.0 turbo"}),
			}},
			want: "turbo-OCI",
		},
		{
			name: "tar-wrapped overlaybd",
			manifest: ocispec.Manifest{Layers: []ocispec.Descriptor{
				testLayer(ocispec.MediaTypeImageLayerGzip, map[string]string{overlaybdBlobDigestAnnot: "sha256:" + strings.Repeat("00", 32)}),
			}},
			want: "tar-wrapped",
		},
		{
			name: "standard tar",
			manifest: ocispec.Manifest{Layers: []ocispec.Descriptor{
				testLayer(ocispec.MediaTypeImageLayerGzip, nil),
			}},
			want: "standard OCI tar layer",
		},
		{
			name: "unknown mediaType",
			manifest: ocispec.Manifest{Layers: []ocispec.Descriptor{
				testLayer("application/vnd.example.mystery", nil),
			}},
			want: "unsupported mediaType",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOverlayBDSource(tc.manifest, "img")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

type invalidManifestSource struct {
	fetchCalled bool
}

func (s *invalidManifestSource) Resolve(context.Context, string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest("sha256:../escape"), Size: 1}, nil
}

func (s *invalidManifestSource) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	s.fetchCalled = true
	return io.NopCloser(strings.NewReader("x")), nil
}

func TestFetchManifestJSONRejectsMalformedDigestBeforeFetch(t *testing.T) {
	source := &invalidManifestSource{}
	if _, _, err := fetchManifestJSON(context.Background(), source, "latest"); err == nil || !strings.Contains(err.Error(), "invalid digest") {
		t.Fatalf("malformed manifest digest error = %v", err)
	}
	if source.fetchCalled {
		t.Fatal("malformed manifest descriptor reached Fetch")
	}
}

// staticFetcher serves fixed bytes regardless of the descriptor, used to
// exercise downloadBlob's verification failures.
type staticFetcher struct {
	payload  []byte
	fetchErr error
}

func (f staticFetcher) Fetch(_ context.Context, _ ocispec.Descriptor) (io.ReadCloser, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return io.NopCloser(bytes.NewReader(f.payload)), nil
}

func TestDownloadBlob(t *testing.T) {
	ctx := context.Background()
	blobDir := filepath.Join(t.TempDir(), "blobs", "sha256")

	payload := []byte("sealed overlaybd lower payload")
	desc := ocispec.Descriptor{MediaType: overlaybdZfileLayerMediaType, Digest: sha256Digest(payload), Size: int64(len(payload))}

	path, err := downloadBlob(ctx, staticFetcher{payload: payload}, desc, blobDir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if filepath.Base(path) != desc.Digest.Encoded() {
		t.Errorf("blob file name = %q, want digest-encoded", path)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, payload) {
		t.Errorf("stored payload mismatch: %v", err)
	}

	// Second download reuses the existing blob without fetching again.
	pathAgain, err := downloadBlob(ctx, staticFetcher{fetchErr: os.ErrNotExist}, desc, blobDir)
	if err != nil || pathAgain != path {
		t.Errorf("idempotent reuse: %q, %v", pathAgain, err)
	}

	// A truncated download fails the size check.
	if _, err := downloadBlob(ctx, staticFetcher{payload: payload[:4]}, desc, blobDir+"-size"); err == nil ||
		!strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("size mismatch must fail: %v", err)
	}

	// Corrupted content of the right length fails the digest check.
	corrupt := bytes.Clone(payload)
	corrupt[0] ^= 0xff
	if _, err := downloadBlob(ctx, staticFetcher{payload: corrupt}, desc, blobDir+"-digest"); err == nil ||
		!strings.Contains(err.Error(), "digest verification failed") {
		t.Errorf("digest mismatch must fail: %v", err)
	}

	// Unsupported and malformed digests are rejected before building a path.
	bad := ocispec.Descriptor{MediaType: overlaybdZfileLayerMediaType, Digest: digest.Digest("sha512:" + strings.Repeat("cd", 64)), Size: 1}
	if _, err := downloadBlob(ctx, staticFetcher{payload: payload}, bad, blobDir+"-algo"); err == nil ||
		!strings.Contains(err.Error(), "unsupported digest algorithm") {
		t.Errorf("sha512 digest must fail: %v", err)
	}
	malformed := ocispec.Descriptor{MediaType: overlaybdZfileLayerMediaType, Digest: digest.Digest("sha256:../escape"), Size: 1}
	if _, err := downloadBlob(ctx, staticFetcher{payload: payload}, malformed, blobDir+"-malformed"); err == nil ||
		!strings.Contains(err.Error(), "invalid digest") {
		t.Errorf("malformed digest must fail: %v", err)
	}

	// Stale files are fully verified before reuse.
	staleDir := blobDir + "-stale"
	if err := os.MkdirAll(staleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, desc.Digest.Encoded()), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadBlob(ctx, staticFetcher{payload: payload}, desc, staleDir); err == nil ||
		!strings.Contains(err.Error(), "size") {
		t.Errorf("stale size must fail: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, desc.Digest.Encoded()), bytes.Repeat([]byte("x"), len(payload)), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadBlob(ctx, staticFetcher{payload: payload}, desc, staleDir); err == nil ||
		!strings.Contains(err.Error(), "digest") {
		t.Errorf("same-size stale content must fail: %v", err)
	}
}

func TestFetchSourceImage(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	layout := artifactLayout{root: t.TempDir()}

	layerPayloads := [][]byte{[]byte("bottom layer"), []byte("middle layer"), []byte("top layer")}
	layers := make([]ocispec.Descriptor, 0, len(layerPayloads))
	for _, payload := range layerPayloads {
		layers = append(layers, pushBytes(t, store, overlaybdZfileLayerMediaType, payload))
	}
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    pushBytes(t, store, ocispec.MediaTypeImageConfig, []byte(`{"architecture":"amd64"}`)),
		Layers:    layers,
	}
	manifestPayload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDesc := pushBytes(t, store, ocispec.MediaTypeImageManifest, manifestPayload)
	if err := store.Tag(ctx, manifestDesc, "6.2.1_obd"); err != nil {
		t.Fatalf("tag: %v", err)
	}

	ref := registry.Reference{Registry: "registry-1.docker.io", Repository: "overlaybd/redis", Reference: "6.2.1_obd"}
	resolved, err := resolveBuildSource(ctx, store, ref)
	if err != nil {
		t.Fatalf("resolveBuildSource: %v", err)
	}
	fetched, err := fetchSourceImage(ctx, store, resolved, layout)
	if err != nil {
		t.Fatalf("fetchSourceImage: %v", err)
	}

	// The source manifest lands under manifest/source-<tag>.json unchanged.
	wantManifestPath := layout.manifestPath("source-6.2.1_obd.json")
	if fetched.ManifestPath != wantManifestPath {
		t.Errorf("manifest path = %q, want %q", fetched.ManifestPath, wantManifestPath)
	}
	storedManifest, err := os.ReadFile(wantManifestPath)
	if err != nil || !bytes.Equal(storedManifest, manifestPayload) {
		t.Errorf("stored manifest mismatch: %v", err)
	}

	if err := layout.checkBlob(manifest.Config); err != nil {
		t.Errorf("source config blob: %v", err)
	}

	// Layer order and on-disk blob content are preserved.
	if len(fetched.Layers) != len(layers) {
		t.Fatalf("layer count = %d, want %d", len(fetched.Layers), len(layers))
	}
	for index, layer := range layers {
		if fetched.Layers[index].Digest != layer.Digest {
			t.Errorf("layer %d order changed: %s", index, fetched.Layers[index].Digest)
		}
		stored, err := os.ReadFile(layout.blobPath(layer.Digest))
		if err != nil || !bytes.Equal(stored, layerPayloads[index]) {
			t.Errorf("layer %d blob mismatch: %v", index, err)
		}
	}

	// localLowers addresses the downloaded blob files, never the network.
	lowers := localLowers(fetched)
	if len(lowers) != len(layers) {
		t.Fatalf("lower count = %d", len(lowers))
	}
	for index, lower := range lowers {
		if lower.File != layout.blobPath(layers[index].Digest) {
			t.Errorf("lower %d file = %q", index, lower.File)
		}
		if lower.Digest != "" || lower.Dir != "" {
			t.Errorf("build-time lower must be file-only: %+v", lower)
		}
	}
}
