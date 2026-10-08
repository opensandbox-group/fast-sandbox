package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// This file is the ORAS-based registry access layer of template-vm
// (ADR-003): the create flow resolves manifests only (lowers stay remote and
// overlaybd range-reads them), while the build flow additionally downloads
// the source manifest and every layer blob into the local artifact layout so
// the build-time rootfs device reads local files only. No docker CLI and no
// go-containerregistry are involved.

// remoteSource abstracts the registry operations both flows need so tests can
// substitute an in-memory ORAS store. remote.Repository satisfies it.
type remoteSource interface {
	Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error)
	Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error)
}

// canonicalRegistryHost maps the Docker Hub aliases to the real distribution
// endpoint. The user-facing spelling (registry.hub.docker.com) is the Hub UI
// host, not a registry endpoint, so requests must go to registry-1.docker.io.
func canonicalRegistryHost(host string) string {
	switch host {
	case "docker.io", "index.docker.io", "registry.hub.docker.com", "registry-1.docker.io":
		return "registry-1.docker.io"
	default:
		return host
	}
}

// repoBlobURL builds the <scheme>://<registry>/v2/<repo>/blobs base that
// overlaybd range-reads against for remote lowers (create flow).
func repoBlobURL(ref registry.Reference, plainHTTP bool) string {
	scheme := "https"
	if plainHTTP {
		scheme = "http"
	}
	host := canonicalRegistryHost(ref.Registry)
	return scheme + "://" + path.Join(host, "v2", ref.Repository) + "/blobs"
}

// parseRegistryReference parses ref and canonicalizes the Docker Hub aliases.
func parseRegistryReference(ref string) (registry.Reference, error) {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return registry.Reference{}, fmt.Errorf("parse image reference %q: %w", ref, err)
	}
	if parsed.Reference == "" {
		return registry.Reference{}, fmt.Errorf("image reference %q has no tag or digest", ref)
	}
	parsed.Registry = canonicalRegistryHost(parsed.Registry)
	return parsed, nil
}

// credential decodes the docker config entry into an ORAS credential,
// preferring the explicit username/password pair over the base64 auth token.
func (e authEntry) credential() (auth.Credential, error) {
	if e.Username != "" || e.Password != "" {
		return auth.Credential{Username: e.Username, Password: e.Password}, nil
	}
	if e.Auth == "" {
		return auth.EmptyCredential, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(e.Auth)
	if err != nil {
		return auth.EmptyCredential, fmt.Errorf("decode auth token: %w", err)
	}
	username, password, found := strings.Cut(string(decoded), ":")
	if !found {
		return auth.EmptyCredential, fmt.Errorf("malformed auth token: missing ':'")
	}
	return auth.Credential{Username: username, Password: password}, nil
}

// credentialFor selects the credential for ref from the docker config,
// matching the longest registry/repository prefix. Registry keys are
// canonicalized like the reference host (scheme and API suffix stripped,
// Docker Hub aliases mapped) so the standard "https://index.docker.io/v1/"
// docker-login entry matches a registry.hub.docker.com reference. The bool
// reports whether a non-anonymous credential was found.
func credentialFor(config dockerConfig, ref registry.Reference) (auth.Credential, bool, error) {
	host := canonicalRegistryHost(ref.Registry)
	full := path.Join(host, ref.Repository)
	best := ""
	bestLength := 0
	for key := range config.Auths {
		keyHost, keyRepo, _ := strings.Cut(normalizeAuthKey(key), "/")
		keyHost = canonicalRegistryHost(keyHost)
		keyRepo = strings.TrimSuffix(keyRepo, "/")
		if keyRepo == "v1" || keyRepo == "v2" {
			keyRepo = ""
		}
		var match string
		switch {
		case keyRepo == "" && keyHost == host:
			match = keyHost
		case keyRepo != "" && (full == keyHost+"/"+keyRepo || strings.HasPrefix(full+"/", keyHost+"/"+keyRepo+"/")):
			match = keyHost + "/" + keyRepo
		default:
			continue
		}
		if len(match) > bestLength {
			best, bestLength = key, len(match)
		}
	}
	if best == "" {
		return auth.EmptyCredential, false, nil
	}
	credential, err := config.Auths[best].credential()
	if err != nil {
		return auth.EmptyCredential, false, err
	}
	return credential, true, nil
}

// newRemoteRepository builds an authenticated ORAS repository handle for ref.
func newRemoteRepository(ref registry.Reference, config dockerConfig, plainHTTP bool) (*remote.Repository, error) {
	repository, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, fmt.Errorf("open repository %s/%s: %w", ref.Registry, ref.Repository, err)
	}
	repository.PlainHTTP = plainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	credential, found, err := credentialFor(config, ref)
	if err != nil {
		return nil, err
	}
	if found {
		client.Credential = auth.StaticCredential(ref.Registry, credential)
	}
	repository.Client = client
	return repository, nil
}

