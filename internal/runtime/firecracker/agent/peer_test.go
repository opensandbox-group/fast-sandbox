package agent

// Stage-2 P2P tests: SigV4 query presigning (B) and peer-gateway routing
// with direct-S3 fallback (C). Metadata stays direct, so its 404 semantics
// survive a gateway's origin-error collapsing.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// fakeGateway stands in for a P2P peer gateway: prefix-mode requests
// (/<prefix>/<upstream URL>) proxied to the origin, with an optional 502
// "origin error" mode.
type fakeGateway struct {
	mu     sync.Mutex
	server *httptest.Server
	prefix string
	hits   []string
	fail   bool
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	return newFakeGatewayWithPrefix(t, "/dart/")
}

func newFakeGatewayWithPrefix(t *testing.T, prefix string) *fakeGateway {
	t.Helper()
	gateway := &fakeGateway{prefix: prefix}
	gateway.server = httptest.NewServer(http.HandlerFunc(gateway.ServeHTTP))
	t.Cleanup(gateway.server.Close)
	return gateway
}

func (d *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.RequestURI
	d.mu.Lock()
	fail := d.fail
	d.mu.Unlock()
	if fail {
		http.Error(w, "origin error: upstream fetch failed", http.StatusBadGateway)
		return
	}
	if !strings.HasPrefix(uri, d.prefix) {
		http.Error(w, "cannot resolve origin", http.StatusBadRequest)
		return
	}
	upstream := uri[len(d.prefix):]
	if !strings.HasPrefix(upstream, "http://") && !strings.HasPrefix(upstream, "https://") {
		http.Error(w, "upstream URL must include a scheme", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.hits = append(d.hits, upstream)
	d.mu.Unlock()
	response, err := http.Get(upstream)
	if err != nil {
		http.Error(w, "origin error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (d *fakeGateway) url() string { return d.server.URL }

// hitCount returns how many prefix-mode requests reached the gateway.
func (d *fakeGateway) hitCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.hits)
}

// --- B: presigned URL construction and SigV4 query signature ---------------

func TestPresignGETParametersAndSignature(t *testing.T) {
	var received *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	client := &s3Client{
		endpoint: server.URL, region: "us-east-1", bucket: testBucket, prefix: testPrefix,
		accessKey: "test-access-key", secretKey: "test-secret-key",
		http: &http.Client{Timeout: time.Second},
	}
	presigned, err := client.presignGET("index/abc.json")
	require.NoError(t, err)

	parsed, err := url.Parse(presigned)
	require.NoError(t, err)
	require.Equal(t, "/"+testBucket+"/"+testPrefix+"/index/abc.json", parsed.Path)
	require.NotEmpty(t, parsed.Query().Get("X-Amz-Signature"))

	// The presigned URL works with a plain (unauthenticated) client and
	// carries no Authorization header — the signature is entirely in the
	// query string.
	response, err := http.Get(presigned)
	require.NoError(t, err)
	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "ok", string(content))
	require.Empty(t, received.Header.Get("Authorization"), "presigned GET must not carry header signing")

	query := parsed.Query()
	require.Equal(t, "AWS4-HMAC-SHA256", query.Get("X-Amz-Algorithm"))
	require.True(t, strings.HasPrefix(query.Get("X-Amz-Credential"), "test-access-key/"))
	require.Contains(t, query.Get("X-Amz-Credential"), "/us-east-1/s3/aws4_request")
	require.Equal(t, "host", query.Get("X-Amz-SignedHeaders"))
	require.Equal(t, "3600", query.Get("X-Amz-Expires"))
	require.NotEmpty(t, query.Get("X-Amz-Date"))
	require.Len(t, query.Get("X-Amz-Signature"), 64)
}

func TestPresignGETSignatureValidatesServerSide(t *testing.T) {
	// The server re-derives the SigV4 query signature from the received
	// request (the same way MinIO/OSS do) and rejects a mismatch, proving
	// the canonical request is correct, not just well-formed.
	secret := "test-secret-key"
	accessKey := "test-access-key"
	verified := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, verifyPresignedSignature(t, accessKey, secret, "us-east-1", r))
		verified <- true
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	client := &s3Client{
		endpoint: server.URL, region: "us-east-1", bucket: testBucket, prefix: "",
		accessKey: accessKey, secretKey: secret,
		http: &http.Client{Timeout: time.Second},
	}
	presigned, err := client.presignGET("digest16/rootfs.ext4")
	require.NoError(t, err)
	response, err := http.Get(presigned)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.True(t, <-verified)
}

// verifyPresignedSignature recomputes the presigned GET signature from the
// received request and compares it with the X-Amz-Signature parameter.
func verifyPresignedSignature(t *testing.T, accessKey, secretKey, region string, r *http.Request) error {
	t.Helper()
	query := r.URL.Query()
	algorithm := query.Get("X-Amz-Algorithm")
	if algorithm != "AWS4-HMAC-SHA256" {
		t.Fatalf("unexpected algorithm %q", algorithm)
	}
	scope := query.Get("X-Amz-Credential")
	parts := strings.Split(scope, "/")
	if len(parts) != 5 || parts[0] != accessKey || parts[4] != "aws4_request" {
		t.Fatalf("unexpected credential scope %q", scope)
	}
	amzDate := query.Get("X-Amz-Date")
	date := parts[1]
	signedHeaders := query.Get("X-Amz-SignedHeaders")
	if signedHeaders != "host" {
		t.Fatalf("signed headers %q, want host", signedHeaders)
	}
	parameters := [][2]string{
		{"X-Amz-Algorithm", algorithm},
		{"X-Amz-Credential", scope},
		{"X-Amz-Date", amzDate},
		{"X-Amz-Expires", query.Get("X-Amz-Expires")},
		{"X-Amz-SignedHeaders", signedHeaders},
	}
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		r.URL.EscapedPath(),
		canonicalQueryString(parameters),
		"host:" + r.Host + "\n",
		"host",
		unsignedPayload,
	}, "\n")
	scopeValue := date + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scopeValue + "\n" + sha256Hex([]byte(canonicalRequest))
	expected := hmacHex(deriveSigningKey(secretKey, date, region), stringToSign)
	if expected != query.Get("X-Amz-Signature") {
		return &httpError{StatusCode: http.StatusForbidden, Body: "signature mismatch"}
	}
	return nil
}

