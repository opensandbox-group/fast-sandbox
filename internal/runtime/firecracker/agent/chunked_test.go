package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

const testChunkSize = 64 << 10

// fakeObjectStore is an S3-ish store serving PUT/GET/HEAD by path. Like
// RGW, it rejects a PUT whose signed x-amz-content-sha256 does not match.
type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	gets    []string
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

func (s *fakeObjectStore) handle(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if claimed := r.Header.Get("X-Amz-Content-Sha256"); claimed != unsignedPayload && claimed != sha256Hex(body) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.objects[key] = body
		s.puts = append(s.puts, key)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		body, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			s.gets = append(s.gets, key)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *fakeObjectStore) putKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

func (s *fakeObjectStore) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key]
}

func (s *fakeObjectStore) setObject(key string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = body
}

// stagedFiles builds artifacts with zero chunks, data chunks, a repeated
// chunk and a partial tail, so every chunking branch is exercised.
func stagedFiles() map[string][]byte {
	data := func(seed byte, n int) []byte {
		block := make([]byte, n)
		for i := range block {
			block[i] = seed + byte(i%251)
		}
		return block
	}
	zero := make([]byte, testChunkSize)
	rootfs := bytes.Join([][]byte{zero, data(1, testChunkSize), zero, data(1, testChunkSize), data(7, 1000)}, nil)
	memory := bytes.Join([][]byte{data(3, testChunkSize), zero, zero}, nil)
	return map[string][]byte{
		"rootfs.ext4":       rootfs,
		manifestMemoryName:  memory,
		manifestVMStateName: data(9, 777),
	}
}

// stageSet writes files plus a VM manifest describing them; the optional
// state bundles (policy.json, vault.json) land in platform.stateFiles, and
// extra adds unknown top-level fields.
func stageSet(t *testing.T, files map[string][]byte, extra map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	manifest := artifacts.VMManifest{
		SchemaVersion: artifacts.VMManifestVersion,
		Runtime:       "firecracker",
		Machine:       artifacts.VMMachine{VCPUCount: 2, MemoryMiB: 1024, RootfsSizeMiB: 1, RootfsDriveID: "rootfs", RootfsDrivePath: "rootfs.img"},
		Kernel:        artifacts.VMKernel{Version: "6.1.177"},
		Compatibility: artifacts.SnapshotCompatibility{CPUArch: "x86_64", FirecrackerVersion: "1.16.1", CPUTemplate: artifacts.CPUTemplateNone, Vendor: artifacts.VendorGenuineIntel, CPUFamily: 6, CPUModel: 85, CPUStepping: 7},
		Files:         map[string]artifacts.ManifestFile{},
	}
	var names []string
	for name, payload := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, payload, 0o640))
		entry, err := artifacts.FileEntry(path, nil)
		require.NoError(t, err)
		names = append(names, name)
		if name == publishVaultName || name == publishPolicyName {
			if manifest.Platform == nil {
				manifest.Platform = &artifacts.VMPlatform{StateFiles: map[string]artifacts.ManifestFile{}}
			}
			manifest.Platform.StateFiles[name] = entry
			continue
		}
		manifest.Files[name] = entry
	}
	sort.Strings(names)
	require.NoError(t, artifacts.WriteSHA256SUMS(dir, names, nil))
	payload, err := artifacts.MarshalVMManifest(manifest)
	require.NoError(t, err)
	if len(extra) > 0 {
		document := map[string]any{}
		require.NoError(t, json.Unmarshal(payload, &document))
		for key, value := range extra {
			document[key] = value
		}
		payload, err = json.MarshalIndent(document, "", "  ")
		require.NoError(t, err)
		payload = append(payload, '\n')
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, publishManifestName), payload, 0o644))
	return dir
}

func newTestClient(t *testing.T, server *httptest.Server, transfer TransferConfig) *Client {
	t.Helper()
	credential := writeCredential()
	credential.Endpoint = server.URL
	client, err := NewClient("s3://bucket/publish", credential, WithTransfer(transfer))
	require.NoError(t, err)
	client.s3.retryDelay = 0
	return client
}

func chunkedTransfer(compression string) TransferConfig {
	return TransferConfig{Format: TransferFormatChunked, Compression: compression, Concurrency: 4, ChunkSize: testChunkSize}
}

