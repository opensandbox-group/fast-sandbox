package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"

	"fast-sandbox/internal/ocitemplate"
)

// resolveTemplate keeps platform-envelope resolution inside the CLI, alongside
// layer-manifest resolution. Fastlet never needs its own registry/cache client.
// Existing explicit-pair CLI inputs continue to work without an envelope.
func resolveTemplate(ctx context.Context, template *templateFile, config dockerConfig) error {
	reference := template.Template.Reference
	named := reference == ""
	if named {
		if template.Template.Rootfs != "" || template.Template.Snapfiles != "" {
			return nil
		}
		reference = template.Template.Repo + ":" + template.Template.TemplateID
	}
	ref, err := parseRegistryReference(strings.TrimPrefix(reference, ocitemplate.Prefix))
	if err != nil {
		return err
	}
	cachePath, err := createManifestCachePath(ref, config)
	if err != nil {
		return err
	}
	var payload []byte
	if cachePath != "" {
		payload, _ = readCachedManifest(cachePath + ".template")
		if pair, err := templatePair(payload, ref); err != nil || pair.Metadata != nil {
			payload = nil
		}
	}
	if len(payload) == 0 {
		repository, err := newRemoteRepository(ref, config, global.plainHTTP)
		if err != nil {
			return err
		}
		cached := &templateRepository{MetadataRepository: repository, ref: ref, config: config}
		resolve := ocitemplate.ResolveFrom
		if named {
			resolve = ocitemplate.ResolveNamed
		}
		payload, err = resolve(ctx, ref, cached)
		if err != nil {
			return fmt.Errorf("resolve OCI template: %w", err)
		}
		pair, err := templatePair(payload, ref)
		if err != nil {
			return err
		}
		if cachePath != "" && pair.Metadata == nil {
			if err := cacheManifestFile(cachePath+".template", payload); err != nil {
				fmt.Fprintf(os.Stderr, "warning: cache template metadata: %v\n", err)
			}
		}
	}
	pair, err := templatePair(payload, ref)
	if err != nil {
		return err
	}
	template.Template.Rootfs, template.Template.Snapfiles = pair.Rootfs, pair.Snapfiles
	template.Metadata = payload
	template.RootfsMetadata = pair.Metadata
	return nil
}

func templatePair(payload []byte, ref registry.Reference) (*ocitemplate.Pair, error) {
	pair, err := ocitemplate.Decode(payload)
	if err != nil {
		return nil, err
	}
	if pair == nil {
		return nil, fmt.Errorf("OCI template has no pair")
	}
	for _, reference := range pair.ImageRefs() {
		value, err := parseRegistryReference(reference)
		if err != nil || value.Registry != ref.Registry || value.Repository != ref.Repository {
			return nil, fmt.Errorf("template pair must stay in the template repository")
		}
	}
	if pair.Metadata != nil && strings.HasPrefix(ref.Reference, "sha256:") &&
		pair.Rootfs != ref.Registry+"/"+ref.Repository+"@"+ref.Reference {
		return nil, fmt.Errorf("RootfsOnly OCI metadata references a different rootfs digest")
	}
	return pair, nil
}

// templateRepository shares the existing digest-verified raw-manifest cache with
// resolveImage. Cold envelope reads therefore also warm the snapfiles manifest.
type templateRepository struct {
	ocitemplate.MetadataRepository
	ref    registry.Reference
	config dockerConfig
}

func (r *templateRepository) cached(reference string) ([]byte, string, error) {
	ref := r.ref
	ref.Reference = reference
	path, err := createManifestCachePath(ref, r.config)
	if err != nil {
		return nil, "", err
	}
	raw, _ := readCachedImageManifest(path, reference)
	return raw, path, nil
}

func (r *templateRepository) Resolve(ctx context.Context, reference string) (oci.Descriptor, error) {
	raw, _, err := r.cached(reference)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if len(raw) != 0 {
		var manifest struct {
			MediaType string `json:"mediaType"`
		}
		if err := json.Unmarshal(raw, &manifest); err == nil && manifest.MediaType != "" {
			return oci.Descriptor{MediaType: manifest.MediaType, Digest: digest.Digest(reference), Size: int64(len(raw))}, nil
		}
	}
	return r.MetadataRepository.Resolve(ctx, reference)
}

func (r *templateRepository) Fetch(ctx context.Context, desc oci.Descriptor) (io.ReadCloser, error) {
	limit := int64(1 << 20)
	if desc.MediaType == oci.MediaTypeImageConfig {
		limit = ocitemplate.MaxRootfsConfigBytes
	}
	if desc.Size < 0 || desc.Size > limit {
		return nil, fmt.Errorf("template metadata exceeds %d bytes", limit)
	}
	raw, path, err := r.cached(desc.Digest.String())
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		raw, err = content.FetchAll(ctx, r.MetadataRepository, desc)
		if err != nil {
			return nil, err
		}
		if err := cacheImageManifest(path, desc.Digest.String(), raw); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cache template manifest: %v\n", err)
		}
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}
