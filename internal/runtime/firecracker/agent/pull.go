package agent

// PullImage chain: SandboxSpec.Image -> index ->
// manifest -> digest-verified download into
// <StateRoot>/images/<sha256(image)>/.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// Client pulls published Firecracker artifacts for an image reference into
// the node-local cache. With a P2P peer gateway (WithPeerGateway) artifact
// bytes route through it, falling back to direct header-signed S3.
type Client struct {
	s3 *s3Client
	// peer is the P2P peer gateway; nil keeps pulls on the direct path.
	peer *peerGateway
}

// ReadImageManifest fetches the published manifest document of an image
// reference WITHOUT touching the node cache: the index (byte-verified
// image field) then the manifest (digest-verified). The control plane
// uses it to resolve snapshot-recorded policy at sandbox create time; the
// document itself is immutable per digest, so callers may cache forever
// keyed by the manifest's digest namespace.
func (c *Client) ReadImageManifest(ctx context.Context, image string) ([]byte, error) {
	index, err := fetchIndex(ctx, c.s3, image)
	if err != nil {
		return nil, err
	}
	manifestKey, err := c.s3.resolveRef(index.ManifestRef)
	if err != nil {
		return nil, err
	}
	return c.fetchManifest(ctx, manifestKey, index.ArtifactDigest)
}

// peerGateway routes artifact GETs through a P2P peer gateway (the
// node-local DART daemon, or an external provider) speaking
// GET <base><routePrefix><presigned-url>.
type peerGateway struct {
	base        string // e.g. http://127.0.0.1:8145
	routePrefix string // e.g. "/dart/"
	http        *http.Client
}

// NewClient builds a pull client for a store root (s3://bucket/prefix) with
// a read-only access key pair. The credential Host names the store endpoint
// and Username/Password carry the access key pair.
func NewClient(storeRoot string, credential registryconfig.Credential, options ...Option) (*Client, error) {
	config := optionsConfig{}
	for _, option := range options {
		option(&config)
	}
	s3, err := newS3Client(storeRoot, credential, config.httpClient, config.region, config.endpoint)
	if err != nil {
		return nil, err
	}
	client := &Client{s3: s3}
	if config.peerBase != "" {
		httpClient := config.httpClient
		if httpClient == nil {
			httpClient = &http.Client{Timeout: defaultS3RequestTimeout}
		}
		client.peer = &peerGateway{
			base:        strings.TrimRight(config.peerBase, "/"),
			routePrefix: normalizePeerRoutePrefix(config.peerRoutePrefix),
			http:        httpClient,
		}
	}
	return client, nil
}

// Option configures the pull client.
type Option func(*optionsConfig)

type optionsConfig struct {
	region          string
	endpoint        string
	httpClient      *http.Client
	peerBase        string
	peerRoutePrefix string
}

// WithRegion overrides the SigV4 signing region (default us-east-1).
func WithRegion(region string) Option {
	return func(config *optionsConfig) { config.region = region }
}

// WithEndpoint overrides the store endpoint; by default the credential Host
// is used.
func WithEndpoint(endpoint string) Option {
	return func(config *optionsConfig) { config.endpoint = endpoint }
}

// WithHTTPClient replaces the default HTTP client (5-minute request
// timeout), e.g. to tune timeouts or the transport.
func WithHTTPClient(client *http.Client) Option {
	return func(config *optionsConfig) { config.httpClient = client }
}

// defaultPeerRoutePrefix is DART's route; a legacy gateway can own any
// prefix.
const defaultPeerRoutePrefix = "/dart/"