func TestChunkedPublishPullRoundTrip(t *testing.T) {
	for _, compression := range []string{TransferCompressionNone, TransferCompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			store := newFakeObjectStore()
			server := httptest.NewServer(http.HandlerFunc(store.handle))
			defer server.Close()
			files := stagedFiles()
			dir := stageSet(t, files, nil)
			client := newTestClient(t, server, chunkedTransfer(compression))

			result, err := client.PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
			require.NoError(t, err)

			// Only chunks and metadata are uploaded; zero chunks and the
			// duplicated rootfs chunk are not.
			chunkPuts := 0
			for _, key := range store.putKeys() {
				require.NotContains(t, []string{"rootfs.ext4", manifestMemoryName, manifestVMStateName}, filepath.Base(key),
					"whole-file artifact %s uploaded", key)
				if strings.HasPrefix(key, "publish/"+artifacts.ChunkStorePrefix) {
					chunkPuts++
				}
			}
			require.Equal(t, 4, chunkPuts, "rootfs data (deduped) + rootfs tail + memory + vmstate")

			// The published manifest binds the chunk index and is the
			// digest the caller got back; the staged manifest matches it.
			base := strings.TrimSuffix(strings.TrimPrefix(result.ManifestRef, "s3://bucket/publish/"), "/manifest.json")
			published := store.object("publish/" + base + "/manifest.json")
			require.Equal(t, result.ArtifactDigest, sha256Hex(published))
			staged, err := os.ReadFile(filepath.Join(dir, publishManifestName))
			require.NoError(t, err)
			require.Equal(t, published, staged)
			binding, err := chunkBinding(published)
			require.NoError(t, err)
			require.NotNil(t, binding)

			stateRoot := t.TempDir()
			reference := artifacts.CheckpointReference(result.ArtifactDigest)
			puller := newTestClient(t, server, TransferConfig{})
			require.NoError(t, puller.PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
			cacheDir := imageDir(stateRoot, reference)
			for name, payload := range files {
				cacheName := name
				if name == "rootfs.ext4" {
					cacheName = nativeRootfsCacheName
				}
				got, err := os.ReadFile(filepath.Join(cacheDir, cacheName))
				require.NoError(t, err)
				require.True(t, bytes.Equal(payload, got), "%s content differs after round trip", name)
			}
			ready, err := ImageReady(stateRoot, reference)
			require.NoError(t, err)
			require.True(t, ready)
		})
	}
}

func TestChunkedPublishSkipsHolesOfSparseFiles(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	files := stagedFiles()
	dir := stageSet(t, files, nil)
	// Rewrite the rootfs as a genuinely sparse file with the same content
	// plus a long trailing hole: data at chunk 1 and 3, holes elsewhere.
	rootfs := append(append([]byte(nil), files["rootfs.ext4"]...), make([]byte, 40*testChunkSize)...)
	path := filepath.Join(dir, "rootfs.ext4")
	handle, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, handle.Truncate(int64(len(rootfs))))
	for offset := 0; offset < len(rootfs); offset += testChunkSize {
		block := rootfs[offset:min(offset+testChunkSize, len(rootfs))]
		if !artifacts.IsZero(block) {
			_, err := handle.WriteAt(block, int64(offset))
			require.NoError(t, err)
		}
	}
	require.NoError(t, handle.Close())
	files["rootfs.ext4"] = rootfs
	dir = stageSet(t, files, nil)
	require.NoError(t, os.Remove(filepath.Join(dir, "rootfs.ext4")))
	require.NoError(t, os.Rename(path, filepath.Join(dir, "rootfs.ext4")))

	result, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)

	stateRoot := t.TempDir()
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	require.NoError(t, newTestClient(t, server, TransferConfig{}).
		PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
	got, err := os.ReadFile(filepath.Join(imageDir(stateRoot, reference), nativeRootfsCacheName))
	require.NoError(t, err)
	require.True(t, bytes.Equal(rootfs, got))
}

// TestChunkedTransferRetriesStalledConnection models the field failure: one
// chunk request hangs (a collapsed TCP connection behind a lossy policer).
// The per-attempt timeout must drop it and the retry must succeed, for both
// the publish (PUT) and the pull (GET) side.
func TestChunkedTransferRetriesStalledConnection(t *testing.T) {
	previous := chunkAttemptTimeout
	chunkAttemptTimeout = 200 * time.Millisecond
	defer func() { chunkAttemptTimeout = previous }()

	store := newFakeObjectStore()
	var stalledPut, stalledGet atomic.Bool
	release := make(chan struct{})
	stall := func(r *http.Request) bool {
		if !strings.Contains(r.URL.Path, artifacts.ChunkStorePrefix) {
			return false
		}
		switch r.Method {
		case http.MethodPut:
			return stalledPut.CompareAndSwap(false, true)
		case http.MethodGet:
			return stalledGet.CompareAndSwap(false, true)
		}
		return false
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall(r) {
			// Hang until the test ends: a handler that never reads its body
			// is not canceled when the client drops the connection.
			<-release
			return
		}
		store.handle(w, r)
	}))
	defer server.Close()
	defer close(release) // runs first: frees parked handlers before Close waits on them

	files := stagedFiles()
	result, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, files, nil))
	require.NoError(t, err)
	require.True(t, stalledPut.Load(), "a chunk PUT was stalled")

	stateRoot := t.TempDir()
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	require.NoError(t, newTestClient(t, server, TransferConfig{}).
		PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
	require.True(t, stalledGet.Load(), "a chunk GET was stalled")
	got, err := os.ReadFile(filepath.Join(imageDir(stateRoot, reference), nativeRootfsCacheName))
	require.NoError(t, err)
	require.True(t, bytes.Equal(files["rootfs.ext4"], got))
}

