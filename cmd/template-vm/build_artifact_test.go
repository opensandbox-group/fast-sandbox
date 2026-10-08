package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"redis:6.2.1_rootfs":    "redis_6.2.1_rootfs",
		"redis:6.2.1_snapfiles": "redis_6.2.1_snapfiles",
		"6.2.1_obd":             "6.2.1_obd",
		"sha256:ab/cd":          "sha256_ab_cd",
		"already-safe_1.2":      "already-safe_1.2",
	}
	for input, want := range cases {
		if got := sanitizeFileName(input); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSourceManifestName(t *testing.T) {
	if got := sourceManifestName("6.2.1_obd"); got != "source-6.2.1_obd.json" {
		t.Errorf("tag-derived name = %q", got)
	}
	digestRef := "sha256:" + strings.Repeat("ab", 32)
	got := sourceManifestName(digestRef)
	if !strings.HasPrefix(got, "source-sha256_") || !strings.HasSuffix(got, ".json") {
		t.Errorf("digest-derived name = %q", got)
	}
}

func TestStagedArtifactLayout(t *testing.T) {
	outputDir := t.TempDir()
	staging, final, err := createStagedArtifactLayout(outputDir, "redis_7.2.4")
	if err != nil {
		t.Fatal(err)
	}
	if staging.root == final.root || filepath.Dir(staging.root) != outputDir {
		t.Fatalf("staging=%q final=%q", staging.root, final.root)
	}
	if err := os.WriteFile(staging.manifestPath("source.json"), []byte("manifest"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final.root); !os.IsNotExist(err) {
		t.Fatalf("final artifact exists before publish: %v", err)
	}
	if err := publishArtifactLayout(staging, final); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final.manifestPath("source.json")); err != nil {
		t.Fatalf("published file: %v", err)
	}
	if _, _, err := createStagedArtifactLayout(outputDir, "redis_7.2.4"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing artifact must not be replaced: %v", err)
	}

	staging, final, err = createStagedArtifactLayout(outputDir, "redis_7.2.5")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging.root) })
	if err := os.Mkdir(final.root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := publishArtifactLayout(staging, final); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("publish race must not replace destination: %v", err)
	}
	if _, err := os.Stat(staging.root); err != nil {
		t.Fatalf("failed publish removed staging artifact: %v", err)
	}
}

func TestInstallBytes(t *testing.T) {
	layout := artifactLayout{root: t.TempDir()}
	payload := []byte(`{"architecture":"amd64","os":"linux"}`)

	desc, err := layout.installBytes(payload, ocispec.MediaTypeImageConfig)
	if err != nil {
		t.Fatalf("installBytes: %v", err)
	}
	if desc.MediaType != ocispec.MediaTypeImageConfig {
		t.Errorf("mediaType = %q", desc.MediaType)
	}
	if desc.Digest != sha256Digest(payload) || desc.Size != int64(len(payload)) {
		t.Errorf("descriptor = %s/%d", desc.Digest, desc.Size)
	}
	stored, err := os.ReadFile(layout.blobPath(desc.Digest))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Errorf("stored blob mismatch: %v", err)
	}

	// Re-installing the same payload reuses the blob and stays idempotent.
	again, err := layout.installBytes(payload, ocispec.MediaTypeImageConfig)
	if err != nil || again.Digest != desc.Digest || again.Size != desc.Size || again.MediaType != desc.MediaType {
		t.Errorf("idempotent install: %+v vs %+v, %v", again, desc, err)
	}

	corrupt := bytes.Repeat([]byte("x"), len(payload))
	if err := os.WriteFile(layout.blobPath(desc.Digest), corrupt, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.installBytes(payload, ocispec.MediaTypeImageConfig); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("same-size corrupted blob must fail: %v", err)
	}
}

