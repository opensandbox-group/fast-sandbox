// Package ocitemplate publishes and resolves fsb-vm template pairs. Only small
// metadata is fetched here; OverlayBD owns the remote layer reads.
package ocitemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const Prefix = "oci://"

type Pair struct {
	Rootfs    string `json:"rootfs"`
	Snapfiles string `json:"snapfiles"`
}

// MetadataRepository is the small registry surface needed to resolve a template.
// A CLI may wrap it with its credential-scoped immutable manifest cache.
type MetadataRepository interface {
	Resolve(context.Context, string) (oci.Descriptor, error)
	Fetch(context.Context, oci.Descriptor) (io.ReadCloser, error)
}

// CredentialFunc resolves a full registry/repository reference, not just a host.
type CredentialFunc func(context.Context, string) (auth.Credential, error)

// RepositoryOption configures registry transport without changing HTTPS defaults.
type RepositoryOption func(*remote.Repository)

// WithPlainHTTP explicitly selects HTTP for registries without TLS.
func WithPlainHTTP(enabled bool) RepositoryOption {
	return func(repo *remote.Repository) { repo.PlainHTTP = enabled }
}

// ValidatePublishRepository requires a repository-only SandboxTemplate target.
// Explicit tags remain supported by the standalone Push API.
func ValidatePublishRepository(reference string) error {
	raw := strings.TrimPrefix(reference, Prefix)
	ref, err := registry.ParseReference(raw)
	if !strings.HasPrefix(reference, Prefix) || err != nil || ref.Reference != "" || raw != ref.Registry+"/"+ref.Repository {
		return fmt.Errorf("OCI publish target must be oci://registry/repository without a tag or digest")
	}
	return nil
}

func Repository(reference string, credentials CredentialFunc, options ...RepositoryOption) (*remote.Repository, registry.Reference, error) {
	ref, err := registry.ParseReference(strings.TrimPrefix(reference, Prefix))
	if err != nil {
		return nil, ref, err
	}
	if ref.Reference == "" {
		return nil, ref, fmt.Errorf("OCI template requires a tag or digest")
	}
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, ref, err
	}
	for _, option := range options {
		option(repo)
	}
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if credentials != nil {
		credentialHost := ref.Registry
		// ORAS sends Docker Hub requests to its distribution endpoint.
		if credentialHost == "docker.io" {
			credentialHost = "registry-1.docker.io"
		}
		client.Credential = func(ctx context.Context, host string) (auth.Credential, error) {
			if host != credentialHost {
				return auth.EmptyCredential, nil
			}
			return credentials(ctx, ref.String())
		}
	}
	repo.Client = client
	return repo, ref, nil
}

