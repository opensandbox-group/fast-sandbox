package agent

// publish.go implements the node-side artifact-set publication for live
// Sandbox snapshots. The layout and upload order are byte-identical to the
// golden-image builder (cmd/sandboxtemplate-builder publish stage): the
// per-build namespace is the first 16 hex chars of the manifest document's
// SHA-256, artifacts and SHA256SUMS upload first, the manifest last within
// the namespace, and the image index object last overall. Unlike a
// SandboxTemplate build, a snapshot publishes ONLY the index key given by
// the caller (the template name) — never a default sha256(sourceImage) key,
// which would alias and pollute the source golden image (last-writer-wins).

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fast-sandbox/internal/artifacts"
)

// publishArtifacts are the artifact files of a native (non-overlaybd) set,
// uploaded in this order before the manifest.
var publishArtifacts = []string{"rootfs.ext4", "vmstate.snap", "memory.snap"}

// publishManifestName is the commit-point document of the artifact set.
const publishManifestName = "manifest.json"

// PublishResult reports one successful publication.
type PublishResult struct {
	ManifestRef    string
	ArtifactDigest string
}

// PublishImage uploads the artifact set staged in dir to the configured
// store under key and returns the manifest reference and digest. It is
// synchronous: multi-GiB memory files legitimately take minutes.
func (c *Client) PublishImage(ctx context.Context, key, dir string) (PublishResult, error) {
	if strings.TrimSpace(key) == "" {
		return PublishResult{}, fmt.Errorf("publish key is required")
	}
	manifestBytes, err := os.ReadFile(filepath.Join(dir, publishManifestName))
	if err != nil {
		return PublishResult{}, fmt.Errorf("read staged manifest: %w", err)
	}
	artifactDigest := artifacts.SHA256Of(manifestBytes)
	base := artifacts.Digest16(manifestBytes)

	// Artifacts and SHA256SUMS first: the namespace stays incomplete (and
	// unaddressable) until the manifest lands.
	entries := append([]string(nil), publishArtifacts...)
	entries = append(entries, artifacts.SHA256SUMSName)
	for _, name := range entries {
		if err := c.putFile(ctx, dir, name, base+"/"+name); err != nil {
			return PublishResult{}, fmt.Errorf("publish %s: %w", name, err)
		}
	}
	manifestURI := c.s3.storeRootURI() + "/" + base + "/" + publishManifestName
	if err := c.putFile(ctx, dir, publishManifestName, base+"/"+publishManifestName); err != nil {
		return PublishResult{}, fmt.Errorf("publish manifest: %w", err)
	}

	// The index is written last overall: a consumer that resolves the key is
	// guaranteed a complete artifact set. The payload's image field equals
	// the key so pull-side byte matching works unchanged.
	indexPayload, err := artifacts.ImageIndexPayload(key, manifestURI, artifactDigest, nowFunc())
	if err != nil {
		return PublishResult{}, err
	}
	indexKey := "index/" + artifacts.ImageIndexKey(key) + ".json"
	payload := string(indexPayload)
	if err := c.s3.put(ctx, indexKey, int64(len(payload)), func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(payload)), nil
	}); err != nil {
		return PublishResult{}, fmt.Errorf("publish index: %w", err)
	}
	return PublishResult{ManifestRef: manifestURI, ArtifactDigest: artifactDigest}, nil
}

// putFile streams one staged file into the store under a store-relative
// key. The opener reopens the file per attempt (net/http closes request
// bodies), so a transient failure retries against a fresh reader.
func (c *Client) putFile(ctx context.Context, dir, name, storeKey string) error {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return c.s3.put(ctx, storeKey, info.Size(), func() (io.ReadCloser, error) {
		return os.Open(path)
	})
}

// now returns the publish clock; factored for tests.
var nowFunc = time.Now