func TestInstallFile(t *testing.T) {
	layout := artifactLayout{root: t.TempDir()}
	source := filepath.Join(t.TempDir(), "committed.layer")
	payload := bytes.Repeat([]byte("lsmt"), 4096)
	if err := os.WriteFile(source, payload, 0o640); err != nil {
		t.Fatal(err)
	}

	annotations := map[string]string{overlaybdBlobDigestAnnot: "placeholder"}
	desc, err := layout.installFile(source, overlaybdPlainLayerMediaType, annotations)
	if err != nil {
		t.Fatalf("installFile: %v", err)
	}
	if desc.Digest != sha256Digest(payload) || desc.Size != int64(len(payload)) {
		t.Errorf("descriptor = %s/%d, want content digest", desc.Digest, desc.Size)
	}
	if desc.Annotations[overlaybdBlobDigestAnnot] != "placeholder" {
		t.Errorf("annotations not propagated: %+v", desc.Annotations)
	}
	stored, err := os.ReadFile(layout.blobPath(desc.Digest))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Errorf("stored blob mismatch: %v", err)
	}
}

func TestImportVerifiedFile(t *testing.T) {
	layout := artifactLayout{root: t.TempDir()}
	source := filepath.Join(t.TempDir(), "overlaybd.commit")
	payload := []byte("converted native layer")
	if err := os.WriteFile(source, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	desc := ocispec.Descriptor{
		MediaType: overlaybdPlainLayerMediaType,
		Digest:    sha256Digest(payload),
		Size:      int64(len(payload)),
	}
	if err := layout.importVerifiedFile(source, desc); err != nil {
		t.Fatalf("importVerifiedFile: %v", err)
	}
	if err := layout.checkBlob(desc); err != nil {
		t.Fatalf("imported blob: %v", err)
	}

	bad := desc
	bad.Size++
	if err := layout.importVerifiedFile(source, bad); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("size mismatch error = %v", err)
	}
}

func TestMinimalImageConfig(t *testing.T) {
	payload := minimalImageConfig()
	var parsed struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Rootfs       struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("config must be valid JSON: %v", err)
	}
	if parsed.Architecture != runtime.GOARCH || parsed.OS != "linux" {
		t.Errorf("platform = %s/%s", parsed.OS, parsed.Architecture)
	}
	if parsed.Rootfs.Type != "layers" || len(parsed.Rootfs.DiffIDs) != 0 {
		t.Errorf("rootfs = %+v; block-device layers carry no diff_ids", parsed.Rootfs)
	}
}

func TestAssembleManifest(t *testing.T) {
	layout := artifactLayout{root: t.TempDir()}
	configDesc, err := layout.installBytes(minimalImageConfig(), ocispec.MediaTypeImageConfig)
	if err != nil {
		t.Fatal(err)
	}
	layers := []ocispec.Descriptor{
		{MediaType: overlaybdZfileLayerMediaType, Digest: sha256Digest([]byte("l1")), Size: 2},
		{MediaType: overlaybdZfileLayerMediaType, Digest: sha256Digest([]byte("l2")), Size: 2},
	}

	payload, desc, err := assembleManifest(configDesc, layers)
	if err != nil {
		t.Fatalf("assembleManifest: %v", err)
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		t.Errorf("manifest mediaType = %q", desc.MediaType)
	}
	if desc.Digest != sha256Digest(payload) || desc.Size != int64(len(payload)) {
		t.Errorf("manifest descriptor does not match payload: %s/%d", desc.Digest, desc.Size)
	}

	var parsed ocispec.Manifest
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if parsed.SchemaVersion != 2 || parsed.MediaType != ocispec.MediaTypeImageManifest {
		t.Errorf("schemaVersion/mediaType = %d/%q", parsed.SchemaVersion, parsed.MediaType)
	}
	if len(parsed.Layers) != 2 || parsed.Layers[0].Digest != layers[0].Digest || parsed.Layers[1].Digest != layers[1].Digest {
		t.Errorf("layer order not preserved: %+v", parsed.Layers)
	}
	if parsed.Config.Digest != configDesc.Digest {
		t.Errorf("config descriptor = %s", parsed.Config.Digest)
	}
}

