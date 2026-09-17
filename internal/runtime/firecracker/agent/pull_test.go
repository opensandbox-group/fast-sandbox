package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fast-sandbox/internal/artifacts"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	"github.com/stretchr/testify/require"
)

const (
	testBucket = "sandbox-images"
	testPrefix = "publish"
	testImage  = "registry.example.com/sandbox:v1.0.21"
)

// fakeStore is an in-memory S3-compatible store serving path-style GETs at
// /<bucket>/<prefix>/<key>. It records requests and injects transient 500s
// by store-relative key, matching the s3Client's view of the store.
type fakeStore struct {
	mu       sync.Mutex
	bucket   string
	prefix   string
	objects  map[string][]byte
	requests []string
	flaky    map[string]int // remaining 500 responses per key
}

func (s *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 || parts[0] != s.bucket {
		http.NotFound(w, r)
		return
	}
	key := parts[1]
	if s.prefix != "" {
		if !strings.HasPrefix(key, s.prefix+"/") {
			http.NotFound(w, r)
			return
		}
		key = strings.TrimPrefix(key, s.prefix+"/")
	}
	s.mu.Lock()
	s.requests = append(s.requests, key)
	if s.flaky[key] > 0 {
		s.flaky[key]--
		s.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	content, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(content)
}

// requested returns a snapshot of the served keys.
func (s *fakeStore) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// countRequested returns how many requests matched key.
func (s *fakeStore) countRequested(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, request := range s.requests {
		if request == key {
			count++
		}
	}
	return count
}

// testS3Client builds a client wired to the fake store with a short retry
// backoff and request timeout.
func testS3Client(t *testing.T, store *fakeStore) *s3Client {
	t.Helper()
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	return &s3Client{
		endpoint: server.URL, region: "us-east-1", bucket: store.bucket, prefix: testPrefix,
		accessKey: "test-access-key", secretKey: "test-secret-key",
		http:       &http.Client{Timeout: time.Second},
		retryDelay: time.Millisecond,
	}
}

// testManifest assembles a manifest document in the publisher's shape for a
// set of artifacts, returning the exact bytes a download would carry.
func testManifest(artifacts map[string][]byte) []byte {
	files := map[string]any{}
	for name, content := range artifacts {
		files[name] = map[string]any{
			"sha256":    sha256Hex(content),
			"sizeBytes": len(content),
		}
	}
	document := map[string]any{
		"schemaVersion": 1,
		"runtime":       "firecracker",
		"lineage":       map[string]any{"image": testImage},
		"files":         files,
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		panic(err)
	}
	return payload
}

// publishFixture publishes a complete artifact set for testImage into a fake
// store exactly like the builder does: artifacts first,
// then the manifest, then the index. It returns the store, the s3 client,
// the manifest bytes, and the artifact contents.
func publishFixture(t *testing.T) (*fakeStore, *s3Client, []byte, map[string][]byte) {
	t.Helper()
	artifacts := map[string][]byte{
		"rootfs.ext4":  bytes.Repeat([]byte{0xab}, 1<<16),
		"vmstate.snap": []byte("vmstate-bytes"),
		"memory.snap":  []byte("memory-bytes"),
	}
	manifestPayload := testManifest(artifacts)
	buildDir := sha256Hex(manifestPayload)[:16]
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}, flaky: map[string]int{}}
	for name, content := range artifacts {
		store.objects[buildDir+"/"+name] = content
	}
	store.objects[buildDir+"/manifest.json"] = manifestPayload
	index := imageIndex{
		Image:          testImage,
		ManifestRef:    "s3://" + testBucket + "/" + testPrefix + "/" + buildDir + "/manifest.json",
		ArtifactDigest: sha256Hex(manifestPayload),
		UpdatedAt:      "2026-08-27T00:00:00Z",
	}
	indexPayload, err := json.Marshal(index)
	require.NoError(t, err)
	store.objects["index/"+imageKey(testImage)+".json"] = indexPayload
	return store, testS3Client(t, store), manifestPayload, artifacts
}

