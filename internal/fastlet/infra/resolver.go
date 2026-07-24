package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	infracatalog "fast-sandbox/internal/catalog/infra"
)

var ErrArtifactSourceUnsupported = errors.New("Infra artifact source is unsupported")

type ArtifactResolver interface {
	Open(context.Context, infracatalog.Artifact) (io.ReadCloser, error)
}

// OCIArtifactOpener binds the platform's chosen OCI client (containerd, ORAS,
// or a release-local mirror) without making registry access part of the
// Sandbox create path.
type OCIArtifactOpener interface {
	OpenOCI(context.Context, infracatalog.Artifact) (io.ReadCloser, error)
}

// ArtifactSignatureVerifier is the policy hook for release signatures or
// attestations. Content digest verification is always enforced by
// ArtifactStore independently of this hook.
type ArtifactSignatureVerifier interface {
	VerifyArtifact(context.Context, infracatalog.Artifact) error
}

type PlatformResolverOptions struct {
	StaticRoots       []string
	OCI               OCIArtifactOpener
	SignatureVerifier ArtifactSignatureVerifier
}

type PlatformResolver struct {
	embedded          map[string][]byte
	staticRoots       []string
	oci               OCIArtifactOpener
	signatureVerifier ArtifactSignatureVerifier
}

func NewPlatformResolver(staticRoots []string) *PlatformResolver {
	return NewPlatformResolverWithOptions(PlatformResolverOptions{StaticRoots: staticRoots})
}

func NewPlatformResolverWithOptions(options PlatformResolverOptions) *PlatformResolver {
	resolver := &PlatformResolver{
		embedded: map[string][]byte{
			"embedded://test-infra/v1": []byte(infracatalog.TestInfraScript),
		},
		oci: options.OCI, signatureVerifier: options.SignatureVerifier,
	}
	for _, root := range options.StaticRoots {
		if filepath.IsAbs(root) {
			resolver.staticRoots = append(resolver.staticRoots, filepath.Clean(root))
		}
	}
	return resolver
}

func (r *PlatformResolver) Open(ctx context.Context, artifact infracatalog.Artifact) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.signatureVerifier != nil {
		if err := r.signatureVerifier.VerifyArtifact(ctx, artifact); err != nil {
			return nil, fmt.Errorf("verify Infra artifact signature: %w", err)
		}
	}
	switch artifact.SourceType {
	case infracatalog.SourceEmbedded:
		payload, ok := r.embedded[artifact.Reference]
		if !ok {
			return nil, fmt.Errorf("%w: embedded reference %s", ErrArtifactSourceUnsupported, artifact.Reference)
		}
		return io.NopCloser(bytes.NewReader(payload)), nil
	case infracatalog.SourceStatic:
		path := strings.TrimPrefix(artifact.Reference, "file://")
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		if !r.allowed(resolved) {
			return nil, fmt.Errorf("%w: static path %s is outside platform roots", ErrArtifactSourceUnsupported, resolved)
		}
		return os.Open(resolved)
	case infracatalog.SourcePreinstalled:
		return nil, fmt.Errorf("%w: preinstalled artifacts are not opened", ErrArtifactSourceUnsupported)
	case infracatalog.SourceOCIArtifact:
		if r.oci == nil {
			return nil, fmt.Errorf("%w: OCI resolver is not configured for %s", ErrArtifactSourceUnsupported, artifact.Reference)
		}
		return r.oci.OpenOCI(ctx, artifact)
	default:
		return nil, fmt.Errorf("%w: source type %s", ErrArtifactSourceUnsupported, artifact.SourceType)
	}
}

func (r *PlatformResolver) allowed(path string) bool {
	for _, root := range r.staticRoots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
