package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

// This file owns the local artifact layout produced by `template-vm build`:
//
//	<output-dir>/
//	  blobs/sha256/<source layer digests + committed layers + config>
//	  manifest/<source-manifest>.json
//	          <name>_<tag>_rootfs.json
//	          <name>_<tag>_snapfiles.json
//	          index.json
//
// The layout is self-contained: every blob referenced by the two generated
// manifests exists under blobs/sha256/, so a later push step can upload the
// pair to any registry straight from this directory.

const (
	blobsDirName    = "blobs"
	sha256DirName   = "sha256"
	manifestDirName = "manifest"
	layoutIndexName = "index.json"
)

// artifactLayout addresses the output directories of one build.
type artifactLayout struct {
	root string
}

func (l artifactLayout) blobDir() string {
	return filepath.Join(l.root, blobsDirName, sha256DirName)
}

func (l artifactLayout) manifestDir() string {
	return filepath.Join(l.root, manifestDirName)
}

// blobPath maps a previously validated sha256 digest to its blob file.
func (l artifactLayout) blobPath(d digest.Digest) string {
	return filepath.Join(l.blobDir(), d.Encoded())
}

func validateSHA256Descriptor(desc ocispec.Descriptor, label string) error {
	if err := desc.Digest.Validate(); err != nil {
		return fmt.Errorf("%s has invalid digest %q: %w", label, desc.Digest, err)
	}
	if desc.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("%s uses unsupported digest algorithm %q (%s)", label, desc.Digest.Algorithm(), desc.Digest)
	}
	if desc.Size < 0 {
		return fmt.Errorf("%s has negative size %d", label, desc.Size)
	}
	return nil
}

// manifestPath maps a manifest file name into the manifest dir.
func (l artifactLayout) manifestPath(name string) string {
	return filepath.Join(l.manifestDir(), name)
}

// ensure creates the blob and manifest directories.
func (l artifactLayout) ensure() error {
	if err := os.MkdirAll(l.blobDir(), 0o750); err != nil {
		return fmt.Errorf("create blob directory: %w", err)
	}
	if err := os.MkdirAll(l.manifestDir(), 0o750); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	return nil
}

func createStagedArtifactLayout(outputDir, name string) (artifactLayout, artifactLayout, error) {
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return artifactLayout{}, artifactLayout{}, fmt.Errorf("create artifact output directory: %w", err)
	}
	final := artifactLayout{root: filepath.Join(outputDir, name)}
	if _, err := os.Lstat(final.root); err == nil {
		return artifactLayout{}, artifactLayout{}, fmt.Errorf("artifact output %s already exists", final.root)
	} else if !os.IsNotExist(err) {
		return artifactLayout{}, artifactLayout{}, fmt.Errorf("check artifact output %s: %w", final.root, err)
	}
	stagingRoot, err := os.MkdirTemp(outputDir, "."+name+".tmp-")
	if err != nil {
		return artifactLayout{}, artifactLayout{}, fmt.Errorf("create artifact staging directory: %w", err)
	}
	staging := artifactLayout{root: stagingRoot}
	if err := staging.ensure(); err != nil {
		_ = os.RemoveAll(stagingRoot)
		return artifactLayout{}, artifactLayout{}, err
	}
	return staging, final, nil
}

func publishArtifactLayout(staging, final artifactLayout) error {
	if err := unix.Renameat2(unix.AT_FDCWD, staging.root, unix.AT_FDCWD, final.root, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("artifact output %s already exists", final.root)
		}
		return fmt.Errorf("publish artifact %s: %w", final.root, err)
	}
	return nil
}

// unsafeNameChars matches every character not allowed in artifact file names.
var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFileName turns an image name (redis:6.2.1_rootfs) into a safe
// manifest file name (redis_6.2.1_rootfs).
func sanitizeFileName(name string) string {
	return unsafeNameChars.ReplaceAllString(name, "_")
}

// sourceManifestName derives the stored file name of the downloaded source
// manifest from its reference (tag or digest).
func sourceManifestName(reference string) string {
	return "source-" + sanitizeFileName(reference) + ".json"
}

// installBytes writes payload as a blob addressed by its sha256 digest and
// returns the descriptor (with the given mediaType). The write is atomic.
func (l artifactLayout) installBytes(payload []byte, mediaType string) (ocispec.Descriptor, error) {
	sum := sha256.Sum256(payload)
	d := digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:]))
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(payload))}
	final := l.blobPath(d)
	if _, err := os.Stat(final); err == nil {
		if err := checkFileDescriptor(final, desc); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("verify existing blob %s: %w", d, err)
		}
		return desc, nil
	} else if !os.IsNotExist(err) {
		return ocispec.Descriptor{}, fmt.Errorf("stat blob %s: %w", d, err)
	}
	if err := l.ensure(); err != nil {
		return ocispec.Descriptor{}, err
	}
	temp, err := os.CreateTemp(l.blobDir(), ".blob-*.tmp")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create temp blob: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return ocispec.Descriptor{}, fmt.Errorf("write temp blob: %w", err)
	}
	if err := temp.Close(); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("close temp blob: %w", err)
	}
	if err := os.Rename(tempName, final); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("commit blob %s: %w", final, err)
	}
	return desc, nil
}