// --- C: artifact routing through a peer gateway with direct-S3 fallback ----

func TestPullImageArtifactsViaPeerGateway(t *testing.T) {
	store, client, _, artifacts := publishFixture(t)
	gateway := newFakeGateway(t)
	peerClient := &Client{s3: client, peer: &peerGateway{
		base: gateway.url(), routePrefix: "/dart/", http: &http.Client{Timeout: time.Minute},
	}}
	root := t.TempDir()

	require.NoError(t, peerClient.PullImage(context.Background(), root, testImage))

	dir := imageDir(root, testImage)
	for cacheName, content := range map[string][]byte{
		"rootfs.img":   artifacts["rootfs.ext4"],
		"vmstate.snap": artifacts["vmstate.snap"],
		"memory.snap":  artifacts["memory.snap"],
	} {
		got, err := os.ReadFile(filepath.Join(dir, cacheName))
		require.NoError(t, err)
		require.Equal(t, content, got, cacheName)
	}

	// Every object reached the store exactly once: the index and manifest
	// DIRECT (metadata keeps exact 404 semantics), the three artifacts via
	// the gateway forwarding the presigned upstream (a direct fetch would
	// double the artifact keys).
	buildDir := sha256Hex(testManifest(artifacts))[:16]
	require.Equal(t, 1, store.countRequested("index/"+imageKey(testImage)+".json"))
	require.Equal(t, 1, store.countRequested(buildDir+"/manifest.json"))
	require.Equal(t, 1, store.countRequested(buildDir+"/rootfs.ext4"))
	require.Equal(t, 1, store.countRequested(buildDir+"/vmstate.snap"))
	require.Equal(t, 1, store.countRequested(buildDir+"/memory.snap"))

	require.Equal(t, 3, gateway.hitCount(), "three artifacts must go through the peer gateway")
	for _, upstream := range gateway.hits {
		parsed, err := url.Parse(upstream)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(upstream, "http://"), upstream)
		query := parsed.Query()
		require.NotEmpty(t, query.Get("X-Amz-Signature"), "gateway upstream must be presigned")
		require.Equal(t, "host", query.Get("X-Amz-SignedHeaders"))
		require.NotContains(t, upstream, "index/", "metadata must not go through the peer gateway")
		require.NotContains(t, upstream, "manifest.json", "metadata must not go through the peer gateway")
	}
}

