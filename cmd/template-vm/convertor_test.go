package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry"
)

type recordingConvertorRunner struct {
	binary string
	args   []string
	output []byte
	err    error
}

func (r *recordingConvertorRunner) Run(_ context.Context, binary string, args []string) ([]byte, error) {
	r.binary = binary
	r.args = append([]string{}, args...)
	return r.output, r.err
}

func testResolvedOCISource() resolvedBuildSource {
	layer := testLayer(ocispec.MediaTypeImageLayerGzip, nil)
	return resolvedBuildSource{
		Ref: registry.Reference{
			Registry:   "registry.example.com",
			Repository: "team/app",
			Reference:  "latest",
		},
		ManifestDesc: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    sha256Digest([]byte("source manifest")),
			Size:      int64(len("source manifest")),
		},
		Manifest: ocispec.Manifest{Layers: []ocispec.Descriptor{layer}},
	}
}

func TestBuildConvertorArgs(t *testing.T) {
	source := testResolvedOCISource()
	got := buildConvertorArgs(source, "/work/converted", 30, "alice:secret:part", true)
	want := []string{
		"--repository", "registry.example.com/team/app",
		"--input-digest", source.ManifestDesc.Digest.String(),
		"--overlaybd", "template-vm-local",
		"--oci",
		"--mkfs=true",
		"--vsize", "30",
		"--no-upload",
		"--dump-manifest",
		"--reserve",
		"--dir", "/work/converted",
		"--username", "alice:secret:part",
		"--plain",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %q\nwant      = %q", got, want)
	}
	anonymous := buildConvertorArgs(source, "/work/converted", 20, "", false)
	if strings.Contains(strings.Join(anonymous, " "), "--username") || strings.Contains(strings.Join(anonymous, " "), "--plain") {
		t.Fatalf("anonymous arguments = %q", anonymous)
	}
}

func TestConvertorCredential(t *testing.T) {
	source := testResolvedOCISource()
	config := dockerConfig{Auths: map[string]authEntry{
		"registry.example.com":      {Username: "registry", Password: "fallback"},
		"registry.example.com/team": {Username: "alice", Password: "secret:part"},
	}}
	got, err := convertorCredential(config, source)
	if err != nil || got != "alice:secret:part" {
		t.Fatalf("credential = %q, %v", got, err)
	}
	got, err = convertorCredential(dockerConfig{Auths: map[string]authEntry{}}, source)
	if err != nil || got != "" {
		t.Fatalf("anonymous credential = %q, %v", got, err)
	}
}

func TestRunOCIConvertorRedactsCredential(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "convertor")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := global.convertor
	global.convertor = binary
	t.Cleanup(func() { global.convertor = old })

	runner := &recordingConvertorRunner{
		output: []byte("authentication failed for alice:secret and secret"),
		err:    errors.New("exit status 1"),
	}
	err := runOCIConvertor(context.Background(), testResolvedOCISource(), dir, 20, "alice:secret", false, runner)
	if err == nil {
		t.Fatal("converter failure must propagate")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("credential leaked in error: %v", err)
	}
	if runner.binary != binary {
		t.Errorf("binary = %q", runner.binary)
	}
}

