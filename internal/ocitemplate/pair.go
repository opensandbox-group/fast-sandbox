package ocitemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"

	"fast-sandbox/internal/artifacts"
)

const (
	AnnotationTemplateID   = "io.fast-sandbox.template.id"
	AnnotationSandboxUID   = "io.fast-sandbox.sandbox.uid"
	AnnotationNamespace    = "io.fast-sandbox.sandbox.namespace"
	AnnotationCheckpointID = "io.fast-sandbox.checkpoint.id"
	// AnnotationRootfsBaseLayers records how many bottom layers of a rootfs
	// manifest belong to the immutable template base. It travels with the
	// published pair so checkpoint lineages keep the boundary that keeps
	// base blobs shared across sandboxes.
	AnnotationRootfsBaseLayers = "io.fast-sandbox.rootfs.base-layers"
)

// validatePairID validates both derived tag names, including the registry tag length
// limit. It is a tag component, never a repository path or a digest.
func validatePairID(id string) error {
	for _, suffix := range []string{"_rootfs", "_snapfiles"} {
		ref := registry.Reference{Reference: id + suffix}
		if id == "" || ref.ValidateReferenceAsTag() != nil {
			return fmt.Errorf("invalid template pair ID %q", id)
		}
	}
	return nil
}

func pairAnnotations(id string, metadata []byte) (map[string]string, error) {
	if err := validatePairID(id); err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("template metadata must be a JSON object")
	}
	annotations := map[string]string{AnnotationTemplateID: id}
	if raw, ok := doc["platform"]; ok {
		var platform artifacts.VMPlatform
		if err := json.Unmarshal(raw, &platform); err != nil {
			return nil, err
		}
		if platform.Checkpoint == nil {
			return annotations, nil
		}
		owner := platform.Checkpoint
		if strings.TrimSpace(owner.SandboxUID) == "" || strings.TrimSpace(owner.Namespace) == "" {
			return nil, fmt.Errorf("checkpoint requires sandbox UID and namespace")
		}
		annotations[AnnotationSandboxUID], annotations[AnnotationNamespace], annotations[AnnotationCheckpointID] = owner.SandboxUID, owner.Namespace, id
	}
	return annotations, nil
}

func resolvePair(ctx context.Context, repo MetadataRepository, ref registry.Reference, snap oci.Descriptor, manifest oci.Manifest) ([]byte, error) {
	annotations := manifest.Annotations
	id := annotations[AnnotationTemplateID]
	if err := validatePairID(id); err != nil {
		return nil, err
	}
	doc := map[string]json.RawMessage{}
	// Owner annotations are authoritative; never infer ownership from tag text.
	uid, namespace, checkpoint := annotations[AnnotationSandboxUID], annotations[AnnotationNamespace], annotations[AnnotationCheckpointID]
	if uid != "" || namespace != "" || checkpoint != "" {
		if strings.TrimSpace(uid) == "" || strings.TrimSpace(namespace) == "" || checkpoint != id {
			return nil, fmt.Errorf("incomplete or mismatched checkpoint owner annotations")
		}
		raw, err := json.Marshal(map[string]string{"sandboxUID": uid, "namespace": namespace, "id": checkpoint})
		if err != nil {
			return nil, err
		}
		doc["checkpoint"] = raw
	}
	root, err := repo.Resolve(ctx, id+"_rootfs")
	if err != nil {
		return nil, err
	}
	if root.MediaType != oci.MediaTypeImageManifest {
		return nil, fmt.Errorf("unexpected rootfs manifest media type %q", root.MediaType)
	}
	repository := ref.Registry + "/" + ref.Repository
	pair := Pair{Rootfs: repository + "@" + root.Digest.String(), Snapfiles: repository + "@" + snap.Digest.String()}
	raw, err := json.Marshal(pair)
	if err != nil {
		return nil, err
	}
	doc["oci"] = raw
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, err = Decode(payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// IDs are write-once. Identical retries are harmless, but conflicting content
// must not change one half of an already published pair. Registry-side tag
// immutability is still needed against concurrent writers and other clients.
func pushImmutableManifest(ctx context.Context, repo *remote.Repository, tag string, desc oci.Descriptor, raw []byte) error {
	existing, err := repo.Resolve(ctx, tag)
	if err == nil {
		if existing.Digest != desc.Digest {
			return fmt.Errorf("refusing to overwrite published template tag %q", tag)
		}
		return nil
	}
	if !errors.Is(err, errdef.ErrNotFound) {
		return err
	}
	if err = repo.Manifests().PushReference(ctx, desc, bytes.NewReader(raw), tag); err != nil {
		return err
	}
	got, err := repo.Resolve(ctx, tag)
	if err != nil {
		return err
	}
	if got.Digest != desc.Digest {
		return fmt.Errorf("remote template digest mismatch for %q", tag)
	}
	return nil
}

// DeletePair deletes only the two manifests owned by id, never their blobs.
// Rootfs manifests carry the pair ID too, so identical layer sets in different
// snapshots have distinct manifest digests and can be deleted independently.
// The snapfiles publication point is deleted last, making partial deletion
// retryable using the same immutable reference and ID.
func DeletePair(ctx context.Context, reference, id string, credentials CredentialFunc) error {
	if err := validatePairID(id); err != nil {
		return err
	}
	repo, ref, err := Repository(reference, credentials)
	if err != nil {
		return err
	}
	if err := ValidateDigestReference(reference, strings.TrimPrefix(ref.Reference, "sha256:")); err != nil {
		return err
	}
	var targets []oci.Descriptor
	// Validate both objects before the first deletion. A mismatched tag or
	// annotation must never delete one half of another snapshot.
	for _, name := range []string{id + "_rootfs", ref.Reference} {
		desc, err := repo.Resolve(ctx, name)
		if errors.Is(err, errdef.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		raw, err := fetchSmall(ctx, repo, desc)
		if err != nil {
			return err
		}
		var manifest oci.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		if manifest.Annotations[AnnotationTemplateID] != id {
			return fmt.Errorf("refusing to delete manifest %s: snapshot pair ID mismatch", desc.Digest)
		}
		targets = append(targets, desc)
	}
	for _, target := range targets {
		if err := repo.Manifests().Delete(ctx, target); err != nil && !errors.Is(err, errdef.ErrNotFound) {
			return err
		}
	}
	return nil
}