func TestWriteLayoutIndex(t *testing.T) {
	layout := artifactLayout{root: t.TempDir()}
	if err := layout.ensure(); err != nil {
		t.Fatal(err)
	}
	index := layoutIndex{
		SourceRef: "registry.hub.docker.com/overlaybd/redis:6.2.1_obd",
		SourceManifest: layoutIndexEntry{
			Name:           "registry.hub.docker.com/overlaybd/redis:6.2.1_obd",
			ManifestFile:   "source-6.2.1_obd.json",
			ManifestDigest: "sha256:" + strings.Repeat("11", 32),
		},
		Images: []layoutIndexEntry{
			{Name: "redis:6.2.1_rootfs", ManifestFile: "redis_6.2.1_rootfs.json", ManifestDigest: "sha256:" + strings.Repeat("22", 32)},
			{Name: "redis:6.2.1_snapfiles", ManifestFile: "redis_6.2.1_snapfiles.json", ManifestDigest: "sha256:" + strings.Repeat("33", 32)},
		},
	}
	if err := layout.writeLayoutIndex(index); err != nil {
		t.Fatalf("writeLayoutIndex: %v", err)
	}

	payload, err := os.ReadFile(filepath.Join(layout.manifestDir(), layoutIndexName))
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var parsed layoutIndex
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("index.json must be valid JSON: %v", err)
	}
	if parsed.SchemaVersion != layoutIndexSchemaVersion {
		t.Errorf("schema_version = %d, want %d", parsed.SchemaVersion, layoutIndexSchemaVersion)
	}
	if parsed.SourceRef != index.SourceRef || len(parsed.Images) != 2 {
		t.Errorf("index round-trip mismatch: %+v", parsed)
	}
	if parsed.Images[0].ManifestFile != "redis_6.2.1_rootfs.json" ||
		parsed.Images[1].ManifestDigest != "sha256:"+strings.Repeat("33", 32) {
		t.Errorf("image entries mismatch: %+v", parsed.Images)
	}
}

// installValidPair installs a config blob and one layer blob, returning the
// assembled manifest referencing both.
func installValidPair(t *testing.T, layout artifactLayout) []byte {
	t.Helper()
	configDesc, err := layout.installBytes(minimalImageConfig(), ocispec.MediaTypeImageConfig)
	if err != nil {
		t.Fatal(err)
	}
	layerDesc, err := layout.installBytes([]byte("sealed layer bytes"), overlaybdZfileLayerMediaType)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := assembleManifest(configDesc, []ocispec.Descriptor{layerDesc})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestValidateLayout(t *testing.T) {
	layout := artifactLayout{root: t.TempDir()}
	manifest := installValidPair(t, layout)
	if err := layout.validateLayout(manifest); err != nil {
		t.Fatalf("complete layout must validate: %v", err)
	}

	// Parse the manifest back to locate the layer blob for tampering.
	var parsed ocispec.Manifest
	if err := json.Unmarshal(manifest, &parsed); err != nil {
		t.Fatal(err)
	}
	layerPath := layout.blobPath(parsed.Layers[0].Digest)

	// Missing blob.
	if err := os.Remove(layerPath); err != nil {
		t.Fatal(err)
	}
	if err := layout.validateLayout(manifest); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing blob must fail: %v", err)
	}

	// Size mismatch (shorter than recorded).
	if err := os.WriteFile(layerPath, []byte("xy"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := layout.validateLayout(manifest); err == nil || !strings.Contains(err.Error(), "size") {
		t.Errorf("size mismatch must fail: %v", err)
	}

	// Same length, corrupted content: digest mismatch.
	corrupt := bytes.Repeat([]byte("z"), len("sealed layer bytes"))
	if err := os.WriteFile(layerPath, corrupt, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := layout.validateLayout(manifest); err == nil || !strings.Contains(err.Error(), "content digest") {
		t.Errorf("digest mismatch must fail: %v", err)
	}

	// Manifest JSON that cannot be parsed fails fast.
	if err := layout.validateLayout([]byte("{not json")); err == nil {
		t.Error("unparseable manifest must fail")
	}
}