// normalizePeerRoutePrefix coerces the prefix into "/<prefix>/".
func normalizePeerRoutePrefix(prefix string) string {
	if prefix == "" {
		return defaultPeerRoutePrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

// WithPeerGateway routes artifact downloads through the P2P peer gateway
// at base, as presign -> GET <base><routePrefix><presigned-url> (empty
// routePrefix = DART's "/dart/"). Metadata (index/manifest) stays direct:
// its 404 semantics must survive exactly. Empty base = direct mode.
func WithPeerGateway(base, routePrefix string) Option {
	return func(config *optionsConfig) {
		config.peerBase = base
		config.peerRoutePrefix = routePrefix
	}
}

// PullImage resolves the addressing chain for image and materializes the
// native artifact set in the local cache:
//
//  1. validate the image reference;
//  2. idempotency: a committed cache returns without any network request;
//  3. GET the image reference index (404 -> ErrImageNotReady);
//  4. GET the manifest it points at, verified against index.artifactDigest;
//  5. download each native artifact into the per-build digest namespace,
//     skipping files that already verify (resume) and re-pulling corrupt
//     ones;
//  6. write the local manifest as the commit point.
//
// The call is safe to race with a concurrent PullImage of the same image:
// every write lands in a temporary file and renames into place only after
// full verification, so no half-published state is ever observable.
//
// Idempotency semantics: a committed cache is returned without any network
// request and is never refreshed for the same image reference. A publisher
// rebuild of the same reference (last-writer-wins index) is therefore only
// picked up after clearing the cache directory or switching to a new tag —
// the intended trade-off for warm-image preheating.
func (c *Client) PullImage(ctx context.Context, stateRoot, image string) (resultErr error) {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("%w: image reference is required", runtimecontract.ErrInvalidConfig)
	}
	dir := imageDir(stateRoot, image)
	if complete, err := cacheComplete(dir); err != nil {
		return err
	} else if complete {
		return nil
	}
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return fmt.Errorf("prepare image cache: %w", err)
	}

	klog.InfoS("firecracker agent pull started", "image", image, "stateRoot", stateRoot)
	started := time.Now()
	completed := false
	defer func() {
		elapsed := time.Since(started)
		if completed {
			klog.InfoS("firecracker agent pull completed", "image", image, "elapsed", elapsed.String())
			return
		}
		klog.ErrorS(resultErr, "firecracker agent pull failed", "image", image, "elapsed", elapsed.String())
	}()

	index, err := fetchIndex(ctx, c.s3, image)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return fmt.Errorf("%w: %q", runtimecontract.ErrImageNotReady, image)
		}
		return err
	}
	// The index image field must match the requested reference byte for
	// byte: no normalization, no default tags.
	if index.Image != image {
		return fmt.Errorf("image index for %q belongs to %q: reference mismatch", image, index.Image)
	}

	manifestKey, err := c.s3.resolveRef(index.ManifestRef)
	if err != nil {
		return err
	}
	if err := c.materializeSet(ctx, dir, manifestKey, index.ArtifactDigest); err != nil {
		return err
	}
	completed = true
	return nil
}

// PullCheckpoint materializes a checkpoint artifact set addressed directly by
// its manifest reference and digest — the cross-host restore path of a paused
// Sandbox. Checkpoints carry no image index, so the caller supplies the
// immutable digest the publisher reported; the set is staged under the
// canonical checkpoint reference (artifacts.CheckpointReference) so both the
// producing and the resuming node share the cache layout with images.
func (c *Client) PullCheckpoint(ctx context.Context, stateRoot, reference, manifestRef, artifactDigest string) (resultErr error) {
	if strings.TrimSpace(reference) == "" {
		return fmt.Errorf("%w: checkpoint reference is required", runtimecontract.ErrInvalidConfig)
	}
	if strings.TrimSpace(manifestRef) == "" || strings.TrimSpace(artifactDigest) == "" {
		return fmt.Errorf("%w: checkpoint manifestRef and artifactDigest are required", runtimecontract.ErrInvalidConfig)
	}
	dir := imageDir(stateRoot, reference)
	if complete, err := cacheComplete(dir); err != nil {
		return err
	} else if complete {
		return nil
	}
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return fmt.Errorf("prepare checkpoint cache: %w", err)
	}

	klog.InfoS("firecracker agent checkpoint pull started", "reference", reference, "stateRoot", stateRoot)
	started := time.Now()
	completed := false
	defer func() {
		elapsed := time.Since(started)
		if completed {
			klog.InfoS("firecracker agent checkpoint pull completed", "reference", reference, "elapsed", elapsed.String())
			return
		}
		klog.ErrorS(resultErr, "firecracker agent checkpoint pull failed", "reference", reference, "elapsed", elapsed.String())
	}()

	manifestKey, err := c.s3.resolveRef(manifestRef)
	if err != nil {
		return err
	}
	if err := c.materializeSet(ctx, dir, manifestKey, artifactDigest); err != nil {
		return err
	}
	completed = true
	return nil
}

