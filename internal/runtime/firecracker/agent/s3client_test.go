package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/registryconfig"
)

func TestParseStoreRoot(t *testing.T) {
	bucket, prefix, err := parseStoreRoot("s3://sandbox-images")
	require.NoError(t, err)
	require.Equal(t, "sandbox-images", bucket)
	require.Empty(t, prefix)

	bucket, prefix, err = parseStoreRoot("s3://sandbox-images/publish/v1")
	require.NoError(t, err)
	require.Equal(t, "sandbox-images", bucket)
	require.Equal(t, "publish/v1", prefix)

	_, _, err = parseStoreRoot("https://sandbox-images.s3.amazonaws.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "scheme must be s3://")

	_, _, err = parseStoreRoot("s3:///no-bucket")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket is required")
}

func TestArtifactBodyOutlivesMetadataTimeout(t *testing.T) {
	for _, route := range []string{"S3", "DART"} {
		t.Run(route, func(t *testing.T) {
			finishArtifact := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if strings.HasSuffix(r.URL.Path, "/metadata.json") {
					<-r.Context().Done()
					return
				}
				select {
				case <-finishArtifact:
					if strings.HasPrefix(r.URL.Path, "/dart/") {
						_, _ = io.WriteString(w, "DART")
						return
					}
					_, _ = io.WriteString(w, "S3")
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			options := []Option{WithEndpoint(server.URL)}
			if route == "DART" {
				options = append(options, WithPeerGateway(server.URL, ""))
			}
			client, err := NewClient("s3://artifacts", registryconfig.Credential{Username: "key", Password: "secret"}, options...)
			require.NoError(t, err)
			require.Equal(t, 5*time.Minute, client.s3.http.Timeout)
			require.Equal(t, 15*time.Minute, client.artifactHTTP.Timeout)
			client.s3.http.Timeout = 250 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			artifact, err := client.getArtifact(ctx, "rootfs.img")
			require.NoError(t, err)
			defer artifact.Close()
			metadata, err := client.s3.get(ctx, "metadata.json")
			require.NoError(t, err)
			defer metadata.Close()
			_, err = io.ReadAll(metadata)
			require.ErrorIs(t, err, context.DeadlineExceeded)

			close(finishArtifact)
			contents, err := io.ReadAll(artifact)
			require.NoError(t, err)
			require.Equal(t, route, string(contents))
		})
	}
}

func TestS3GetPathAndSigningHeaders(t *testing.T) {
	var received *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(server.Close)
	client := clientForEndpoint(t, server.URL, testPrefix)

	body, err := client.get(context.Background(), "index/abc.json")
	require.NoError(t, err)
	content, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "payload", string(content))
	require.NoError(t, body.Close())

	// Path-style request under the store prefix.
	require.Equal(t, "/"+testBucket+"/"+testPrefix+"/index/abc.json", received.URL.Path)
	// SigV4 headers: date, empty payload hash, and the authorization header.
	require.NotEmpty(t, received.Header.Get("X-Amz-Date"))
	require.Equal(t, emptyPayloadHash, received.Header.Get("X-Amz-Content-Sha256"))
	authorization := received.Header.Get("Authorization")
	require.True(t, strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=test-access-key/"), authorization)
	require.Contains(t, authorization, "/s3/aws4_request")
	require.Contains(t, authorization, "SignedHeaders=host;x-amz-content-sha256;x-amz-date")
	require.Contains(t, authorization, "Signature=")
}

func TestS3GetNotFound(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	client := testS3Client(t, store)

	_, err := client.get(context.Background(), "index/missing.json")
	require.ErrorIs(t, err, ErrObjectNotFound)
	require.Equal(t, 1, store.countRequested("index/missing.json"), "404 must not be retried")
}

func TestS3GetRetriesOn5xx(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{
		"manifest.json": []byte("{}"),
	}, flaky: map[string]int{"manifest.json": 2}}
	client := testS3Client(t, store)

	body, err := client.get(context.Background(), "manifest.json")
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Equal(t, 3, store.countRequested("manifest.json"), "two 5xx then success")
}

func TestS3GetExhaustsRetries(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{
		"manifest.json": []byte("{}"),
	}, flaky: map[string]int{"manifest.json": 100}}
	client := testS3Client(t, store)

	_, err := client.get(context.Background(), "manifest.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "GET "+testPrefix+"/manifest.json")
	require.Equal(t, s3RetryAttempts+1, store.countRequested("manifest.json"))
}

func TestS3GetClientErrorNotRetried(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	client := testS3Client(t, store)

	// A forbidden object is a 403: it must fail fast, not retry.
	client.http.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("AccessDenied"))}, nil
	})
	_, err := client.get(context.Background(), "manifest.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "403")
	require.Contains(t, err.Error(), "AccessDenied")
}

func TestS3GetContextCancellation(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}, flaky: map[string]int{"x": 100}}
	client := testS3Client(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := client.get(ctx, "x")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestResolveRef(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	client := testS3Client(t, store)

	key, err := client.resolveRef("s3://" + testBucket + "/" + testPrefix + "/abc123/manifest.json")
	require.NoError(t, err)
	require.Equal(t, "abc123/manifest.json", key)

	// Prefix-less stores resolve against the bucket directly.
	client.prefix = ""
	key, err = client.resolveRef("s3://" + testBucket + "/abc123/manifest.json")
	require.NoError(t, err)
	require.Equal(t, "abc123/manifest.json", key)

	// References outside the configured bucket or prefix are rejected.
	_, err = client.resolveRef("s3://other-bucket/abc123/manifest.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "points at bucket")
	client.prefix = testPrefix
	_, err = client.resolveRef("s3://" + testBucket + "/elsewhere/abc.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside store prefix")
	_, err = client.resolveRef("http://not-s3/abc.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid object reference")
}

func TestNewClient(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)

	credential := registryconfig.Credential{
		Host: server.URL, Username: "ak", Password: "sk",
	}
	client, err := NewClient("s3://"+testBucket+"/"+testPrefix, credential)
	require.NoError(t, err)
	require.NotNil(t, client)
	require.Equal(t, server.URL, client.s3.endpoint)
	require.Equal(t, "us-east-1", client.s3.region)

	// The credential Host supplies the endpoint when no override is given.
	client, err = NewClient("s3://"+testBucket+"/"+testPrefix, credential, WithRegion("oss-cn-hangzhou"))
	require.NoError(t, err)
	require.Equal(t, "oss-cn-hangzhou", client.s3.region)

	// WithEndpoint overrides the credential host.
	client, err = NewClient("s3://"+testBucket+"/"+testPrefix, registryconfig.Credential{},
		WithEndpoint(server.URL))
	require.NoError(t, err)
	require.Equal(t, server.URL, client.s3.endpoint)

	// A bare host gets the https scheme by default.
	client, err = NewClient("s3://"+testBucket, registryconfig.Credential{Host: "minio.example.com:9000"})
	require.NoError(t, err)
	require.Equal(t, "https://minio.example.com:9000", client.s3.endpoint)

	_, err = NewClient("nonsense", credential)
	require.Error(t, err)
	_, err = NewClient("s3://"+testBucket, registryconfig.Credential{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint is required")
}

// roundTripFunc adapts a function to http.RoundTripper for one-off clients.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
