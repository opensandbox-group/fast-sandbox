package agent

// publish.go implements the node-side artifact-set publication for live
// Sandbox snapshots. The layout and upload order are byte-identical to the
// golden-image builder (cmd/sandboxtemplate-builder publish stage): the
// per-build namespace is the first 16 hex chars of the manifest document's
// SHA-256, artifacts and SHA256SUMS upload first, the manifest last within
// the namespace, and the image index object last overall. Unlike a
// SandboxTemplate build, a snapshot publishes ONLY the index key given by
// the caller (the template name) — never a default sha256(image) key,
// which would alias and pollute the source golden image (last-writer-wins).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// publishArtifacts are the artifact files of a native (non-overlaybd) set,
// uploaded in this order before the manifest.
var publishArtifacts = []string{"rootfs.ext4", manifestVMStateName, manifestMemoryName}

// publishVaultName / publishPolicyName are the optional state bundle files
// of a snapshot set (checkpoint vault; recorded policy on checkpoint AND
// template sets): uploaded when staged next to the manifest. The names are
// owned by the shared protocol package — the driver stages and the pull
// layer materializes the same bytes under the same names.
const (
	publishVaultName  = agentprotocol.VaultBundleName
	publishPolicyName = agentprotocol.PolicyBundleName
)

// publishManifestName is the commit-point document of the artifact set.
const publishManifestName = "manifest.json"

// PublishResult reports one successful publication.
type PublishResult struct {
	ManifestRef    string
	ArtifactDigest string
}

// PublishImage uploads the artifact set staged in dir to the configured
// store and returns the manifest reference and digest. It is synchronous:
// multi-GiB memory files legitimately take minutes. kind selects the
// publication mode: a template set (default) requires key and writes the
// image index last; a checkpoint set carries no key and writes no index —
// the instance-private set is addressed by its returned ref + digest.
func (c *Client) PublishImage(ctx context.Context, kind, key, dir string) (PublishResult, error) {
	if kind == "" {
		kind = agentprotocol.PublishKindTemplate
	}
	switch kind {
	case agentprotocol.PublishKindTemplate:
		if strings.TrimSpace(key) == "" {
			return PublishResult{}, fmt.Errorf("publish key is required for a template artifact set")
		}
	case agentprotocol.PublishKindCheckpoint:
		if strings.TrimSpace(key) != "" {
			return PublishResult{}, fmt.Errorf("checkpoint artifact sets publish no image index; key must be empty")
		}
	default:
		return PublishResult{}, fmt.Errorf("unknown publish kind %q", kind)
	}
	started := time.Now()
	manifestBytes, chunkIndex, err := c.prepareManifest(ctx, kind, key, dir, started)
	if err != nil {
		return PublishResult{}, err
	}
	artifactDigest := artifacts.SHA256Of(manifestBytes)
	base := artifacts.Digest16(manifestBytes)
	klog.InfoS("runtime-agent publish started", "kind", kind, "key", key, "digest", artifactDigest, "format", c.transfer.normalized().Format, "stagingDir", dir)
	if c.transfer.Chunked() {
		// The staged files are exactly the bytes the manifest describes; a
		// driver that commits them into its cache (same-node restore) then
		// passes the agent's cache check without a re-hash.
		defer markStagedVerified(dir, manifestBytes)
	}

	// Artifacts and SHA256SUMS first: the namespace stays incomplete (and
	// unaddressable) until the manifest lands. The optional state bundle
	// files (vault, recorded policy) join the artifact uploads when staged —
	// any stat failure other than absence fails the publish, since the
	// staged manifest and SHA256SUMS already reference the files and a
	// stranded member would break the cross-node resume. A chunked set
	// already uploaded its native artifacts as chunks.
	var entries []string
	if c.transfer.Chunked() {
		if err := c.s3.putBytes(ctx, base+"/"+artifacts.ChunkIndexName, chunkIndex); err != nil {
			klog.ErrorS(err, "runtime-agent publish failed", "kind", kind, "key", key, "file", artifacts.ChunkIndexName, "elapsed", time.Since(started).String())
			return PublishResult{}, fmt.Errorf("publish chunk index: %w", err)
		}
	} else {
		entries = append(entries, publishArtifacts...)
	}
	for _, bundleName := range []string{publishVaultName, publishPolicyName} {
		if _, err := os.Stat(filepath.Join(dir, bundleName)); err == nil {
			entries = append(entries, bundleName)
		} else if !errors.Is(err, os.ErrNotExist) {
			klog.ErrorS(err, "runtime-agent publish failed", "kind", kind, "key", key, "file", bundleName)
			return PublishResult{}, fmt.Errorf("stat staged %s: %w", bundleName, err)
		}
	}
	entries = append(entries, artifacts.SHA256SUMSName)
	for _, name := range entries {
		if err := c.putFile(ctx, dir, name, base+"/"+name); err != nil {
			klog.ErrorS(err, "runtime-agent publish failed", "kind", kind, "key", key, "file", name, "elapsed", time.Since(started).String())
			return PublishResult{}, fmt.Errorf("publish %s: %w", name, err)
		}
	}
	manifestURI := c.s3.storeRootURI() + "/" + base + "/" + publishManifestName
	if err := c.putFile(ctx, dir, publishManifestName, base+"/"+publishManifestName); err != nil {
		klog.ErrorS(err, "runtime-agent publish failed", "kind", kind, "key", key, "file", publishManifestName, "elapsed", time.Since(started).String())
		return PublishResult{}, fmt.Errorf("publish manifest: %w", err)
	}
	if kind == agentprotocol.PublishKindCheckpoint {
		// No index: the set is complete and addressed by ref + digest only.
		klog.InfoS("runtime-agent publish completed", "kind", kind, "key", key, "manifestRef", manifestURI, "elapsed", time.Since(started).String())
		return PublishResult{ManifestRef: manifestURI, ArtifactDigest: artifactDigest}, nil
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
		klog.ErrorS(err, "runtime-agent publish failed", "kind", kind, "key", key, "file", indexKey, "elapsed", time.Since(started).String())
		return PublishResult{}, fmt.Errorf("publish index: %w", err)
	}
	klog.InfoS("runtime-agent publish completed", "kind", kind, "key", key, "manifestRef", manifestURI, "elapsed", time.Since(started).String())
	return PublishResult{ManifestRef: manifestURI, ArtifactDigest: artifactDigest}, nil
}