func TestPeerGatewayCustomRoutePrefix(t *testing.T) {
	// A legacy gateway may own any prefix; the pull client must honor it.
	store, client, _, artifacts := publishFixture(t)
	gateway := newFakeGatewayWithPrefix(t, "/peer-blocks/")
	peerClient := &Client{s3: client, peer: &peerGateway{
		base: gateway.url(), routePrefix: "/peer-blocks/", http: &http.Client{Timeout: time.Minute},
	}}
	require.NoError(t, peerClient.PullImage(context.Background(), t.TempDir(), testImage))
	// The fake only counts requests under "/peer-blocks/", so the hit count
	// proves the artifacts routed through the configured prefix.
	require.Equal(t, 3, gateway.hitCount(), "artifacts must route through the configured prefix")
	buildDir := sha256Hex(testManifest(artifacts))[:16]
	require.Equal(t, 1, store.countRequested(buildDir+"/rootfs.ext4"), "origin fetch stays deduplicated through the custom-prefix gateway")
}

func TestPullImagePeerGatewayUnreachableFallsBackToDirect(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	gateway := newFakeGateway(t)
	gatewayBase := gateway.url()
	gateway.server.Close() // simulate a dead gateway process

	peerClient := &Client{s3: client, peer: &peerGateway{
		base: gatewayBase, routePrefix: "/dart/", http: &http.Client{Timeout: time.Second},
	}}
	require.NoError(t, peerClient.PullImage(context.Background(), t.TempDir(), testImage))

	// Transport failure to the gateway falls back to the direct path: every
	// object (index + manifest + 3 artifacts) was fetched from the store
	// directly.
	require.Equal(t, 5, len(store.requested()))
	require.Zero(t, gateway.hitCount())
}

func TestPullImagePeerGatewayErrorFallsBackToDirect(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	gateway := newFakeGateway(t)
	gateway.mu.Lock()
	gateway.fail = true // the gateway answers 502 "origin error"
	gateway.mu.Unlock()

	peerClient := &Client{s3: client, peer: &peerGateway{
		base: gateway.url(), routePrefix: "/dart/", http: &http.Client{Timeout: time.Second},
	}}
	require.NoError(t, peerClient.PullImage(context.Background(), t.TempDir(), testImage))

	require.Equal(t, 5, len(store.requested()), "gateway errors must fall back to direct S3")
}

func TestPullImagePeerGatewayKeepsImageNotReadySemantics(t *testing.T) {
	// The index lives only in the store, and it is fetched DIRECT even in
	// gateway mode: a missing build must keep surfacing ErrImageNotReady
	// (the gateway would collapse the origin 404 into a 502).
	store, client, _, _ := publishFixture(t)
	store.mu.Lock()
	delete(store.objects, "index/"+imageKey(testImage)+".json")
	store.mu.Unlock()
	gateway := newFakeGateway(t)

	peerClient := &Client{s3: client, peer: &peerGateway{
		base: gateway.url(), routePrefix: "/dart/", http: &http.Client{Timeout: time.Second},
	}}
	err := peerClient.PullImage(context.Background(), t.TempDir(), testImage)
	require.ErrorIs(t, err, runtimecontract.ErrImageNotReady)
	require.Zero(t, gateway.hitCount(), "no artifact request may reach the gateway without an index")
}

func TestNormalizePeerRoutePrefix(t *testing.T) {
	require.Equal(t, "/dart/", normalizePeerRoutePrefix(""))
	require.Equal(t, "/dart/", normalizePeerRoutePrefix(defaultPeerRoutePrefix))
	require.Equal(t, "/p2p/", normalizePeerRoutePrefix("/p2p/"))
	require.Equal(t, "/p2p/", normalizePeerRoutePrefix("/p2p"))
	require.Equal(t, "/p2p/", normalizePeerRoutePrefix("p2p/"))
	require.Equal(t, "/p2p/", normalizePeerRoutePrefix("p2p"))
}