func TestPullImageFullFlow(t *testing.T) {
	store, client, manifestPayload, artifacts := publishFixture(t)
	root := t.TempDir()

	require.NoError(t, (&Client{s3: client}).PullImage(context.Background(), root, testImage))

	dir := imageDir(root, testImage)
	names := map[string]string{"rootfs.img": "rootfs.ext4", "vmstate.snap": "vmstate.snap", "memory.snap": "memory.snap"}
	for cacheName, publishName := range names {
		content, err := os.ReadFile(filepath.Join(dir, cacheName))
		require.NoError(t, err, cacheName)
		require.Equal(t, artifacts[publishName], content, cacheName)
	}
	committed, err := os.ReadFile(filepath.Join(dir, manifestName))
	require.NoError(t, err)
	require.Equal(t, manifestPayload, committed)

	// The addressing chain hit index, manifest, and every artifact exactly
	// once, all under the per-build digest namespace.
	expected := []string{
		"index/" + imageKey(testImage) + ".json",
		sha256Hex(manifestPayload)[:16] + "/manifest.json",
		sha256Hex(manifestPayload)[:16] + "/rootfs.ext4",
		sha256Hex(manifestPayload)[:16] + "/vmstate.snap",
		sha256Hex(manifestPayload)[:16] + "/memory.snap",
	}
	require.ElementsMatch(t, expected, store.requested())
}

func TestPullImageIdempotent(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	root := t.TempDir()
	pull := &Client{s3: client}

	require.NoError(t, pull.PullImage(context.Background(), root, testImage))
	require.NoError(t, pull.PullImage(context.Background(), root, testImage))

	// A committed cache returns without a single request.
	require.Len(t, store.requested(), 5, "second pull must not touch the store")
}

func TestPullImageResumeSkipsMatchingFiles(t *testing.T) {
	store, client, manifestPayload, artifacts := publishFixture(t)
	root := t.TempDir()

	// A previous attempt already committed rootfs.img (content matches the
	// published manifest) but crashed before the rest.
	dir := imageDir(root, testImage)
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rootfs.img"), artifacts["rootfs.ext4"], cacheFileMode))

	require.NoError(t, (&Client{s3: client}).PullImage(context.Background(), root, testImage))

	requested := store.requested()
	require.NotContains(t, requested, sha256Hex(manifestPayload)[:16]+"/rootfs.ext4", "matching file must be skipped")
	require.Contains(t, requested, sha256Hex(manifestPayload)[:16]+"/vmstate.snap")
	require.Contains(t, requested, sha256Hex(manifestPayload)[:16]+"/memory.snap")
	for _, name := range []string{"rootfs.img", "vmstate.snap", "memory.snap"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.NotEmpty(t, content, name)
	}
}

func TestPullImageRepairsCorruptCache(t *testing.T) {
	store, client, manifestPayload, artifacts := publishFixture(t)
	root := t.TempDir()

	// rootfs.img exists with the same size but different content (silent
	// corruption passes the stat fast path and must be caught by the
	// digest); the manifest was committed.
	dir := imageDir(root, testImage)
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))
	corrupt := bytes.Repeat([]byte{0x00}, len(artifacts["rootfs.ext4"]))
	require.NotEqual(t, artifacts["rootfs.ext4"], corrupt)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rootfs.img"), corrupt, cacheFileMode))
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), manifestPayload, cacheFileMode))

	require.NoError(t, (&Client{s3: client}).PullImage(context.Background(), root, testImage))

	content, err := os.ReadFile(filepath.Join(dir, "rootfs.img"))
	require.NoError(t, err)
	require.Equal(t, artifacts["rootfs.ext4"], content, "corrupt file must be re-pulled")
	require.Equal(t, 1, store.countRequested(sha256Hex(manifestPayload)[:16]+"/rootfs.ext4"))
}

func TestPullImageEmptyImageRejected(t *testing.T) {
	_, client, _, _ := publishFixture(t)
	root := t.TempDir()
	err := (&Client{s3: client}).PullImage(context.Background(), root, "  ")
	require.ErrorIs(t, err, runtimecontract.ErrInvalidConfig)
}

func TestPullImageIndexNotFound(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	store.mu.Lock()
	store.objects = map[string][]byte{}
	store.mu.Unlock()
	root := t.TempDir()

	err := (&Client{s3: client}).PullImage(context.Background(), root, testImage)
	require.ErrorIs(t, err, runtimecontract.ErrImageNotReady)
	require.Equal(t, 1, store.countRequested("index/"+imageKey(testImage)+".json"), "404 must not be retried")
}

