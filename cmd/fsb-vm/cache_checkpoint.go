package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/registry"

	"fast-sandbox/internal/ocitemplate"
)

// Cache only immutable, already-published content. No devices or mounts are
// retained here; blobs use a separate bounded local cache.
func newCacheCheckpointCommand() *cobra.Command {
	var published, authFile, artifacts string
	cmd := &cobra.Command{Use: "cache-checkpoint", Short: "Cache a locally published OCI checkpoint", Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(published)
			if err != nil {
				return err
			}
			var result ocitemplate.PublishedCheckpoint
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			raw, err = os.ReadFile(authFile)
			if err != nil {
				return err
			}
			config, err := parseDockerConfig(string(raw))
			if err != nil {
				return err
			}
			return cacheCheckpoint(cmd.Context(), &result, config, artifacts)
		}}
	cmd.Flags().StringVar(&published, "published", "", "published immutable metadata file")
	cmd.Flags().StringVar(&authFile, "docker-auth-file", "", "restore credential file")
	cmd.Flags().StringVar(&artifacts, "artifacts", "", "local packed artifact directory")
	for _, flag := range []string{"published", "docker-auth-file", "artifacts"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func cacheCheckpoint(ctx context.Context, result *ocitemplate.PublishedCheckpoint, config dockerConfig, artifacts string) error {
	ref, err := parseRegistryReference(strings.TrimPrefix(result.Reference, ocitemplate.Prefix))
	if err != nil {
		return err
	}
	pair, err := templatePair(result.Metadata, ref)
	if err != nil {
		return err
	}
	publicationRef := pair.Snapfiles
	if pair.Metadata != nil {
		publicationRef = pair.Rootfs
	}
	if publicationRef != strings.TrimPrefix(result.Reference, ocitemplate.Prefix) {
		return fmt.Errorf("published checkpoint pair mismatch")
	}
	// Validate all manifests before writing metadata or requesting blob imports.
	var layers []oci.Descriptor
	for _, reference := range pair.ImageRefs() {
		imageRef, err := parseRegistryReference(reference)
		if err != nil {
			return err
		}
		raw := result.Manifests[reference]
		if digest.FromBytes(raw).String() != imageRef.Reference {
			return fmt.Errorf("published manifest digest mismatch")
		}
		var manifest oci.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		layers = append(layers, manifest.Layers...)
	}
	for _, reference := range pair.ImageRefs() {
		imageRef, _ := parseRegistryReference(reference)
		path, err := createManifestCachePath(imageRef, config)
		if err != nil {
			return err
		}
		if err := cacheImageManifest(path, imageRef.Reference, result.Manifests[reference]); err != nil {
			return err
		}
	}
	if pair.Metadata != nil {
		if err := cacheRootfsConfig(ref, config, artifacts, result.Manifests[pair.Rootfs]); err != nil {
			return err
		}
		return retainCheckpointLayers(ctx, layers, artifacts)
	}
	path, err := createManifestCachePath(ref, config)
	if err != nil {
		return err
	}
	if err := cacheManifestFile(path+".template", result.Metadata); err != nil {
		return err
	}
	return retainCheckpointLayers(ctx, layers, artifacts)
}

func cacheRootfsConfig(ref registry.Reference, config dockerConfig, artifacts string, manifestBytes []byte) error {
	var manifest oci.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	configDigest := manifest.Config.Digest
	if configDigest.Validate() != nil || configDigest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("published RootfsOnly config requires a valid sha256 digest")
	}
	if manifest.Config.Size < 0 || manifest.Config.Size > ocitemplate.MaxRootfsConfigBytes {
		return fmt.Errorf("published RootfsOnly config size mismatch")
	}
	configRef := ref
	configRef.Reference = configDigest.String()
	path, err := createManifestCachePath(configRef, config)
	if err != nil {
		return err
	}
	// Config payloads are private staging files written by pack.
	blob := filepath.Join(artifacts, "blobs", "sha256", configDigest.Encoded())
	info, err := os.Lstat(blob)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != manifest.Config.Size {
		return fmt.Errorf("published RootfsOnly config size or type mismatch")
	}
	file, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, ocitemplate.MaxRootfsConfigBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) != manifest.Config.Size || digest.FromBytes(raw) != configDigest {
		return fmt.Errorf("published RootfsOnly config digest or size mismatch")
	}
	return cacheImageManifest(path, configRef.Reference, raw)
}

// The publisher has already verified these private staging files against their
// descriptors. Retain the same inode after publication instead of copying and
// hashing it again. Missing inherited layers continue to use registry reads.
func retainCheckpointLayers(ctx context.Context, layers []oci.Descriptor, artifacts string) error {
	if global.blobCacheMaxBytes <= 0 {
		return nil
	}
	unlock, err := tryLock(blobCacheRoot(), "cache")
	if err != nil {
		return err
	}
	defer unlock()
	seen := map[digest.Digest]bool{}
	for _, layer := range layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if layer.Digest.Validate() != nil || layer.Digest.Algorithm() != digest.SHA256 || layer.Size <= 0 {
			return fmt.Errorf("invalid checkpoint layer")
		}
		if seen[layer.Digest] {
			continue
		}
		seen[layer.Digest] = true
		path, err := filepath.Abs(filepath.Join(artifacts, "blobs/sha256", layer.Digest.Encoded()))
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		} // inherited layers already belong to the shared cache
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != layer.Size {
			return fmt.Errorf("local checkpoint layer size or type mismatch")
		}
		if err := retainBlob(path, layer.Digest, layer.Size); err != nil {
			return err
		}
	}
	return nil
}
