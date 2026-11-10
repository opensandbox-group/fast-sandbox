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

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/registryconfig"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
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
	if r.Method == http.MethodGet {
		s.mu.Lock()
		body, ok := s.bodies[strings.Trim(r.URL.Path, "/")]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
		return
	}
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
	manifest := artifacts.VMManifest{
		SchemaVersion: artifacts.VMManifestVersion,
		Runtime:       "firecracker",
		Machine:       artifacts.VMMachine{VCPUCount: 2, MemoryMiB: 1024, RootfsSizeMiB: 1, RootfsDriveID: "rootfs", RootfsDrivePath: "rootfs.img"},
		Kernel:        artifacts.VMKernel{Version: "6.1.177"},
		Compatibility: artifacts.SnapshotCompatibility{CPUArch: "x86_64", FirecrackerVersion: "1.16.1", CPUTemplate: artifacts.CPUTemplateNone, Vendor: artifacts.VendorGenuineIntel, CPUFamily: 6, CPUModel: 85, CPUStepping: 7},
		Files: map[string]artifacts.ManifestFile{
			"rootfs.ext4":  {SHA256: artifacts.SHA256Of([]byte("rootfs")), SizeBytes: 6},
			"vmstate.snap": {SHA256: artifacts.SHA256Of([]byte("vmstate")), SizeBytes: 7},
			"memory.snap":  {SHA256: artifacts.SHA256Of([]byte("memory")), SizeBytes: 6},
		},
	}
	manifestBytes, err := artifacts.MarshalVMManifest(manifest)
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

	result, err := client.PublishImage(t.Context(), "", "app-v2", dir)
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

func TestPublishCheckpointUploadsOptionalVaultBundle(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)

	// A staged vault bundle file joins the artifact uploads.
	dir := stageArtifactSet(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, publishVaultName), []byte(`{"v":1,"vault":{}}`), 0o600))
	_, err = client.PublishImage(t.Context(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)
	vaultKeys := 0
	for _, key := range store.ordered() {
		if strings.HasSuffix(key, "/"+publishVaultName) {
			vaultKeys++
			require.Equal(t, []byte(`{"v":1,"vault":{}}`), store.body(key))
		}
	}
	require.Equal(t, 1, vaultKeys, "the vault bundle uploads exactly once, before the manifest")

	// A set without a vault bundle uploads none.
	store2 := newFakePublishStore()
	server2 := httptest.NewServer(http.HandlerFunc(store2.handle))
	defer server2.Close()
	client2, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server2.URL))
	require.NoError(t, err)
	dir2 := stageArtifactSet(t)
	_, err = client2.PublishImage(t.Context(), agentprotocol.PublishKindCheckpoint, "", dir2)
	require.NoError(t, err)
	for _, key := range store2.ordered() {
		require.NotContains(t, key, publishVaultName)
	}
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

	result, err := client.PublishImage(t.Context(), "", "app-v2", dir)
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

	_, err = client.PublishImage(t.Context(), "", "app-v2", stageArtifactSet(t))
	require.ErrorIs(t, err, ErrNotWritable)
	require.Empty(t, store.ordered())
}

func TestPublishImageRequiresStagedManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newFakePublishStore().handle))
	defer server.Close()
	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)

	_, err = client.PublishImage(t.Context(), "", "app-v2", t.TempDir())
	require.ErrorContains(t, err, "read staged manifest")
}

func mustManifestBytes(t *testing.T, dir string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)
	return payload
}

func TestPublishCheckpointSkipsIndex(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	dir := stageArtifactSet(t)

	result, err := client.PublishImage(t.Context(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)

	digest16 := artifacts.Digest16(mustManifestBytes(t, dir))
	base := "bucket/publish/" + digest16
	require.Equal(t, []string{
		base + "/rootfs.ext4",
		base + "/vmstate.snap",
		base + "/memory.snap",
		base + "/SHA256SUMS",
		base + "/manifest.json",
	}, store.ordered(), "a checkpoint set is complete without an image index")
	require.Equal(t, "s3://bucket/publish/"+digest16+"/manifest.json", result.ManifestRef)
	require.Equal(t, artifacts.SHA256Of(mustManifestBytes(t, dir)), result.ArtifactDigest)
}

func TestPublishCheckpointRejectsIndexKey(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	_, err = client.PublishImage(t.Context(), agentprotocol.PublishKindCheckpoint, "app-v2", stageArtifactSet(t))
	require.ErrorContains(t, err, "key must be empty")
	require.Empty(t, store.ordered(), "a rejected checkpoint publishes nothing")
}

func TestPublishTemplateKindRequiresKey(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()

	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	_, err = client.PublishImage(t.Context(), agentprotocol.PublishKindTemplate, "", stageArtifactSet(t))
	require.ErrorContains(t, err, "publish key is required")
}

// stageRootfsArtifactSet models the native cold-boot artifact contract.
func stageRootfsArtifactSet(t *testing.T) string {
	t.Helper()
	dir := stageArtifactSet(t)
	document, err := artifacts.DecodeVMManifest(mustManifestBytes(t, dir))
	require.NoError(t, err)
	document.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly"}
	document.Kernel.Cmdline = "console=ttyS0 root=/dev/vda rw"
	for _, name := range []string{manifestVMStateName, manifestMemoryName} {
		delete(document.Files, name)
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
	policy := []byte(`{"version":1}`)
	document.Platform.StateFiles = map[string]artifacts.ManifestFile{
		publishPolicyName: {SHA256: artifacts.SHA256Of(policy), SizeBytes: int64(len(policy))},
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, publishPolicyName), policy, 0o600))
	payload, err := artifacts.MarshalVMManifest(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, publishManifestName), payload, 0o600))
	return dir
}

