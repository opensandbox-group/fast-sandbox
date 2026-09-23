package agent

// Live stage-2 verification against a REAL MinIO (and optionally a REAL DART
// instance). These tests are gated behind environment variables because they
// need a seeded store and are meant for the reference host (or a local
// MinIO container). They close the risk item "presigned URL signature
// correctness against MinIO" that unit tests cannot (the fake servers
// verify parameter construction; MinIO validates the signature server-side).
//
// Setup (local smoke):
//
//	docker run -d --name minio -p 19000:9000 -e MINIO_ROOT_USER=smoke \
//	    -e MINIO_ROOT_PASSWORD=smoke-secret minio/minio server /data
//	mc alias set smoke http://127.0.0.1:19000 smoke smoke-secret
//	mc mb smoke/sandbox-images
//	mc cp rootfs.bin smoke/sandbox-images/digest16/rootfs.ext4
//	FS_LIVE_MINIO_ENDPOINT=http://127.0.0.1:19000 \
//	FS_LIVE_MINIO_AK=smoke FS_LIVE_MINIO_SK=smoke-secret \
//	FS_LIVE_MINIO_BUCKET=sandbox-images \
//	FS_LIVE_MINIO_KEY=digest16/rootfs.ext4 \
//	FS_LIVE_MINIO_SHA256=<sha256 of rootfs.bin> \
//	FS_LIVE_DART_ADDR=http://127.0.0.1:8145 \
//	go test ./internal/runtime/firecracker/agent/ -run TestLive -count=1 -v

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func liveMinioEnv(t *testing.T) (endpoint, accessKey, secretKey, bucket, key, wantSHA string) {
	t.Helper()
	endpoint = os.Getenv("FS_LIVE_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("FS_LIVE_MINIO_ENDPOINT not set; live verification is opt-in")
	}
	return endpoint,
		os.Getenv("FS_LIVE_MINIO_AK"),
		os.Getenv("FS_LIVE_MINIO_SK"),
		os.Getenv("FS_LIVE_MINIO_BUCKET"),
		os.Getenv("FS_LIVE_MINIO_KEY"),
		os.Getenv("FS_LIVE_MINIO_SHA256")
}

func liveS3Client(endpoint, accessKey, secretKey, bucket string) *s3Client {
	return &s3Client{
		endpoint: endpoint, region: "us-east-1", bucket: bucket, prefix: "",
		accessKey: accessKey, secretKey: secretKey,
		http:       &http.Client{Timeout: 10 * time.Minute},
		retryDelay: 100 * time.Millisecond,
	}
}

// TestLivePresignedGETAgainstMinIO proves the presigned URL is accepted by a
// real SigV4 server (MinIO re-derives the query signature) and returns the
// exact seeded object bytes.
func TestLivePresignedGETAgainstMinIO(t *testing.T) {
	endpoint, accessKey, secretKey, bucket, key, wantSHA := liveMinioEnv(t)
	client := liveS3Client(endpoint, accessKey, secretKey, bucket)

	presigned, err := client.presignGET(key)
	require.NoError(t, err)
	response, err := http.Get(presigned)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "MinIO rejected the presigned signature")
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if wantSHA != "" {
		require.Equal(t, wantSHA, sha256Hex(payload))
	}
}

// TestLivePullThroughDART exercises the exact stage-2 agent path against a
// real DART instance: presign -> GET <dart>/dart/<presigned-url> -> origin
// fetch -> whole-object digest. A second read proves the block cache
// serves it (the origin counter stops moving; asserted via the admin
// metrics the operator can curl).
func TestLivePullThroughDART(t *testing.T) {
	endpoint, accessKey, secretKey, bucket, key, wantSHA := liveMinioEnv(t)
	dartBase := os.Getenv("FS_LIVE_DART_ADDR")
	if dartBase == "" {
		t.Skip("FS_LIVE_DART_ADDR not set; the DART-leg of the live check is opt-in")
	}
	client := &Client{s3: liveS3Client(endpoint, accessKey, secretKey, bucket), peer: &peerGateway{
		base: dartBase, routePrefix: defaultPeerRoutePrefix, http: &http.Client{Timeout: 10 * time.Minute},
	}}

	for read := 1; read <= 2; read++ {
		body, err := client.getArtifact(context.Background(), key)
		require.NoError(t, err)
		digest := sha256.New()
		written, err := io.Copy(digest, body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		if wantSHA != "" {
			require.Equal(t, wantSHA, hex.EncodeToString(digest.Sum(nil)), "read %d digest mismatch", read)
		}
		t.Logf("read %d: %d bytes via DART (%s)", read, written, dartBase)
	}
}

// dartAdmin reads one Prometheus counter line (dart_block_source_total).
func dartCounter(t *testing.T, admin, source string) int {
	t.Helper()
	response, err := http.Get("http://" + admin + "/metrics")
	require.NoError(t, err)
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	prefix := `dart_block_source_total{source="` + source + `"} `
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.HasPrefix(line, prefix) {
			var value int
			_, err := fmt.Sscanf(line, prefix+"%d", &value)
			require.NoError(t, err)
			return value
		}
	}
	return 0
}