func writeConvertedFixture(t *testing.T, root string, source resolvedBuildSource, layerPayload []byte) ocispec.Manifest {
	t.Helper()
	workDir := filepath.Join(root, "73--"+source.ManifestDesc.Digest.Encoded())
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		t.Fatal(err)
	}
	configPayload := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configPayload),
		Size:      int64(len(configPayload)),
	}
	layerDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromBytes(layerPayload),
		Size:      int64(len(layerPayload)),
		Annotations: map[string]string{
			overlaybdBlobDigestAnnot: digest.FromBytes(layerPayload).String(),
		},
	}
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifestPayload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "manifest.json"), manifestPayload, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "config.json"), configPayload, 0o640); err != nil {
		t.Fatal(err)
	}
	layerDir := filepath.Join(workDir, "0000_"+source.Manifest.Layers[0].Digest.String())
	if err := os.MkdirAll(layerDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layerDir, "overlaybd.commit"), layerPayload, 0o640); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func rewriteConvertedConfig(t *testing.T, root string, payload []byte) {
	t.Helper()
	workDir, err := findConvertedWorkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	manifestPayload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestPayload, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Config.Digest = digest.FromBytes(payload)
	manifest.Config.Size = int64(len(payload))
	manifestPayload, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestPayload, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "config.json"), payload, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestImportConvertedImage(t *testing.T) {
	source := testResolvedOCISource()
	convertorRoot := t.TempDir()
	layerPayload := []byte("native overlaybd layer")
	manifest := writeConvertedFixture(t, convertorRoot, source, layerPayload)
	layout := artifactLayout{root: t.TempDir()}

	fetched, err := importConvertedImage(source, convertorRoot, layout)
	if err != nil {
		t.Fatalf("import converted image: %v", err)
	}
	if len(fetched.Layers) != 1 || fetched.Layers[0].Digest != manifest.Layers[0].Digest {
		t.Fatalf("layers = %+v", fetched.Layers)
	}
	if err := layout.checkBlob(manifest.Config); err != nil {
		t.Errorf("converted config: %v", err)
	}
	if err := layout.checkBlob(manifest.Layers[0]); err != nil {
		t.Errorf("converted layer: %v", err)
	}
	manifestPayload, err := os.ReadFile(fetched.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ManifestDesc.Digest != digest.FromBytes(manifestPayload) {
		t.Errorf("manifest descriptor = %s", fetched.ManifestDesc.Digest)
	}
	if err := layout.validateLayout(manifestPayload); err != nil {
		t.Errorf("converted layout: %v", err)
	}
}

func TestPrepareBuildSourceBranches(t *testing.T) {
	t.Run("native bypasses convertor", func(t *testing.T) {
		ctx := context.Background()
		store := memory.New()
		configDesc := pushBytes(t, store, ocispec.MediaTypeImageConfig, []byte(`{"architecture":"amd64","os":"linux"}`))
		layerDesc := pushBytes(t, store, overlaybdZfileLayerMediaType, []byte("native layer"))
		manifest := ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    configDesc,
			Layers:    []ocispec.Descriptor{layerDesc},
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		resolved := resolvedBuildSource{
			Ref:          registry.Reference{Registry: "registry.example.com", Repository: "team/native", Reference: "latest"},
			ManifestDesc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))},
			ManifestRaw:  raw,
			Manifest:     manifest,
		}
		runner := &recordingConvertorRunner{err: errors.New("must not run")}
		fetched, err := prepareBuildSource(ctx, store, resolved, dockerConfig{Auths: map[string]authEntry{}}, resolved.Ref.String(), artifactLayout{root: t.TempDir()}, t.TempDir(), 20, false, runner)
		if err != nil {
			t.Fatal(err)
		}
		if runner.binary != "" || len(runner.args) != 0 {
			t.Fatalf("native source invoked convertor: %q %q", runner.binary, runner.args)
		}
		if len(fetched.Layers) != 1 || fetched.Layers[0].Digest != layerDesc.Digest {
			t.Fatalf("native layers = %+v", fetched.Layers)
		}
	})

	t.Run("OCI imports converted layers", func(t *testing.T) {
		source := testResolvedOCISource()
		workDir := t.TempDir()
		convertorRoot := filepath.Join(workDir, convertedSourceDirName)
		converted := writeConvertedFixture(t, convertorRoot, source, []byte("converted native layer"))
		layout := artifactLayout{root: t.TempDir()}
		binary := filepath.Join(t.TempDir(), "convertor")
		if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := global.convertor
		global.convertor = binary
		t.Cleanup(func() { global.convertor = old })
		runner := &recordingConvertorRunner{}

		fetched, err := prepareBuildSource(context.Background(), nil, source, dockerConfig{Auths: map[string]authEntry{}}, source.Ref.String(), layout, workDir, 20, false, runner)
		if err != nil {
			t.Fatal(err)
		}
		if runner.binary != binary {
			t.Fatalf("convertor binary = %q", runner.binary)
		}
		if len(fetched.Layers) != 1 || fetched.Layers[0].Digest != converted.Layers[0].Digest {
			t.Fatalf("converted layers = %+v", fetched.Layers)
		}
		if _, err := os.Stat(layout.blobPath(source.Manifest.Layers[0].Digest)); !os.IsNotExist(err) {
			t.Fatalf("original OCI blob must not be imported: %v", err)
		}
	})

	t.Run("convertor failure stops preparation", func(t *testing.T) {
		source := testResolvedOCISource()
		binary := filepath.Join(t.TempDir(), "convertor")
		if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := global.convertor
		global.convertor = binary
		t.Cleanup(func() { global.convertor = old })
		layoutRoot := filepath.Join(t.TempDir(), "artifact")
		_, err := prepareBuildSource(context.Background(), nil, source, dockerConfig{Auths: map[string]authEntry{}}, source.Ref.String(), artifactLayout{root: layoutRoot}, t.TempDir(), 20, false, &recordingConvertorRunner{err: errors.New("exit status 1")})
		if err == nil || !strings.Contains(err.Error(), "convert OCI image") {
			t.Fatalf("convertor failure = %v", err)
		}
		if _, statErr := os.Stat(layoutRoot); !os.IsNotExist(statErr) {
			t.Fatalf("failed conversion created artifact files: %v", statErr)
		}
	})
}

