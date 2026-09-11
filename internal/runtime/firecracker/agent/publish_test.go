package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/registryconfig"

	"github.com/stretchr/testify/require"
)

// fakePublishStore records every PUT: its key, body, and the order of arrival.
type fakePublishStore struct {
	mu        sync.Mutex
	puts      []string
	bodies    map[string][]byte
	authed    map[string]string
	failFirst map[string]int // key suffix -> respond 500 on the first N attempts
}

func newFakePublishStore() *fakePublishStore {
	return &fakePublishStore{bodies: map[string][]byte{}, authed: map[string]string{}, failFirst: map[string]int{}}
}

func (s *fakePublishStore) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.Trim(r.URL.Path, "/")
	if s.failFirst[key] > 0 {
		s.failFirst[key]--
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"transient"}`))
		return
	}
	s.puts = append(s.puts, key)
	s.bodies[key] = body
	s.authed[key] = r.Header.Get("Authorization")
	w.WriteHeader(http.StatusOK)
}

func (s *fakePublishStore) ordered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

func (s *fakePublishStore) body(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[key]
}

// stageArtifactSet writes a minimal native artifact set whose manifest is
// byte-stable for the expected digest.
func stageArtifactSet(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{
		"schemaVersion": 1,
		"runtime":       "firecracker",
		"machine":       map[string]any{"vcpu": "2", "memory": "1Gi"},
		"format":        "native",
	}
	manifestBytes, err := artifacts.MarshalManifest(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("rootfs"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vmstate.snap"), []byte("vmstate"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.snap"), []byte("memory"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("sums\n"), 0o644))
	return dir
}

func writeCredential() registryconfig.Credential {
	return registryconfig.Credential{
		Host: "store.example.com", Username: "reader", Password: "reader-secret",
		WriteUsername: "writer", WritePassword: "writer-secret",
	}
}

func TestPublishImageUploadOrderAndIndexLayout(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	dir := stageArtifactSet(t)

	result, err := client.PublishImage(t.Context(), "app-v2", dir)
	require.NoError(t, err)

	digest16 := artifacts.Digest16(mustManifestBytes(t, dir))
	// The fake store serves path-style URLs, so every key carries the bucket.
	base := "bucket/publish/" + digest16
	require.Equal(t, []string{
		base + "/rootfs.ext4",
		base + "/vmstate.snap",
		base + "/memory.snap",
		base + "/SHA256SUMS",
		base + "/manifest.json",
		"bucket/publish/index/" + artifacts.ImageIndexKey("app-v2") + ".json",
	}, store.ordered(), "upload order: artifacts, checksums, manifest, then the index last")

	require.Equal(t, "s3://bucket/publish/"+digest16+"/manifest.json", result.ManifestRef)
	require.Equal(t, artifacts.SHA256Of(mustManifestBytes(t, dir)), result.ArtifactDigest)

	var index struct {
		Image          string `json:"image"`
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
	}
	require.NoError(t, json.Unmarshal(store.body("bucket/publish/index/"+artifacts.ImageIndexKey("app-v2")+".json"), &index))
	require.Equal(t, "app-v2", index.Image, "payload image equals the key it is written under")
	require.Equal(t, result.ManifestRef, index.ManifestRef)
	require.Equal(t, result.ArtifactDigest, index.ArtifactDigest)

	require.Contains(t, store.authed[base+"/manifest.json"], "Credential=writer/",
		"PUTs sign with the write credential")
}

func TestPublishImageRetriesTransientFailureWithFreshBody(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	// 500 on the FIRST attempt of every object: the retry must open a fresh
	// body (net/http closes request bodies after each attempt; seeking the
	// closed *os.File was the field bug).
	dir := stageArtifactSet(t)
	digest16 := artifacts.Digest16(mustManifestBytes(t, dir))
	for _, name := range []string{"rootfs.ext4", "vmstate.snap", "memory.snap", "SHA256SUMS", "manifest.json"} {
		store.failFirst["bucket/publish/"+digest16+"/"+name] = 1
	}

	result, err := client.PublishImage(t.Context(), "app-v2", dir)
	require.NoError(t, err)
	require.Contains(t, result.ManifestRef, digest16)
	// Every object landed exactly once (the failed attempts are not recorded).
	require.Len(t, store.ordered(), 6)
}

func TestPublishImageRefusesReadOnlyStore(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	readOnly := writeCredential()
	readOnly.WriteUsername, readOnly.WritePassword = "", ""
	client, err := NewClient("s3://bucket/publish", readOnly, WithEndpoint(server.URL))
	require.NoError(t, err)

	_, err = client.PublishImage(t.Context(), "app-v2", stageArtifactSet(t))
	require.ErrorIs(t, err, ErrNotWritable)
	require.Empty(t, store.ordered())
}

func TestPublishImageRequiresStagedManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newFakePublishStore().handle))
	defer server.Close()
	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)

	_, err = client.PublishImage(t.Context(), "app-v2", t.TempDir())
	require.ErrorContains(t, err, "read staged manifest")
}

func mustManifestBytes(t *testing.T, dir string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)
	return payload
}