func TestChunkedPublishReusesStoredChunks(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	files := stagedFiles()

	_, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, files, nil))
	require.NoError(t, err)
	before := len(store.putKeys())

	// A fresh client (no in-memory known set) must find the chunks via HEAD
	// and upload only the per-build metadata.
	_, err = newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, files, map[string]any{"rebuild": "second"}))
	require.NoError(t, err)
	for _, key := range store.putKeys()[before:] {
		require.False(t, strings.HasPrefix(key, "publish/"+artifacts.ChunkStorePrefix), "chunk %s re-uploaded", key)
	}
}

func TestLegacyPublishDropsInheritedChunkBinding(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	inherited := map[string]any{artifacts.ManifestChunksField: map[string]any{"version": 1, "ref": "chunks.json", "sha256": strings.Repeat("a", 64), "sizeBytes": 1}}
	dir := stageSet(t, stagedFiles(), inherited)

	result, err := newTestClient(t, server, TransferConfig{}).PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)
	base := strings.TrimSuffix(strings.TrimPrefix(result.ManifestRef, "s3://bucket/publish/"), "/manifest.json")
	var document map[string]any
	require.NoError(t, json.Unmarshal(store.object("publish/"+base+"/manifest.json"), &document))
	require.NotContains(t, document, artifacts.ManifestChunksField)
	require.NotEmpty(t, store.object("publish/"+base+"/rootfs.ext4"), "legacy publish uploads whole files")
}

func TestChunkedPullRejectsCorruptChunk(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	result, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, stagedFiles(), nil))
	require.NoError(t, err)
	for _, key := range store.putKeys() {
		if strings.HasPrefix(key, "publish/"+artifacts.ChunkStorePrefix) {
			body := append([]byte(nil), store.object(key)...)
			body[0] ^= 0xff
			store.setObject(key, body)
			break
		}
	}
	stateRoot := t.TempDir()
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	err = newTestClient(t, server, TransferConfig{}).PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest)
	require.ErrorContains(t, err, "digest mismatch")
	ready, readyErr := ImageReady(stateRoot, reference)
	require.NoError(t, readyErr)
	require.False(t, ready, "a failed pull must not leave a committed cache entry")
}

func TestSparseWriterPreservesContent(t *testing.T) {
	payload := bytes.Join([][]byte{make([]byte, 3*sparseBlock), bytes.Repeat([]byte{5}, 100), make([]byte, sparseBlock+17)}, nil)
	tmp, err := os.CreateTemp(t.TempDir(), "sparse")
	require.NoError(t, err)
	written, err := io.Copy(sparseWriter{file: tmp}, bytes.NewReader(payload))
	require.NoError(t, err)
	require.NoError(t, tmp.Truncate(written))
	require.NoError(t, tmp.Close())
	got, err := os.ReadFile(tmp.Name())
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got))
}

func TestFileVerifiedMarkerInvalidatesOnChange(t *testing.T) {
	if currentBootID() == "" {
		t.Skip("no boot id: the marker is disabled on this platform")
	}
	path := filepath.Join(t.TempDir(), "artifact")
	payload := []byte("verified artifact")
	require.NoError(t, os.WriteFile(path, payload, 0o640))
	digest := sha256.Sum256(payload)
	file := nativeFile{sha256: hex.EncodeToString(digest[:]), sizeBytes: int64(len(payload))}

	match, err := fileVerified(path, file)
	require.NoError(t, err)
	require.True(t, match)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if !markerMatches(path, info, file.sha256) {
		t.Skip("filesystem without user xattrs")
	}

	// Same size, different content, later mtime: the marker no longer
	// applies and the full check catches the mismatch. (The explicit
	// Chtimes models a later edit: the kernel's coarse mtime clock can give
	// a rewrite within the same tick the identical timestamp.)
	require.NoError(t, os.WriteFile(path, []byte("tampered artifact"), 0o640))
	later := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(path, later, later))
	match, err = fileVerified(path, file)
	require.NoError(t, err)
	require.False(t, match)
}