// isIndexMediaType reports whether desc references an OCI image index or a
// docker manifest list.
func isIndexMediaType(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex ||
		mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// selectPlatformManifest picks the linux/<host arch> entry of an image
// index; the error lists the available platforms when none matches.
func selectPlatformManifest(index ocispec.Index) (ocispec.Descriptor, error) {
	wantArch := runtime.GOARCH
	available := make([]string, 0, len(index.Manifests))
	for _, manifest := range index.Manifests {
		if manifest.Platform == nil {
			available = append(available, "no-platform")
			continue
		}
		platform := manifest.Platform
		available = append(available, platform.OS+"/"+platform.Architecture)
		if platform.OS == "linux" && platform.Architecture == wantArch {
			return manifest, nil
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("no manifest for linux/%s; available: [%s]",
		wantArch, strings.Join(available, ", "))
}

// fetchManifestJSON resolves reference on source and returns the platform
// manifest, traversing one index level when the tag points at an index. The
// returned bytes are digest-verified (content.FetchAll checks the descriptor).
func fetchManifestJSON(ctx context.Context, source remoteSource, reference string) (ocispec.Descriptor, []byte, error) {
	desc, err := source.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("resolve %q: %w", reference, err)
	}
	if err := validateSHA256Descriptor(desc, "resolved manifest"); err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	raw, err := content.FetchAll(ctx, source, desc)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("fetch %s: %w", desc.Digest, err)
	}
	if !isIndexMediaType(desc.MediaType) {
		return desc, raw, nil
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("parse image index %s: %w", desc.Digest, err)
	}
	child, err := selectPlatformManifest(index)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	if err := validateSHA256Descriptor(child, "platform manifest"); err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	raw, err = content.FetchAll(ctx, source, child)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("fetch platform manifest %s: %w", child.Digest, err)
	}
	return child, raw, nil
}

type buildSourceKind uint8

const (
	buildSourceNative buildSourceKind = iota
	buildSourceOCI
)

type resolvedBuildSource struct {
	Ref          registry.Reference
	ManifestDesc ocispec.Descriptor
	ManifestRaw  []byte
	Manifest     ocispec.Manifest
}

func validateManifestDescriptors(manifest ocispec.Manifest, imageRef string) error {
	if err := validateSHA256Descriptor(manifest.Config, "image config"); err != nil {
		return fmt.Errorf("image %q: %w", imageRef, err)
	}
	for index, layer := range manifest.Layers {
		if err := validateSHA256Descriptor(layer, fmt.Sprintf("layer %d", index)); err != nil {
			return fmt.Errorf("image %q: %w", imageRef, err)
		}
	}
	return nil
}