func TestPullImageIndexImageMismatch(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	store.mu.Lock()
	var index imageIndex
	require.NoError(t, json.Unmarshal(store.objects["index/"+imageKey(testImage)+".json"], &index))
	index.Image = "registry.example.com/other:v2"
	payload, err := json.Marshal(index)
	require.NoError(t, err)
	store.objects["index/"+imageKey(testImage)+".json"] = payload
	store.mu.Unlock()

	err = (&Client{s3: client}).PullImage(context.Background(), t.TempDir(), testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reference mismatch")
}

func TestPullImageManifestNotFound(t *testing.T) {
	store, client, manifestPayload, _ := publishFixture(t)
	store.mu.Lock()
	delete(store.objects, sha256Hex(manifestPayload)[:16]+"/manifest.json")
	store.mu.Unlock()

	err := (&Client{s3: client}).PullImage(context.Background(), t.TempDir(), testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "incomplete build")
}

func TestPullImageManifestDigestMismatch(t *testing.T) {
	store, client, manifestPayload, _ := publishFixture(t)
	store.mu.Lock()
	var index imageIndex
	require.NoError(t, json.Unmarshal(store.objects["index/"+imageKey(testImage)+".json"], &index))
	index.ArtifactDigest = strings.Repeat("0", 64)
	payload, err := json.Marshal(index)
	require.NoError(t, err)
	store.objects["index/"+imageKey(testImage)+".json"] = payload
	store.mu.Unlock()

	err = (&Client{s3: client}).PullImage(context.Background(), t.TempDir(), testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "digest mismatch")
	require.Contains(t, err.Error(), sha256Hex(manifestPayload))
}

func TestPullImageArtifactDigestMismatchCleansUp(t *testing.T) {
	store, client, manifestPayload, _ := publishFixture(t)
	store.mu.Lock()
	document := map[string]any{}
	require.NoError(t, json.Unmarshal(store.objects[sha256Hex(manifestPayload)[:16]+"/manifest.json"], &document))
	files := document["files"].(map[string]any)
	rootfs := files["rootfs.ext4"].(map[string]any)
	rootfs["sha256"] = strings.Repeat("1", 64)
	payload, err := json.Marshal(document)
	require.NoError(t, err)
	store.objects[sha256Hex(manifestPayload)[:16]+"/manifest.json"] = payload
	// A tampered manifest is still bound by the index digest.
	var index imageIndex
	require.NoError(t, json.Unmarshal(store.objects["index/"+imageKey(testImage)+".json"], &index))
	index.ArtifactDigest = sha256Hex(payload)
	indexPayload, err := json.Marshal(index)
	require.NoError(t, err)
	store.objects["index/"+imageKey(testImage)+".json"] = indexPayload
	store.mu.Unlock()

	root := t.TempDir()
	err = (&Client{s3: client}).PullImage(context.Background(), root, testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "digest mismatch")
	require.ErrorContains(t, err, "rootfs.ext4")

	// The failed download leaves no temporary or partial file behind.
	entries, err := os.ReadDir(imageDir(root, testImage))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPullImageRetriesTransientFailures(t *testing.T) {
	store, client, manifestPayload, _ := publishFixture(t)
	store.mu.Lock()
	store.flaky[sha256Hex(manifestPayload)[:16]+"/rootfs.ext4"] = 2
	store.mu.Unlock()

	require.NoError(t, (&Client{s3: client}).PullImage(context.Background(), t.TempDir(), testImage))
	require.Equal(t, 3, store.countRequested(sha256Hex(manifestPayload)[:16]+"/rootfs.ext4"), "two 5xx then success")
}

func TestPullImageManifestMissingFile(t *testing.T) {
	store, client, manifestPayload, _ := publishFixture(t)
	store.mu.Lock()
	document := map[string]any{}
	require.NoError(t, json.Unmarshal(store.objects[sha256Hex(manifestPayload)[:16]+"/manifest.json"], &document))
	files := document["files"].(map[string]any)
	delete(files, "vmstate.snap")
	payload, err := json.Marshal(document)
	require.NoError(t, err)
	store.objects[sha256Hex(manifestPayload)[:16]+"/manifest.json"] = payload
	var index imageIndex
	require.NoError(t, json.Unmarshal(store.objects["index/"+imageKey(testImage)+".json"], &index))
	index.ArtifactDigest = sha256Hex(payload)
	indexPayload, err := json.Marshal(index)
	require.NoError(t, err)
	store.objects["index/"+imageKey(testImage)+".json"] = indexPayload
	store.mu.Unlock()

	err = (&Client{s3: client}).PullImage(context.Background(), t.TempDir(), testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing the vmstate.snap artifact")
}

func TestPullImageRejectsOversizedManifest(t *testing.T) {
	// A manifest larger than the read limit must be rejected instead of
	// being read into memory unbounded.
	document := map[string]any{}
	require.NoError(t, json.Unmarshal(testManifest(map[string][]byte{"rootfs.ext4": []byte("rootfs")}), &document))
	document["padding"] = strings.Repeat("x", maxManifestBytes+1024)
	manifestPayload, err := json.Marshal(document)
	require.NoError(t, err)
	buildDir := sha256Hex(manifestPayload)[:16]
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}, flaky: map[string]int{}}
	store.objects[buildDir+"/manifest.json"] = manifestPayload
	store.objects[buildDir+"/rootfs.ext4"] = []byte("rootfs")
	index := imageIndex{
		Image:          testImage,
		ManifestRef:    "s3://" + testBucket + "/" + testPrefix + "/" + buildDir + "/manifest.json",
		ArtifactDigest: sha256Hex(manifestPayload),
		UpdatedAt:      "2026-08-27T00:00:00Z",
	}
	indexPayload, err := json.Marshal(index)
	require.NoError(t, err)
	store.objects["index/"+imageKey(testImage)+".json"] = indexPayload
	client := testS3Client(t, store)

	err = (&Client{s3: client}).PullImage(context.Background(), t.TempDir(), testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds the")
}

func TestPullImageConcurrent(t *testing.T) {
	_, client, _, _ := publishFixture(t)
	root := t.TempDir()
	pull := &Client{s3: client}

	done := make(chan error, 8)
	for range 8 {
		go func() { done <- pull.PullImage(context.Background(), root, testImage) }()
	}
	for range 8 {
		require.NoError(t, <-done)
	}

	// Every artifact downloaded exactly once: matching files are skipped
	// after the first commit (or coalesced by the per-file hash check).
	dir := imageDir(root, testImage)
	for _, name := range []string{"rootfs.img", "vmstate.snap", "memory.snap", manifestName} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, name)
	}
	// No temporary files survive concurrent pulls.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 4)
}

