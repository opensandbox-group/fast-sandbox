package infra

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	infracatalog "fast-sandbox/internal/catalog/infra"

	"github.com/stretchr/testify/require"
)

type fakeOCIOpener struct {
	opened  infracatalog.Artifact
	payload []byte
}

func (o *fakeOCIOpener) OpenOCI(_ context.Context, artifact infracatalog.Artifact) (io.ReadCloser, error) {
	o.opened = artifact
	return io.NopCloser(bytes.NewReader(o.payload)), nil
}

type fakeSignatureVerifier struct {
	verified infracatalog.Artifact
	err      error
}

func (v *fakeSignatureVerifier) VerifyArtifact(_ context.Context, artifact infracatalog.Artifact) error {
	v.verified = artifact
	return v.err
}

func TestPlatformResolverStaticArtifactConfinedToPlatformRoot(t *testing.T) {
	root := t.TempDir()
	allowedRoot := filepath.Join(root, "platform")
	outsideRoot := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(allowedRoot, 0755))
	require.NoError(t, os.MkdirAll(outsideRoot, 0755))

	artifactPath := filepath.Join(allowedRoot, "component")
	require.NoError(t, os.WriteFile(artifactPath, []byte("component"), 0755))
	resolver := NewPlatformResolver([]string{allowedRoot})

	reader, err := resolver.Open(context.Background(), staticArtifact(artifactPath))
	require.NoError(t, err)
	payload, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, "component", string(payload))

	escapedTarget := filepath.Join(outsideRoot, "escaped")
	require.NoError(t, os.WriteFile(escapedTarget, []byte("escaped"), 0755))
	escapedLink := filepath.Join(allowedRoot, "escaped-link")
	require.NoError(t, os.Symlink(escapedTarget, escapedLink))

	_, err = resolver.Open(context.Background(), staticArtifact(escapedLink))
	require.ErrorIs(t, err, ErrArtifactSourceUnsupported)
}

func TestPlatformResolverRejectsBrokenStaticSymlink(t *testing.T) {
	allowedRoot := t.TempDir()
	brokenLink := filepath.Join(allowedRoot, "broken")
	require.NoError(t, os.Symlink(filepath.Join(allowedRoot, "missing"), brokenLink))

	_, err := NewPlatformResolver([]string{allowedRoot}).Open(context.Background(), staticArtifact(brokenLink))
	require.Error(t, err)
}

func TestPlatformResolverDelegatesOCIAndRunsSignaturePolicy(t *testing.T) {
	opener := &fakeOCIOpener{payload: []byte("oci bundle")}
	verifier := &fakeSignatureVerifier{}
	resolver := NewPlatformResolverWithOptions(PlatformResolverOptions{OCI: opener, SignatureVerifier: verifier})
	artifact := infracatalog.Artifact{
		SourceType: infracatalog.SourceOCIArtifact, Reference: "oci://registry/execd:v1",
		Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	reader, err := resolver.Open(context.Background(), artifact)
	require.NoError(t, err)
	require.Equal(t, "oci bundle", readAll(t, reader))
	require.Equal(t, artifact, opener.opened)
	require.Equal(t, artifact, verifier.verified)

	verifier.err = errors.New("unsigned")
	_, err = resolver.Open(context.Background(), artifact)
	require.ErrorContains(t, err, "unsigned")
}

func readAll(t *testing.T, reader io.ReadCloser) string {
	t.Helper()
	defer reader.Close()
	payload, err := io.ReadAll(reader)
	require.NoError(t, err)
	return string(payload)
}

func staticArtifact(path string) infracatalog.Artifact {
	return infracatalog.Artifact{
		SourceType: infracatalog.SourceStatic,
		Reference:  "file://" + path,
	}
}