func Decode(payload []byte) (*Pair, error) {
	var document struct {
		OCI *Pair `json:"oci"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, err
	}
	if document.OCI != nil {
		for _, value := range []string{document.OCI.Rootfs, document.OCI.Snapfiles} {
			ref, err := registry.ParseReference(value)
			if err != nil || !strings.HasPrefix(ref.Reference, "sha256:") {
				return nil, fmt.Errorf("template pair must use immutable sha256 references")
			}
			if err := digest.Digest(ref.Reference).Validate(); err != nil {
				return nil, err
			}
		}
	}
	return document.OCI, nil
}

// ResolveFrom resolves the platform envelope through the caller's registry/cache.
func ResolveFrom(ctx context.Context, ref registry.Reference, repo MetadataRepository) ([]byte, error) {
	desc, err := repo.Resolve(ctx, ref.Reference)
	if errors.Is(err, errdef.ErrNotFound) && !strings.HasPrefix(ref.Reference, "sha256:") {
		desc, err = repo.Resolve(ctx, ref.Reference+"_snapfiles")
	}
	if err != nil {
		return nil, err
	}
	if desc.MediaType != oci.MediaTypeImageManifest {
		return nil, fmt.Errorf("unexpected template manifest media type %q", desc.MediaType)
	}
	raw, err := fetchSmall(ctx, repo, desc)
	if err != nil {
		return nil, err
	}
	var manifest oci.Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.Config.MediaType != oci.MediaTypeImageConfig {
		return nil, fmt.Errorf("unexpected template config media type %q; rebuild the template as a rootfs/snapfiles pair", manifest.Config.MediaType)
	}
	return resolvePair(ctx, repo, ref, desc, manifest)
}

func fetchSmall(ctx context.Context, repo MetadataRepository, desc oci.Descriptor) ([]byte, error) {
	if desc.Size < 0 || desc.Size > 1<<20 {
		return nil, fmt.Errorf("template metadata exceeds 1 MiB")
	}
	return content.FetchAll(ctx, repo, desc)
}

// PublishedCheckpoint contains the exact immutable metadata that was published.
// It can seed the existing restore cache without another registry round trip.
type PublishedCheckpoint struct {
	Reference string                     `json:"reference"`
	Metadata  json.RawMessage            `json:"metadata"`
	Manifests map[string]json.RawMessage `json:"manifests"`
}

// Push publishes rootfs first and the snapfiles manifest last as the publication point.
func Push(ctx context.Context, dir, destination string, metadata []byte, credentials CredentialFunc, options ...RepositoryOption) (string, string, error) {
	result, err := PushForCache(ctx, dir, destination, metadata, credentials, options...)
	if err != nil {
		return "", "", err
	}
	ref, err := registry.ParseReference(strings.TrimPrefix(result.Reference, Prefix))
	return result.Reference, strings.TrimPrefix(ref.Reference, "sha256:"), err
}

// PushForCache also returns the published immutable metadata for local cache population.
func PushForCache(ctx context.Context, dir, destination string, metadata []byte, credentials CredentialFunc, options ...RepositoryOption) (*PublishedCheckpoint, error) {
	repo, ref, err := Repository(destination, credentials, options...)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(ref.Reference, "sha256:") {
		return nil, fmt.Errorf("publish destination must have a tag")
	}
	var index struct {
		Images []localImage `json:"images"`
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest/index.json"))
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &index); err != nil {
		return nil, err
	}
	if len(index.Images) != 2 {
		return nil, fmt.Errorf("expected exactly two template images")
	}
	// Incremental exports carry descriptors for inherited remote layers, not
	// local copies. Keep those blobs reachable from the checkpoint repository.
	sources := map[string]string{}
	if rawSources, e := os.ReadFile(filepath.Join(dir, RemoteSourcesName)); e == nil {
		if e = json.Unmarshal(rawSources, &sources); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	annotations, err := pairAnnotations(ref.Reference, metadata)
	if err != nil {
		return nil, err
	}
	var root, snap *localImage
	for i := range index.Images {
		image := &index.Images[i]
		switch {
		case strings.HasSuffix(image.Name, "_rootfs"):
			if root != nil {
				return nil, fmt.Errorf("duplicate rootfs image")
			}
			root = image
		case strings.HasSuffix(image.Name, "_snapfiles"):
			if snap != nil {
				return nil, fmt.Errorf("duplicate snapfiles image")
			}
			snap = image
		default:
			return nil, fmt.Errorf("unknown pair image %q", image.Name)
		}
	}
	if root == nil || snap == nil {
		return nil, fmt.Errorf("template requires rootfs and snapfiles")
	}
	rootRaw, rootRef, err := pushLocalImage(ctx, repo, dir, ref.Reference, *root, sources, credentials, map[string]string{AnnotationTemplateID: ref.Reference})
	if err != nil {
		return nil, err
	}
	snapRaw, immutable, err := pushLocalImage(ctx, repo, dir, ref.Reference, *snap, sources, credentials, annotations)
	if err != nil {
		return nil, err
	}
	snapDesc := content.NewDescriptorFromBytes(oci.MediaTypeImageManifest, snapRaw)
	rootDesc := content.NewDescriptorFromBytes(oci.MediaTypeImageManifest, rootRaw)
	var manifest oci.Manifest
	if err := json.Unmarshal(snapRaw, &manifest); err != nil {
		return nil, err
	}
	payload, err := pairMetadata(ref, snapDesc, rootDesc, manifest)
	if err != nil {
		return nil, err
	}
	return &PublishedCheckpoint{Reference: Prefix + immutable, Metadata: payload,
		Manifests: map[string]json.RawMessage{rootRef: rootRaw, immutable: snapRaw}}, nil
}

// ValidateDigestReference binds a checkpoint's immutable address to its durable
// status digest. Resume never accepts a mutable tag or a mismatched digest.
func ValidateDigestReference(reference, sum string) error {
	ref, err := registry.ParseReference(strings.TrimPrefix(reference, Prefix))
	if err != nil {
		return err
	}
	expected := digest.Digest("sha256:" + sum)
	if err = expected.Validate(); err != nil {
		return err
	}
	if ref.Reference != expected.String() {
		return fmt.Errorf("OCI checkpoint manifest digest does not match checkpoint status")
	}
	return nil
}

// inheritBlob avoids downloading the parent's rootfs to local checkpoint
// staging. Already present blobs need no transfer; registry mounts avoid the
// first cross-repository transfer when supported. ORAS streams on mount fallback.
func inheritBlob(ctx context.Context, destination *remote.Repository, desc oci.Descriptor, source string, credentials CredentialFunc) error {
	exists, err := destination.Blobs().Exists(ctx, desc)
	if err != nil || exists {
		return err
	}
	from, parsed, err := Repository(source+"@"+desc.Digest.String(), credentials, WithPlainHTTP(destination.PlainHTTP))
	if err != nil {
		return err
	}
	getContent := func() (io.ReadCloser, error) { return from.Blobs().Fetch(ctx, desc) }
	if parsed.Registry == destination.Reference.Registry {
		err = destination.Mount(ctx, desc, parsed.Repository, getContent)
		if err == nil {
			return nil
		}
		// A destination writer may lack source-read permission. Some registries
		// reject the mount instead of offering an upload; stream with the two
		// separate accounts in that case.
		var response *errcode.ErrorResponse
		if !errors.As(err, &response) || (response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden) {
			return err
		}
	}
	stream, err := getContent()
	if err != nil {
		return err
	}
	defer stream.Close()
	return destination.Blobs().Push(ctx, desc, stream)
}

type localImage struct {
	Name   string `json:"name"`
	File   string `json:"manifest_file"`
	Digest string `json:"manifest_digest"`
}

func pushLocalImage(ctx context.Context, repo *remote.Repository, dir, tag string, image localImage, sources map[string]string, credentials CredentialFunc, annotations map[string]string) ([]byte, string, error) {
	if filepath.Base(image.File) != image.File {
		return nil, "", fmt.Errorf("invalid manifest file")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest", image.File))
	if err != nil {
		return nil, "", err
	}
	if digest.FromBytes(raw).String() != image.Digest {
		return nil, "", fmt.Errorf("manifest digest mismatch")
	}
	var manifest oci.Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, "", err
	}
	if len(annotations) != 0 {
		if manifest.Annotations == nil {
			manifest.Annotations = make(map[string]string)
		}
		for _, key := range []string{AnnotationSandboxUID, AnnotationNamespace, AnnotationCheckpointID} {
			delete(manifest.Annotations, key)
		}
		for key, value := range annotations {
			manifest.Annotations[key] = value
		}
		raw, err = json.Marshal(manifest)
		if err != nil {
			return nil, "", err
		}
		if len(raw) > 1<<20 {
			return nil, "", fmt.Errorf("snapfiles manifest exceeds 1 MiB metadata limit")
		}
	}
	for _, desc := range append([]oci.Descriptor{manifest.Config}, manifest.Layers...) {
		if err = pushLocalBlob(ctx, repo, dir, desc, sources, credentials); err != nil {
			return nil, "", err
		}
	}

	suffix := ""
	if strings.HasSuffix(image.Name, "_rootfs") {
		suffix = "_rootfs"
	} else if strings.HasSuffix(image.Name, "_snapfiles") {
		suffix = "_snapfiles"
	} else {
		return nil, "", fmt.Errorf("unknown pair image %q", image.Name)
	}
	desc := content.NewDescriptorFromBytes(oci.MediaTypeImageManifest, raw)
	if err = pushImmutableManifest(ctx, repo, tag+suffix, desc, raw); err != nil {
		return nil, "", err
	}
	immutable := repo.Reference.Registry + "/" + repo.Reference.Repository + "@" + desc.Digest.String()
	return raw, immutable, nil
}

func pushLocalBlob(ctx context.Context, repo *remote.Repository, dir string, desc oci.Descriptor, sources map[string]string, credentials CredentialFunc) error {
	if desc.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("unsupported blob digest")
	}
	if err := desc.Digest.Validate(); err != nil {
		return err
	}
	file, e := os.Open(filepath.Join(dir, "blobs/sha256", desc.Digest.Encoded()))
	if os.IsNotExist(e) && sources[desc.Digest.String()] != "" {
		if e = inheritBlob(ctx, repo, desc, sources[desc.Digest.String()], credentials); e != nil {
			return e
		}
		return nil
	}
	if e != nil {
		return e
	}
	verifier := desc.Digest.Verifier()
	n, e := io.Copy(verifier, file)
	if e != nil || n != desc.Size || !verifier.Verified() {
		file.Close()
		return fmt.Errorf("blob digest/size mismatch: %s", desc.Digest)
	}
	_, e = file.Seek(0, 0)
	if e == nil {
		var exists bool
		exists, e = repo.Blobs().Exists(ctx, desc)
		if e == nil && !exists {
			e = repo.Blobs().Push(ctx, desc, file)
		}
	}
	file.Close()
	if e != nil {
		return e
	}
	return nil
}

// BasicCredential keeps OCI metadata and native OverlayBD reads on the same
// authentication contract. Identity tokens alone must not become anonymous reads.
func BasicCredential(username, password, identityToken string) (auth.Credential, error) {
	if username == "" && password == "" && identityToken != "" {
		return auth.EmptyCredential, fmt.Errorf("OCI identitytoken-only credentials are not supported; use anonymous or username/password authentication")
	}
	return auth.Credential{Username: username, Password: password}, nil
}