func resolveBuildSource(ctx context.Context, source remoteSource, ref registry.Reference) (resolvedBuildSource, error) {
	manifestDesc, raw, err := fetchManifestJSON(ctx, source, ref.Reference)
	if err != nil {
		return resolvedBuildSource{}, fmt.Errorf("pull manifest %q: %w", ref.String(), err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return resolvedBuildSource{}, fmt.Errorf("parse manifest %q: %w", ref.String(), err)
	}
	if err := validateManifestDescriptors(manifest, ref.String()); err != nil {
		return resolvedBuildSource{}, err
	}
	return resolvedBuildSource{
		Ref:          ref,
		ManifestDesc: manifestDesc,
		ManifestRaw:  raw,
		Manifest:     manifest,
	}, nil
}

// resolveImage is the create-flow manifest resolver (ADR-003): it fetches
// only the manifest and returns the ordered (bottom-up) lower layers plus the
// repoBlobUrl overlaybd range-reads from. No blob is downloaded.
func resolveImage(ctx context.Context, imageRef string, config dockerConfig) (resolvedImage, error) {
	ref, err := parseRegistryReference(imageRef)
	if err != nil {
		return resolvedImage{}, err
	}
	repository, err := newRemoteRepository(ref, config, global.plainHTTP)
	if err != nil {
		return resolvedImage{}, err
	}
	_, raw, err := fetchManifestJSON(ctx, repository, ref.Reference)
	if err != nil {
		return resolvedImage{}, fmt.Errorf("pull manifest %q: %w", imageRef, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return resolvedImage{}, fmt.Errorf("parse manifest %q: %w", imageRef, err)
	}
	if err := validateManifestDescriptors(manifest, imageRef); err != nil {
		return resolvedImage{}, err
	}
	lowers := make([]lowerLayer, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		lowers = append(lowers, lowerLayer{Digest: layer.Digest.String(), Size: layer.Size})
	}
	if len(lowers) == 0 {
		return resolvedImage{}, fmt.Errorf("image %q has no layers", imageRef)
	}
	return resolvedImage{Lowers: lowers, RepoBlobURL: repoBlobURL(ref, global.plainHTTP)}, nil
}

// ---- overlaybd layer classification (build-flow source validation) ----

// Media type / annotation conventions of accelerated-container-image,
// mirrored from AgentENV's classifier (src/image/oci_image.rs).
const (
	turboArtifactType          = "application/vnd.containerd.overlaybd.turbo.v1+json"
	turboAnnotationPrefix      = "containerd.io/snapshot/overlaybd/turbo-oci/"
	overlaybdVersionAnnotation = "containerd.io/snapshot/overlaybd/version"
	overlaybdBlobDigestAnnot   = "containerd.io/snapshot/overlaybd/blob-digest"

	// overlaybdZfileLayerMediaType / overlaybdPlainLayerMediaType are the
	// mediaTypes stamped on locally committed layers (zfile-compressed or
	// plain LSMT).
	overlaybdZfileLayerMediaType = "application/vnd.containerd.overlaybd.image.layer.v1.zfile"
	overlaybdPlainLayerMediaType = "application/vnd.containerd.overlaybd.image.layer.v1.lsmt"
)

// isStandardTarMediaType covers the OCI and docker tar layer spellings.
func isStandardTarMediaType(mediaType string) bool {
	switch mediaType {
	case ocispec.MediaTypeImageLayer,
		ocispec.MediaTypeImageLayerGzip,
		ocispec.MediaTypeImageLayerZstd,
		"application/vnd.docker.image.rootfs.diff.tar",
		"application/vnd.docker.image.rootfs.diff.tar.gzip":
		return true
	default:
		return false
	}
}

// isOverlayBDNativeMediaType matches the overlaybd/zfile mediaType family on
// component boundaries (same token rule as AgentENV).
func isOverlayBDNativeMediaType(mediaType string) bool {
	for _, token := range []string{".overlaybd", "/overlaybd", "+overlaybd", ".zfile", "/zfile", "+zfile"} {
		if strings.Contains(mediaType, token) {
			return true
		}
	}
	return false
}

// isNativeLayer reports whether a layer blob IS a sealed overlaybd lower,
// either by native mediaType or by the mediaType-lying convention (tar
// mediaType + blob-digest annotation equal to the layer's own digest).
func isNativeLayer(layer ocispec.Descriptor) bool {
	if isOverlayBDNativeMediaType(layer.MediaType) {
		return true
	}
	if !isStandardTarMediaType(layer.MediaType) {
		return false
	}
	annotated, ok := layer.Annotations[overlaybdBlobDigestAnnot]
	return ok && annotated == layer.Digest.String()
}

// isTurboLayer detects turbo-OCI layers, whose blobs carry only index
// metadata; overlaybd cannot mount them without the original OCI layers.
func isTurboLayer(layer ocispec.Descriptor) bool {
	for key := range layer.Annotations {
		if strings.HasPrefix(key, turboAnnotationPrefix) {
			return true
		}
	}
	if version, ok := layer.Annotations[overlaybdVersionAnnotation]; ok && strings.Contains(version, "turbo") {
		return true
	}
	return false
}

func classifyBuildSource(manifest ocispec.Manifest, imageRef string) (buildSourceKind, error) {
	if manifest.ArtifactType == turboArtifactType {
		return 0, fmt.Errorf("image %q is a turbo-OCI artifact (artifactType=%s); not supported", imageRef, turboArtifactType)
	}
	if len(manifest.Layers) == 0 {
		return 0, fmt.Errorf("image %q has no layers", imageRef)
	}
	nativeLayers := 0
	ociLayers := 0
	for index, layer := range manifest.Layers {
		switch {
		case isTurboLayer(layer):
			return 0, fmt.Errorf("image %q layer %d (%s) is turbo-OCI; not supported", imageRef, index, layer.Digest)
		case isNativeLayer(layer):
			nativeLayers++
		case isStandardTarMediaType(layer.MediaType):
			if annotated, ok := layer.Annotations[overlaybdBlobDigestAnnot]; ok {
				return 0, fmt.Errorf("image %q layer %d (%s) is a tar-wrapped overlaybd blob (blob-digest=%s); not supported",
					imageRef, index, layer.Digest, annotated)
			}
			if version, ok := layer.Annotations[overlaybdVersionAnnotation]; ok {
				return 0, fmt.Errorf("image %q layer %d (%s) is a tar-wrapped overlaybd blob (version=%s); not supported",
					imageRef, index, layer.Digest, version)
			}
			ociLayers++
		default:
			return 0, fmt.Errorf("image %q layer %d (%s) has an unsupported mediaType %q",
				imageRef, index, layer.Digest, layer.MediaType)
		}
	}
	if nativeLayers != 0 && ociLayers != 0 {
		return 0, fmt.Errorf("image %q mixes native overlaybd and standard OCI tar layers; not supported", imageRef)
	}
	if nativeLayers == len(manifest.Layers) {
		return buildSourceNative, nil
	}
	return buildSourceOCI, nil
}

// validateOverlayBDSource requires every layer to be a sealed overlaybd lower.
func validateOverlayBDSource(manifest ocispec.Manifest, imageRef string) error {
	kind, err := classifyBuildSource(manifest, imageRef)
	if err != nil {
		return err
	}
	if kind == buildSourceOCI {
		layer := manifest.Layers[0]
		return fmt.Errorf("image %q layer 0 (%s) is a standard OCI tar layer (%s), not an overlaybd image",
			imageRef, layer.Digest, layer.MediaType)
	}
	return nil
}

// ---- build-flow download ----

// fetchedImage is the build-side outcome of downloading the source image:
// the manifest JSON stored under manifest/ and every layer blob stored under
// blobs/sha256/ of the artifact layout.
type fetchedImage struct {
	SourceRef    string
	ManifestDesc ocispec.Descriptor
	ManifestPath string
	Layers       []ocispec.Descriptor
	Layout       artifactLayout
}

// blobPath returns the local path of a downloaded layer blob.
func (f fetchedImage) blobPath(layerDigest digest.Digest) string {
	return f.Layout.blobPath(layerDigest)
}

// downloadBlob streams desc from fetcher into blobDir/<encoded digest>,
// verifying size and digest. The write is atomic (tmp+rename), and an
// already-present blob is fully verified before reuse.
func downloadBlob(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor, blobDir string) (string, error) {
	if err := validateSHA256Descriptor(desc, "blob descriptor"); err != nil {
		return "", err
	}
	final := filepath.Join(blobDir, desc.Digest.Encoded())
	if _, err := os.Stat(final); err == nil {
		if err := checkFileDescriptor(final, desc); err != nil {
			return "", fmt.Errorf("verify existing blob %s: %w", desc.Digest, err)
		}
		return final, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat blob %s: %w", desc.Digest, err)
	}
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		return "", fmt.Errorf("create blob directory %s: %w", blobDir, err)
	}
	reader, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return "", fmt.Errorf("fetch blob %s: %w", desc.Digest, err)
	}
	defer reader.Close()

	temp, err := os.CreateTemp(blobDir, ".blob-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	verifier := desc.Digest.Verifier()
	written, err := io.Copy(temp, io.TeeReader(reader, verifier))
	if err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("download blob %s: %w", desc.Digest, err)
	}
	if written != desc.Size {
		_ = temp.Close()
		return "", fmt.Errorf("blob %s size mismatch: got %d, want %d", desc.Digest, written, desc.Size)
	}
	if !verifier.Verified() {
		_ = temp.Close()
		return "", fmt.Errorf("blob %s digest verification failed", desc.Digest)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temp blob: %w", err)
	}
	if err := os.Rename(tempName, final); err != nil {
		return "", fmt.Errorf("commit blob %s: %w", final, err)
	}
	return final, nil
}