// prepareManifest returns the manifest bytes the set publishes under. A
// legacy set publishes the staged manifest as is — only a chunk binding
// inherited from the manifest of the image the sandbox booted from is
// dropped, since the set carries whole files. A chunked set first uploads
// its content-addressed chunks, then binds their index (returned for the
// upload into the set's namespace) into the manifest: the manifest digest,
// and with it the namespace, is only known after.
func (c *Client) prepareManifest(ctx context.Context, kind, key, dir string, started time.Time) ([]byte, []byte, error) {
	if !c.transfer.Chunked() {
		manifestBytes, err := bindChunkIndex(dir, nil)
		if err != nil {
			return nil, nil, err
		}
		return manifestBytes, nil, nil
	}
	index, stats, err := c.publishChunks(ctx, dir, publishArtifacts)
	if err != nil {
		klog.ErrorS(err, "runtime-agent publish failed", "kind", kind, "key", key, "stage", "chunks", "elapsed", time.Since(started).String())
		return nil, nil, fmt.Errorf("publish chunks: %w", err)
	}
	logPublishStats(kind, key, stats)
	indexBytes, err := artifacts.MarshalChunkIndex(index)
	if err != nil {
		return nil, nil, err
	}
	manifestBytes, err := bindChunkIndex(dir, &artifacts.ChunkIndexRef{
		Version: artifacts.ChunkIndexVersion, Ref: artifacts.ChunkIndexName,
		SHA256: sha256Hex(indexBytes), SizeBytes: int64(len(indexBytes)),
	})
	if err != nil {
		return nil, nil, err
	}
	return manifestBytes, indexBytes, nil
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

// markStagedVerified records the manifest digests of the staged artifacts as
// verified markers. Best-effort: a failure only costs a re-hash later.
func markStagedVerified(dir string, manifestBytes []byte) {
	document, err := parseManifest(manifestBytes)
	if err != nil {
		return
	}
	for _, name := range publishArtifacts {
		if entry, ok := document.Files[name]; ok && validSHA256(entry.SHA256) {
			markVerified(filepath.Join(dir, name), entry.SHA256)
		}
	}
}

// nowFunc is the publish clock; factored for tests.
var nowFunc = time.Now
