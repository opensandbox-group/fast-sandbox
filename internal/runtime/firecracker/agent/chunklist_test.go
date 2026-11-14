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
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// stageChunkListSet stages a set the way a chunk-list-aware fastlet does: the
// native artifacts carry only their size, no whole-file sha256.
func stageChunkListSet(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := stageSet(t, files, nil)
	path := filepath.Join(dir, publishManifestName)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	document := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &document))
	for name, entry := range document["files"].(map[string]any) {
		delete(entry.(map[string]any), "sha256")
		document["files"].(map[string]any)[name] = entry
	}
	raw, err = json.MarshalIndent(document, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o644))
	return dir
}

func publishedManifest(t *testing.T, store *fakeObjectStore, result PublishResult) (string, []byte) {
	t.Helper()
	base := strings.TrimSuffix(strings.TrimPrefix(result.ManifestRef, "s3://bucket/publish/"), "/manifest.json")
	return base, store.object("publish/" + base + "/manifest.json")
}

func TestChunkListPublishPullRoundTrip(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	files := stagedFiles()
	dir := stageChunkListSet(t, files)
	client := newTestClient(t, server, chunkedTransfer(TransferCompressionNone))

	result, err := client.PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)
	_, published := publishedManifest(t, store, result)
	document, err := artifacts.DecodeVMManifest(published)
	require.NoError(t, err)
	for _, name := range []string{"rootfs.ext4", manifestMemoryName, manifestVMStateName} {
		entry := document.Files[name]
		require.Empty(t, entry.SHA256, "%s must stay without a whole-file sha256", name)
		require.NotNil(t, entry.ChunkList, "%s needs its chunk-list digest", name)
		want, err := artifacts.FileChunkListSHA256(filepath.Join(dir, name), testChunkSize, 2)
		require.NoError(t, err)
		require.Equal(t, want, entry.ChunkList.SHA256, "%s chunk-list digest", name)
	}

	// A legacy-mode puller re-verifies through the chunk-list re-hash; a
	// chunked one trusts the marker the pull wrote.
	for _, transfer := range []TransferConfig{{}, chunkedTransfer(TransferCompressionNone)} {
		stateRoot := t.TempDir()
		reference := artifacts.CheckpointReference(result.ArtifactDigest)
		puller := newTestClient(t, server, transfer)
		require.NoError(t, puller.PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
		for name, payload := range files {
			cacheName := name
			if name == "rootfs.ext4" {
				cacheName = nativeRootfsCacheName
			}
			got, err := os.ReadFile(filepath.Join(imageDir(stateRoot, reference), cacheName))
			require.NoError(t, err)
			require.True(t, bytes.Equal(payload, got), "%s content differs", name)
		}
		ready, err := ImageReady(stateRoot, reference)
		require.NoError(t, err)
		require.True(t, ready, "full re-verify of a chunk-list set")
		ready, err = ImageReadyVerified(stateRoot, reference)
		require.NoError(t, err)
		require.True(t, ready, "marker-fronted verify of a chunk-list set")

		// A corrupted cached file fails the re-verify.
		memory := filepath.Join(imageDir(stateRoot, reference), manifestMemoryName)
		payload, err := os.ReadFile(memory)
		require.NoError(t, err)
		payload[0] ^= 0xff
		require.NoError(t, os.WriteFile(memory, payload, 0o640))
		ready, err = ImageReady(stateRoot, reference)
		require.NoError(t, err)
		require.False(t, ready, "a changed byte must fail the chunk-list re-verify")
	}
}

// A staged whole-file sha256 is kept next to the chunk-list digest, so agents
// that predate chunk-list digests still read sets published by new ones.
func TestChunkedPublishKeepsWholeFileSHA256(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	dir := stageSet(t, stagedFiles(), nil)
	client := newTestClient(t, server, chunkedTransfer(TransferCompressionNone))
	result, err := client.PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)
	_, published := publishedManifest(t, store, result)
	document, err := artifacts.DecodeVMManifest(published)
	require.NoError(t, err)
	for _, name := range []string{"rootfs.ext4", manifestMemoryName, manifestVMStateName} {
		require.True(t, validSHA256(document.Files[name].SHA256), "%s keeps its sha256", name)
		require.NotNil(t, document.Files[name].ChunkList, "%s also gets its chunk-list digest", name)
	}
}

// A manifest whose chunk-list digest does not describe the bound index is
// rejected before any chunk is trusted.
func TestChunkedPullRejectsChunkListMismatch(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	dir := stageChunkListSet(t, stagedFiles())
	client := newTestClient(t, server, chunkedTransfer(TransferCompressionNone))
	result, err := client.PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)
	base, published := publishedManifest(t, store, result)

	document := map[string]any{}
	require.NoError(t, json.Unmarshal(published, &document))
	memory := document["files"].(map[string]any)[manifestMemoryName].(map[string]any)
	memory["chunkList"].(map[string]any)["sha256"] = strings.Repeat("ab", 32)
	forged, err := json.MarshalIndent(document, "", "  ")
	require.NoError(t, err)
	forged = append(forged, '\n')
	store.setObject("publish/"+base+"/manifest.json", forged)

	digest := sha256Hex(forged)
	err = newTestClient(t, server, TransferConfig{}).PullCheckpoint(context.Background(), t.TempDir(),
		artifacts.CheckpointReference(digest), result.ManifestRef, digest)
	require.ErrorContains(t, err, "chunk-list digest")
}

func TestNativeFilesNeedADigest(t *testing.T) {
	list := &artifacts.ChunkListDigest{Version: 1, ChunkSize: testChunkSize, SHA256: strings.Repeat("cd", 32)}
	entries := map[string]artifacts.ManifestFile{
		"rootfs.ext4":       {SHA256: strings.Repeat("ab", 32), SizeBytes: 1},
		manifestVMStateName: {SizeBytes: 1, ChunkList: list},
		manifestMemoryName:  {SizeBytes: 1},
	}
	_, err := manifest{Files: entries}.nativeFiles()
	require.ErrorContains(t, err, "memory.snap has an invalid sha256", "an entry with neither digest fails closed")

	entries[manifestMemoryName] = artifacts.ManifestFile{SizeBytes: 1, ChunkList: &artifacts.ChunkListDigest{Version: 9, ChunkSize: testChunkSize, SHA256: list.SHA256}}
	_, err = manifest{Files: entries}.nativeFiles()
	require.ErrorContains(t, err, "unsupported chunk-list digest version")

	entries[manifestMemoryName] = artifacts.ManifestFile{SizeBytes: 1, ChunkList: list}
	files, err := manifest{Files: entries}.nativeFiles()
	require.NoError(t, err)
	for _, file := range files {
		switch file.publish {
		case "rootfs.ext4":
			require.Equal(t, strings.Repeat("ab", 32), file.markerSum(), "a whole-file sha256 stays the marker identity")
		default:
			require.Equal(t, chunkListMarkerPrefix+list.SHA256, file.markerSum())
		}
	}

	// A whole-file download has nothing to check a chunk-list-only entry
	// against.
	err = stageFile(context.Background(), &Client{}, t.TempDir(), "unused", nativeFile{publish: manifestMemoryName, cache: manifestMemoryName, sizeBytes: 1, chunkList: list})
	require.ErrorContains(t, err, "no whole-file sha256")
}