func TestPullCheckpointFullFlowWithoutIndex(t *testing.T) {
	store, client, manifestPayload, _ := publishFixture(t)
	// A checkpoint is addressed by manifest ref + digest: drop the index so
	// the pull cannot silently fall back to the image resolution path.
	delete(store.objects, "index/"+imageKey(testImage)+".json")

	digest := sha256Hex(manifestPayload)
	reference := artifacts.CheckpointReference(digest)
	manifestRef := "s3://" + testBucket + "/" + testPrefix + "/" + digest[:16] + "/manifest.json"
	root := t.TempDir()

	require.NoError(t, (&Client{s3: client}).PullCheckpoint(t.Context(), root, reference, manifestRef, digest))
	dir := imageDir(root, reference)
	complete, err := cacheComplete(dir)
	require.NoError(t, err)
	require.True(t, complete, "the checkpoint artifact set is committed in the local cache")

	before := len(store.requested())
	require.NoError(t, (&Client{s3: client}).PullCheckpoint(t.Context(), root, reference, manifestRef, digest))
	require.Len(t, store.requested(), before, "a committed checkpoint cache is not re-pulled")
}

func TestPullCheckpointRejectsDigestMismatch(t *testing.T) {
	_, client, manifestPayload, _ := publishFixture(t)
	digest := sha256Hex(manifestPayload)
	manifestRef := "s3://" + testBucket + "/" + testPrefix + "/" + digest[:16] + "/manifest.json"
	err := (&Client{s3: client}).PullCheckpoint(t.Context(), t.TempDir(), artifacts.CheckpointReference("other"), manifestRef, strings.Repeat("0", 64))
	require.ErrorContains(t, err, "digest mismatch")
}

func TestPullCheckpointRequiresAddress(t *testing.T) {
	_, client, _, _ := publishFixture(t)
	require.Error(t, (&Client{s3: client}).PullCheckpoint(t.Context(), t.TempDir(), "", "s3://b/m.json", "digest"))
	require.Error(t, (&Client{s3: client}).PullCheckpoint(t.Context(), t.TempDir(), artifacts.CheckpointReference("d"), "", ""))
}