func TestImportConvertedImageRejectsInvalidOutput(t *testing.T) {
	source := testResolvedOCISource()

	t.Run("multiple manifests", func(t *testing.T) {
		root := t.TempDir()
		writeConvertedFixture(t, root, source, []byte("one"))
		other := filepath.Join(root, "other")
		if err := os.MkdirAll(other, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, "manifest.json"), []byte("{}"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := importConvertedImage(source, root, artifactLayout{root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "exactly 1") {
			t.Fatalf("multiple manifests error = %v", err)
		}
	})

	t.Run("corrupt layer", func(t *testing.T) {
		root := t.TempDir()
		writeConvertedFixture(t, root, source, []byte("native layer"))
		workDir, err := findConvertedWorkDir(root)
		if err != nil {
			t.Fatal(err)
		}
		layerPath := filepath.Join(workDir, "0000_"+source.Manifest.Layers[0].Digest.String(), "overlaybd.commit")
		if err := os.WriteFile(layerPath, []byte("corrupt"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := importConvertedImage(source, root, artifactLayout{root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("corrupt layer error = %v", err)
		}
	})

	for _, tc := range []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "malformed config", payload: []byte("{not-json"), want: "parse converted config"},
		{name: "missing platform config", payload: []byte(`{"rootfs":{"type":"layers"}}`), want: "architecture and os"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeConvertedFixture(t, root, source, []byte("native layer"))
			rewriteConvertedConfig(t, root, tc.payload)
			if _, err := importConvertedImage(source, root, artifactLayout{root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("config validation error = %v", err)
			}
		})
	}

	t.Run("malformed converted digest", func(t *testing.T) {
		root := t.TempDir()
		writeConvertedFixture(t, root, source, []byte("native layer"))
		workDir, err := findConvertedWorkDir(root)
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(workDir, "manifest.json")
		payload, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var manifest ocispec.Manifest
		if err := json.Unmarshal(payload, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Layers[0].Digest = digest.Digest("sha256:../escape")
		payload, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, payload, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := importConvertedImage(source, root, artifactLayout{root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "invalid digest") {
			t.Fatalf("malformed digest error = %v", err)
		}
	})
}
