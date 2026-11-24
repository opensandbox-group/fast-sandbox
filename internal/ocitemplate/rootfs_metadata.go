package ocitemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"

	"fast-sandbox/internal/artifacts"
)

const (
	AnnotationRootfsMetadata = "io.fast-sandbox.rootfs-metadata"
	annotationRootfsID       = "io.fast-sandbox.rootfs-metadata.snapshot-id"
	RootfsMetadataVersion    = "v1"
	rootfsMetadataConfigKey  = "io.fast-sandbox.rootfs-only.v1"
	maxMetadataBytes         = 1 << 20
	// The config also carries up to 5 MiB of state as base64.
	MaxRootfsConfigBytes = 8 << 20
)

// RootfsMetadata contains the files needed before cold boot. State file names
// and digests must agree with the VM manifest before any files are written.
type RootfsMetadata struct {
	Manifest   json.RawMessage   `json:"manifest"`
	StateFiles map[string][]byte `json:"stateFiles,omitempty"`
}

func (m *RootfsMetadata) validate() error {
	if len(m.Manifest) > maxMetadataBytes {
		return errors.New("RootfsOnly VM manifest exceeds 1 MiB")
	}
	document, err := artifacts.DecodeVMManifest(m.Manifest)
	if err != nil {
		return err
	}
	if !document.IsRootfsOnly() {
		return errors.New("OCI config metadata requires a RootfsOnly VM manifest")
	}
	if len(m.StateFiles) != len(document.Platform.StateFiles) {
		return errors.New("OCI state files disagree with the VM manifest")
	}
	for name, payload := range m.StateFiles {
		limit := stateFileLimit(name)
		if limit == 0 {
			return fmt.Errorf("unsupported OCI state file %q", name)
		}
		if len(payload) > limit {
			return fmt.Errorf("OCI state file %q exceeds %d bytes", name, limit)
		}
		file, ok := document.Platform.StateFiles[name]
		if !ok || file.SizeBytes != int64(len(payload)) || file.SHA256 != digest.FromBytes(payload).Encoded() {
			return fmt.Errorf("OCI state file %q disagrees with the VM manifest", name)
		}
	}
	return nil
}

func stateFileLimit(name string) int {
	switch name {
	case "vault.json":
		return 4 << 20
	case "policy.json":
		return 1 << 20
	default:
		return 0
	}
}

// EncodeRootfsConfig extends the existing image config while preserving its
// standard fields. The OCI manifest binds the resulting blob via config.digest.
func EncodeRootfsConfig(config, manifest []byte, extras map[string]string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		return nil, err
	}
	if fields == nil || len(manifest) > maxMetadataBytes {
		return nil, errors.New("invalid RootfsOnly image config or oversized VM manifest")
	}
	metadata := RootfsMetadata{Manifest: manifest, StateFiles: make(map[string][]byte, len(extras))}
	for name, path := range extras {
		limit := stateFileLimit(name)
		if limit == 0 {
			return nil, fmt.Errorf("unsupported OCI state file %q", name)
		}
		raw, err := readMetadataFile(path, limit)
		if err != nil {
			return nil, err
		}
		metadata.StateFiles[name] = raw
	}
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	fields[rootfsMetadataConfigKey] = raw
	raw, err = json.Marshal(fields)
	if len(raw) > MaxRootfsConfigBytes {
		return nil, errors.New("RootfsOnly image config exceeds 8 MiB")
	}
	return raw, err
}

func readMetadataFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, fmt.Errorf("invalid OCI metadata file %s", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if len(raw) > limit {
		return nil, fmt.Errorf("OCI metadata file %s exceeds %d bytes", path, limit)
	}
	return raw, err
}

func decodeRootfsConfig(config []byte) (*RootfsMetadata, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		return nil, err
	}
	var metadata RootfsMetadata
	if err := json.Unmarshal(fields[rootfsMetadataConfigKey], &metadata); err != nil {
		return nil, fmt.Errorf("decode RootfsOnly OCI config: %w", err)
	}
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	return &metadata, nil
}

func rootfsMetadataEnvelope(reference string, metadata *RootfsMetadata) ([]byte, error) {
	return json.Marshal(struct {
		OCI Pair `json:"oci"`
	}{Pair{Rootfs: reference, Metadata: metadata}})
}

func pushRootfsMetadataImage(ctx context.Context, repo *remote.Repository, ref registry.Reference, dir string, image localImage, sources map[string]string, credentials CredentialFunc) (*PublishedCheckpoint, error) {
	if filepath.Base(image.File) != image.File {
		return nil, errors.New("invalid rootfs manifest file")
	}
	raw, err := readMetadataFile(filepath.Join(dir, "manifest", image.File), maxMetadataBytes)
	if err != nil {
		return nil, err
	}
	var manifest oci.Manifest
	if digest.FromBytes(raw).String() != image.Digest {
		return nil, errors.New("rootfs OCI manifest digest mismatch")
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.Annotations[AnnotationRootfsMetadata] != RootfsMetadataVersion || manifest.Config.MediaType != oci.MediaTypeImageConfig ||
		manifest.Config.Digest.Validate() != nil || manifest.Config.Digest.Algorithm() != digest.SHA256 || len(manifest.Layers) == 0 {
		return nil, errors.New("invalid RootfsOnly OCI manifest")
	}
	config, err := readMetadataFile(filepath.Join(dir, "blobs", "sha256", manifest.Config.Digest.Encoded()), MaxRootfsConfigBytes)
	if err != nil {
		return nil, err
	}
	if digest.FromBytes(config) != manifest.Config.Digest || int64(len(config)) != manifest.Config.Size {
		return nil, errors.New("RootfsOnly config digest or size mismatch")
	}
	metadata, err := decodeRootfsConfig(config)
	if err != nil {
		return nil, err
	}
	raw, immutable, err := pushLocalImage(ctx, repo, dir, ref.Reference, image, sources, credentials, map[string]string{annotationRootfsID: ref.Reference})
	if err != nil {
		return nil, err
	}
	payload, err := rootfsMetadataEnvelope(immutable, metadata)
	if err != nil {
		return nil, err
	}
	return &PublishedCheckpoint{Reference: Prefix + immutable, Metadata: payload, Manifests: map[string]json.RawMessage{immutable: raw}}, nil
}

func deleteRootfsMetadataImage(ctx context.Context, repo *remote.Repository, reference, id string) (bool, error) {
	desc, err := repo.Resolve(ctx, reference)
	if errors.Is(err, errdef.ErrNotFound) {
		// The publication digest is deleted last. An ID can already name a
		// replacement, so a retry must not follow its mutable rootfs tag.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := fetchSmall(ctx, repo, desc)
	if errors.Is(err, errdef.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var manifest oci.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, err
	}
	if _, marked := manifest.Annotations[AnnotationRootfsMetadata]; !marked {
		return false, nil
	}
	if desc.Digest.String() != reference || manifest.Annotations[annotationRootfsID] != id {
		return true, errors.New("refusing to delete RootfsOnly OCI manifest: snapshot ID or digest mismatch")
	}
	err = repo.Manifests().Delete(ctx, desc)
	if errors.Is(err, errdef.ErrNotFound) {
		err = nil
	}
	return true, err
}