// installFile moves a produced file (an overlaybd-commit output) into the
// blob store addressed by its content digest. Annotations are attached to
// the returned descriptor.
func (l artifactLayout) installFile(srcPath, mediaType string, annotations map[string]string) (ocispec.Descriptor, error) {
	source, err := os.Open(srcPath)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer source.Close()
	if err := l.ensure(); err != nil {
		return ocispec.Descriptor{}, err
	}
	temp, err := os.CreateTemp(l.blobDir(), ".blob-*.tmp")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create temp blob: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	hasher := sha256.New()
	size, err := io.Copy(temp, io.TeeReader(source, hasher))
	if err != nil {
		_ = temp.Close()
		return ocispec.Descriptor{}, fmt.Errorf("read %s: %w", srcPath, err)
	}
	if err := temp.Close(); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("close temp blob: %w", err)
	}
	d := digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(hasher.Sum(nil)))
	final := l.blobPath(d)
	if err := os.Rename(tempName, final); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("commit blob %s: %w", final, err)
	}
	return ocispec.Descriptor{
		MediaType:   mediaType,
		Digest:      d,
		Size:        size,
		Annotations: annotations,
	}, nil
}

func writeFileAtomic(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".write-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func checkFileDescriptor(path string, desc ocispec.Descriptor) error {
	if err := validateSHA256Descriptor(desc, "blob descriptor"); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != desc.Size {
		return fmt.Errorf("size %d, manifest records %d", info.Size(), desc.Size)
	}
	verifier := desc.Digest.Verifier()
	if _, err := io.Copy(verifier, file); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("content digest does not match %s", desc.Digest)
	}
	return nil
}

func (l artifactLayout) importVerifiedFile(srcPath string, desc ocispec.Descriptor) error {
	if err := checkFileDescriptor(srcPath, desc); err != nil {
		return fmt.Errorf("verify %s: %w", srcPath, err)
	}
	if err := l.ensure(); err != nil {
		return err
	}
	final := l.blobPath(desc.Digest)
	if _, err := os.Stat(final); err == nil {
		return l.checkBlob(desc)
	} else if !os.IsNotExist(err) {
		return err
	}

	placeholder, err := os.CreateTemp(l.blobDir(), ".import-*.tmp")
	if err != nil {
		return err
	}
	tempName := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return err
	}
	if err := os.Remove(tempName); err != nil {
		return err
	}
	defer os.Remove(tempName)

	if err := os.Link(srcPath, tempName); err != nil {
		source, openErr := os.Open(srcPath)
		if openErr != nil {
			return openErr
		}
		defer source.Close()
		temp, createErr := os.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if createErr != nil {
			return createErr
		}
		if _, copyErr := io.Copy(temp, source); copyErr != nil {
			_ = temp.Close()
			return copyErr
		}
		if syncErr := temp.Sync(); syncErr != nil {
			_ = temp.Close()
			return syncErr
		}
		if closeErr := temp.Close(); closeErr != nil {
			return closeErr
		}
	}
	if err := checkFileDescriptor(tempName, desc); err != nil {
		return fmt.Errorf("verify imported blob: %w", err)
	}
	if err := os.Rename(tempName, final); err != nil {
		return fmt.Errorf("commit blob %s: %w", final, err)
	}
	return nil
}

// minimalImageConfig renders the generated config blob shared by both output
// manifests. The manifests describe block-device layers, not filesystem
// changesets, so diff_ids is intentionally empty.
func minimalImageConfig() []byte {
	payload, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"config":       map[string]any{},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{},
		},
	})
	if err != nil {
		panic(err)
	}
	return payload
}

// assembleManifest builds an OCI image manifest JSON around the given layers
// and returns the serialized bytes plus the manifest's own descriptor.
func assembleManifest(configDesc ocispec.Descriptor, layers []ocispec.Descriptor) ([]byte, ocispec.Descriptor, error) {
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    layers,
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("marshal manifest: %w", err)
	}
	sum := sha256.Sum256(payload)
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:])),
		Size:      int64(len(payload)),
	}
	return payload, desc, nil
}

// layoutIndexEntry records one manifest of the build output.
type layoutIndexEntry struct {
	Name           string `json:"name"`
	ManifestFile   string `json:"manifest_file"`
	ManifestDigest string `json:"manifest_digest"`
}

// layoutIndex is manifest/index.json: the contract consumed by the future
// ORAS push step to locate manifests and their blobs.
type layoutIndex struct {
	SchemaVersion  int                `json:"schema_version"`
	SourceRef      string             `json:"source_ref"`
	SourceManifest layoutIndexEntry   `json:"source_manifest"`
	Images         []layoutIndexEntry `json:"images"`
}

const layoutIndexSchemaVersion = 1

// writeLayoutIndex serializes the layout index atomically.
func (l artifactLayout) writeLayoutIndex(index layoutIndex) error {
	index.SchemaVersion = layoutIndexSchemaVersion
	payload, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal layout index: %w", err)
	}
	target := filepath.Join(l.manifestDir(), layoutIndexName)
	temp, err := os.CreateTemp(l.manifestDir(), ".index-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp index: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temp index: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp index: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		return fmt.Errorf("commit layout index %s: %w", target, err)
	}
	return nil
}

// validateLayout checks that every blob referenced by the given manifests
// (config + layers) exists locally with the recorded digest and size.
func (l artifactLayout) validateLayout(manifests ...[]byte) error {
	for _, raw := range manifests {
		var manifest ocispec.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return fmt.Errorf("parse generated manifest: %w", err)
		}
		descriptors := append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...)
		for _, desc := range descriptors {
			if err := l.checkBlob(desc); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkBlob verifies one blob exists with the expected size and digest.
func (l artifactLayout) checkBlob(desc ocispec.Descriptor) error {
	if err := validateSHA256Descriptor(desc, "blob descriptor"); err != nil {
		return err
	}
	path := l.blobPath(desc.Digest)
	if err := checkFileDescriptor(path, desc); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("blob %s missing: %w", desc.Digest, err)
		}
		return fmt.Errorf("blob %s: %w", desc.Digest, err)
	}
	return nil
}