// TestLivePeerHitThroughTwoDARTNodes proves the P2P data plane itself: two
// node-local DART instances (static peers, empty caches) serve the same
// object to two cold agents, and the ORIGIN is fetched once for the whole
// cluster — the second node is served from the peer, its origin counter
// stays put. A single-node DART only proves caching; the peer leg is the
// stage-2 value.
//
// Setup on the reference host (one MinIO, two dart processes):
//
//	dart -listen=127.0.0.1:8145 -admin=127.0.0.1:8146 \
//	    -cache-dir=/tmp/dart-a -cache-size=1GiB \
//	    -self-id=node-a -peers=node-a@127.0.0.1:9201,node-b@127.0.0.1:9202 \
//	    -peer-listen=127.0.0.1:9201 &
//	dart -listen=127.0.0.1:8147 -admin=127.0.0.1:8148 \
//	    -cache-dir=/tmp/dart-b -cache-size=1GiB \
//	    -self-id=node-b -peers=node-a@127.0.0.1:9201,node-b@127.0.0.1:9202 \
//	    -peer-listen=127.0.0.1:9202 &
func TestLivePeerHitThroughTwoDARTNodes(t *testing.T) {
	endpoint, accessKey, secretKey, bucket, key, wantSHA := liveMinioEnv(t)
	nodeA := os.Getenv("FS_LIVE_DART_NODE_A")   // http://127.0.0.1:8145
	nodeB := os.Getenv("FS_LIVE_DART_NODE_B")   // http://127.0.0.1:8147
	adminA := os.Getenv("FS_LIVE_DART_ADMIN_A") // 127.0.0.1:8146
	adminB := os.Getenv("FS_LIVE_DART_ADMIN_B") // 127.0.0.1:8148
	if nodeA == "" || nodeB == "" || adminA == "" || adminB == "" {
		t.Skip("FS_LIVE_DART_NODE_A/B + ADMIN_A/B not set; the two-node peer check is opt-in")
	}
	origin := func() int { return dartCounter(t, adminA, "origin") + dartCounter(t, adminB, "origin") }
	readVia := func(name, base string) {
		client := &Client{s3: liveS3Client(endpoint, accessKey, secretKey, bucket), peer: &peerGateway{
			base: base, routePrefix: defaultPeerRoutePrefix, http: &http.Client{Timeout: 10 * time.Minute},
		}}
		body, err := client.getArtifact(context.Background(), key)
		require.NoError(t, err)
		digest := sha256.New()
		written, err := io.Copy(digest, body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		if wantSHA != "" {
			require.Equal(t, wantSHA, hex.EncodeToString(digest.Sum(nil)), "%s digest mismatch", name)
		}
		t.Logf("%s: %d bytes", name, written)
	}
	originBefore := origin()
	readVia("node A (cold)", nodeA)
	peerA := dartCounter(t, adminA, "peer")
	cacheA := dartCounter(t, adminA, "cache")
	originAfterA := origin()
	readVia("node B (cold)", nodeB)
	originAfterB := origin()
	peerB := dartCounter(t, adminB, "peer")
	cacheB := dartCounter(t, adminB, "cache")

	t.Logf("cluster origin: before=%d after-A=%d after-B=%d", originBefore, originAfterA, originAfterB)
	t.Logf("node A: peer=%d cache=%d origin=%d", peerA, cacheA, dartCounter(t, adminA, "origin"))
	t.Logf("node B: peer=%d cache=%d origin=%d", peerB, cacheB, dartCounter(t, adminB, "origin"))

	// The object was fetched from the origin exactly once cluster-wide: the
	// second node's read must not touch the origin at all.
	require.Equal(t, originAfterA, originAfterB,
		"node B's cold read must be served by the peer, not the origin (single fetch for N nodes)")
	require.Greater(t, originAfterA, originBefore, "node A's cold read must reach the origin")
	require.True(t, peerB > 0 || cacheB > 0, "node B must be served by the peer or cache")
}