func TestRootfsOnlyPublishPullRoundTrip(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	dir := stageRootfsArtifactSet(t)
	result, err := client.PublishImage(t.Context(), "", "rootfs-template", dir)
	require.NoError(t, err)
	base := "bucket/publish/" + artifacts.Digest16(mustManifestBytes(t, dir))
	require.Equal(t, []string{base + "/rootfs.ext4", base + "/policy.json", base + "/SHA256SUMS", base + "/manifest.json",
		"bucket/publish/index/" + artifacts.ImageIndexKey("rootfs-template") + ".json"}, store.ordered())

	root := t.TempDir()
	require.NoError(t, client.PullImage(t.Context(), root, "rootfs-template"))
	cache := imageDir(root, "rootfs-template")
	for _, name := range []string{manifestVMStateName, manifestMemoryName} {
		require.NoFileExists(t, filepath.Join(cache, name))
	}
	data, err := os.ReadFile(filepath.Join(cache, nativeRootfsCacheName))
	require.NoError(t, err)
	require.Equal(t, []byte("rootfs"), data)
	ready, err := ImageReady(root, "rootfs-template")
	require.NoError(t, err)
	require.True(t, ready)

	// Both disk corruption and a missing platform bundle invalidate readiness.
	for _, name := range []string{nativeRootfsCacheName, publishPolicyName} {
		require.NoError(t, os.Remove(filepath.Join(cache, name)))
		ready, err = ImageReady(root, "rootfs-template")
		require.NoError(t, err)
		require.False(t, ready)
		require.NoError(t, client.PullImage(t.Context(), root, "rootfs-template"))
		require.FileExists(t, filepath.Join(cache, name))
	}

	// A cold-boot artifact cannot become a memory checkpoint via either entry.
	for _, reference := range []string{"rootfs-template", "uncached-checkpoint"} {
		err = client.PullCheckpoint(t.Context(), root, reference, result.ManifestRef, result.ArtifactDigest)
		require.ErrorContains(t, err, "rootfsOnly cannot be restored as a checkpoint")
	}
	server.Close()
	require.NoError(t, client.PullImage(t.Context(), root, "rootfs-template"), "a complete cache must work offline")
}

func TestRootfsOnlyPublishRejectsInvalidStagingBeforeUpload(t *testing.T) {
	for _, scenario := range []string{"checkpoint", "vmstate.snap", "memory.snap", "missing-rootfs-entry", "unknown-mode", "missing-policy", "truncated-rootfs"} {
		t.Run(scenario, func(t *testing.T) {
			store := newFakePublishStore()
			server := httptest.NewServer(http.HandlerFunc(store.handle))
			defer server.Close()
			client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
			require.NoError(t, err)
			dir := stageRootfsArtifactSet(t)
			kind, key := agentprotocol.PublishKindTemplate, "rootfs-template"
			switch scenario {
			case "checkpoint":
				kind, key = agentprotocol.PublishKindCheckpoint, ""
			case "missing-policy":
				require.NoError(t, os.Remove(filepath.Join(dir, publishPolicyName)))
			case "truncated-rootfs":
				require.NoError(t, os.Truncate(filepath.Join(dir, "rootfs.ext4"), 1))
			case "vmstate.snap", "memory.snap":
				require.NoError(t, os.WriteFile(filepath.Join(dir, scenario), []byte("stale"), 0o600))
			default:
				document, err := artifacts.DecodeVMManifest(mustManifestBytes(t, dir))
				require.NoError(t, err)
				if scenario == "unknown-mode" {
					document.Platform.SnapshotMode = "futureMode"
				} else {
					delete(document.Files, "rootfs.ext4")
				}
				payload, err := json.Marshal(document)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, publishManifestName), payload, 0o600))
			}
			_, err = client.PublishImage(t.Context(), kind, key, dir)
			require.Error(t, err)
			require.Empty(t, store.ordered(), "invalid metadata must not publish any object")
		})
	}
}

func TestRootfsOnlyPullRejectsCorruptDisk(t *testing.T) {
	store := newFakePublishStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	client, err := NewClient("s3://bucket/publish", writeCredential(), WithEndpoint(server.URL))
	require.NoError(t, err)
	dir := stageRootfsArtifactSet(t)
	_, err = client.PublishImage(t.Context(), "", "rootfs-template", dir)
	require.NoError(t, err)
	store.mu.Lock()
	store.bodies["bucket/publish/"+artifacts.Digest16(mustManifestBytes(t, dir))+"/rootfs.ext4"] = []byte("broken")
	store.mu.Unlock()
	root := t.TempDir()
	err = client.PullImage(t.Context(), root, "rootfs-template")
	require.ErrorContains(t, err, "digest mismatch")
	require.NoFileExists(t, filepath.Join(imageDir(root, "rootfs-template"), manifestName))
}