// materializeSet downloads and commits one artifact set into the local cache
// directory: the manifest (digest-verified against artifactDigest), every
// native artifact next to it in the same per-build namespace, and the local
// manifest as the commit point. It is shared by the index-addressed image
// pull and the ref-addressed checkpoint pull.
func (c *Client) materializeSet(ctx context.Context, dir, manifestKey, artifactDigest string) error {
	payload, err := c.fetchManifest(ctx, manifestKey, artifactDigest)
	if err != nil {
		return err
	}
	document, err := parseManifest(payload)
	if err != nil {
		return err
	}
	files, err := document.nativeFiles()
	if err != nil {
		return err
	}

	// Artifacts live in the same per-build digest namespace as the
	// manifest, so their store keys derive from the manifest reference.
	buildDir := path.Dir(manifestKey)
	for _, file := range files {
		if err := stageFile(ctx, c, dir, path.Join(buildDir, file.publish), file); err != nil {
			return err
		}
	}
	return commitManifest(dir, payload)
}

// getArtifact streams one native artifact object through the peer gateway
// when configured; a gateway transport failure or gateway error (502/400
// prefix drift) falls back to direct S3, so a broken gateway degrades to
// stage-1 behavior without failing the pull.
func (c *Client) getArtifact(ctx context.Context, storeKey string) (io.ReadCloser, error) {
	if c.peer == nil {
		return c.s3.get(ctx, storeKey)
	}
	body, err := c.getArtifactViaGateway(ctx, storeKey)
	if err == nil {
		return body, nil
	}
	if errors.Is(err, ErrObjectNotFound) {
		return nil, err
	}
	var status *httpError
	if errors.As(err, &status) && status.StatusCode >= 200 && status.StatusCode < 500 && status.StatusCode != http.StatusBadRequest {
		// A definitive client-class answer from the origin through the
		// gateway (excluding 400, which is a routing/prefix problem): do
		// not fall back, the origin would answer the same.
		return nil, err
	}
	// Transport error, gateway error (502/503) or prefix drift (400): the
	// gateway cannot serve this object; fall back to the direct
	// header-signed path (which carries its own retry loop). Never silent:
	// origin-bandwidth spikes are diagnosed from exactly this line.
	klog.ErrorS(err, "P2P gateway artifact fetch failed; falling back to direct S3", "storeKey", storeKey, "err", err)
	return c.s3.get(ctx, storeKey)
}

// getArtifactViaGateway performs one presigned GET through the gateway
// prefix route; a non-200 answer (502 origin error, 400 routing) becomes
// an httpError for the caller to classify.
func (c *Client) getArtifactViaGateway(ctx context.Context, storeKey string) (io.ReadCloser, error) {
	presigned, err := c.s3.presignGET(storeKey)
	if err != nil {
		return nil, err
	}
	target := c.peer.base + c.peer.routePrefix + presigned
	if _, err := url.Parse(target); err != nil {
		return nil, fmt.Errorf("build peer gateway request URL: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.peer.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, storeKey)
		}
		return nil, &httpError{StatusCode: response.StatusCode, Body: string(body)}
	}
	return response.Body, nil
}

// maxManifestBytes caps the downloaded manifest document. Manifests are
// small metadata (file list + compatibility tuple); a larger document is
// never legitimate, so it is rejected instead of being read unbounded.
const maxManifestBytes = 1 << 20

// fetchManifest downloads the manifest and verifies its content against the
// index artifactDigest, binding the whole artifact set to the index.
func (c *Client) fetchManifest(ctx context.Context, manifestKey, artifactDigest string) ([]byte, error) {
	body, err := c.s3.get(ctx, manifestKey)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return nil, fmt.Errorf("published manifest %s not found: %w (index points at an incomplete build)", manifestKey, err)
		}
		return nil, fmt.Errorf("fetch published manifest: %w", err)
	}
	defer body.Close()
	payload, err := io.ReadAll(io.LimitReader(body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read published manifest: %w", err)
	}
	if len(payload) > maxManifestBytes {
		return nil, fmt.Errorf("published manifest exceeds the %d-byte limit", maxManifestBytes)
	}
	if sum := sha256Hex(payload); sum != artifactDigest {
		return nil, fmt.Errorf("published manifest digest mismatch: got %s, want %s", sum, artifactDigest)
	}
	return payload, nil
}