// fetchSourceImage downloads an already-resolved native source manifest,
// config, and layers into the artifact layout.
func fetchSourceImage(ctx context.Context, source remoteSource, resolved resolvedBuildSource, layout artifactLayout) (fetchedImage, error) {
	if err := layout.ensure(); err != nil {
		return fetchedImage{}, err
	}
	if err := verifyDescriptorBytes(resolved.ManifestDesc, resolved.ManifestRaw, "source manifest"); err != nil {
		return fetchedImage{}, err
	}
	manifestPath := layout.manifestPath(sourceManifestName(resolved.Ref.Reference))
	if err := writeFileAtomic(manifestPath, resolved.ManifestRaw, 0o640); err != nil {
		return fetchedImage{}, fmt.Errorf("write source manifest %s: %w", manifestPath, err)
	}
	if _, err := downloadBlob(ctx, source, resolved.Manifest.Config, layout.blobDir()); err != nil {
		return fetchedImage{}, fmt.Errorf("download source config: %w", err)
	}
	for index, layer := range resolved.Manifest.Layers {
		if _, err := downloadBlob(ctx, source, layer, layout.blobDir()); err != nil {
			return fetchedImage{}, fmt.Errorf("download layer %d/%d: %w", index+1, len(resolved.Manifest.Layers), err)
		}
	}
	return fetchedImage{
		SourceRef:    resolved.Ref.String(),
		ManifestDesc: resolved.ManifestDesc,
		ManifestPath: manifestPath,
		Layers:       resolved.Manifest.Layers,
		Layout:       layout,
	}, nil
}

// localLowers converts downloaded layers into config.v1.json lower entries
// addressed by local file path (build flow: no remote range reads).
func localLowers(fetched fetchedImage) []OverlayBDBSConfigLower {
	lowers := make([]OverlayBDBSConfigLower, 0, len(fetched.Layers))
	for _, layer := range fetched.Layers {
		lowers = append(lowers, OverlayBDBSConfigLower{File: fetched.blobPath(layer.Digest)})
	}
	return lowers
}
