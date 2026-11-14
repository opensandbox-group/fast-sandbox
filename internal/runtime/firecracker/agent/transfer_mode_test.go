package agent

import (
	"bytes"
	"context"
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

// The legacy transfer mode is the default and must keep the original
// whole-file behaviour; these tests pin it down next to the chunked mode.

func TestTransferConfigValidate(t *testing.T) {
	require.NoError(t, TransferConfig{}.Validate())
	require.False(t, TransferConfig{}.Chunked(), "the empty config is legacy")
	require.True(t, TransferConfig{Format: TransferFormatChunked}.Chunked())
	require.NoError(t, TransferConfig{Format: TransferFormatChunked, Compression: TransferCompressionZstd}.Validate())

	require.ErrorContains(t, TransferConfig{Format: "fast"}.Validate(), "unknown transfer format")
	require.ErrorContains(t, TransferConfig{Compression: "gzip"}.Validate(), "unknown transfer compression")
	require.ErrorContains(t, TransferConfig{Compression: TransferCompressionZstd}.Validate(), "requires format chunked")
	require.ErrorContains(t, TransferConfig{Format: TransferFormatChunked, ChunkSize: 1000}.Validate(), "multiple of 64KiB")
}

func TestLegacyClientKeepsDefaultTransport(t *testing.T) {
	legacy, err := NewClient("s3://bucket/publish", writeCredential())
	require.NoError(t, err)
	require.Nil(t, legacy.s3.http.Transport, "legacy keeps net/http's default transport")

	chunked, err := NewClient("s3://bucket/publish", writeCredential(), WithTransfer(TransferConfig{Format: TransferFormatChunked}))
	require.NoError(t, err)
	transport, ok := chunked.s3.http.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, storeConnsPerHost, transport.MaxIdleConnsPerHost)
	require.Equal(t, storeIdleConnTimeout, transport.IdleConnTimeout)
}

func TestLegacyPublishKeepsWholeFileLayout(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	dir := stageSet(t, stagedFiles(), nil)
	staged, err := os.ReadFile(filepath.Join(dir, publishManifestName))
	require.NoError(t, err)

	result, err := newTestClient(t, server, TransferConfig{}).PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)

	// The staged manifest is published byte for byte, under its own digest,
	// after the whole-file artifacts and SHA256SUMS.
	require.Equal(t, artifacts.SHA256Of(staged), result.ArtifactDigest)
	base := "publish/" + artifacts.Digest16(staged)
	require.Equal(t, []string{
		base + "/rootfs.ext4", base + "/" + manifestVMStateName, base + "/" + manifestMemoryName,
		base + "/" + artifacts.SHA256SUMSName, base + "/" + publishManifestName,
	}, store.putKeys())
	require.Equal(t, staged, store.object(base+"/"+publishManifestName))
	after, err := os.ReadFile(filepath.Join(dir, publishManifestName))
	require.NoError(t, err)
	require.Equal(t, staged, after, "a legacy publish leaves the staged manifest untouched")

	// No verified markers are written in legacy mode.
	for _, name := range publishArtifacts {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.False(t, markerMatches(path, info, ""), "%s carries a marker", name)
		require.False(t, hasMarker(path), "%s carries a marker", name)
	}
}

func TestLegacyPullWritesNoMarkers(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	result, err := newTestClient(t, server, TransferConfig{}).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, stagedFiles(), nil))
	require.NoError(t, err)

	stateRoot := t.TempDir()
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	require.NoError(t, newTestClient(t, server, TransferConfig{}).
		PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
	for _, name := range []string{nativeRootfsCacheName, manifestVMStateName, manifestMemoryName} {
		require.False(t, hasMarker(filepath.Join(imageDir(stateRoot, reference), name)), "%s carries a marker", name)
	}
}

func TestLegacyCacheCheckRehashesDespiteMarker(t *testing.T) {
	if currentBootID() == "" {
		t.Skip("no boot id: the marker is disabled on this platform")
	}
	path := filepath.Join(t.TempDir(), "artifact")
	payload := []byte("verified artifact")
	require.NoError(t, os.WriteFile(path, payload, 0o640))
	file := nativeFile{sha256: sha256Hex(payload), sizeBytes: int64(len(payload))}
	markVerified(path, file.sha256)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if !markerMatches(path, info, file.sha256) {
		t.Skip("filesystem without user xattrs")
	}

	// Same size, same mtime, different content: only a full re-hash can
	// tell. Chunked mode trusts the marker (the documented trade-off for
	// immutable cache files); legacy re-hashes on every check as before.
	handle, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = handle.WriteAt([]byte("V"), 0)
	require.NoError(t, err)
	require.NoError(t, handle.Close())
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))

	match, err := verifyCacheFile(path, file, false)
	require.NoError(t, err)
	require.False(t, match, "legacy mode must re-hash")
	match, err = verifyCacheFile(path, file, true)
	require.NoError(t, err)
	require.True(t, match, "chunked mode trusts the marker")
}

func TestChunkedPublishCarriesStateBundles(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	defer server.Close()
	files := stagedFiles()
	files[publishPolicyName] = []byte(`{"egress":"policy"}`)

	result, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, files, nil))
	require.NoError(t, err)
	base := strings.TrimSuffix(strings.TrimPrefix(result.ManifestRef, "s3://bucket/publish/"), "/manifest.json")
	require.Equal(t, files[publishPolicyName], store.object("publish/"+base+"/"+publishPolicyName),
		"the state bundle is uploaded whole next to the chunk index")

	// Both modes restore the chunked set, bundle included.
	for _, transfer := range []TransferConfig{{}, chunkedTransfer(TransferCompressionNone)} {
		stateRoot := t.TempDir()
		reference := artifacts.CheckpointReference(result.ArtifactDigest)
		require.NoError(t, newTestClient(t, server, transfer).
			PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
		got, err := os.ReadFile(filepath.Join(imageDir(stateRoot, reference), publishPolicyName))
		require.NoError(t, err)
		require.True(t, bytes.Equal(files[publishPolicyName], got))
	}
}

func TestChunkHeadForbiddenFallsBackToUpload(t *testing.T) {
	store := newFakeObjectStore()
	// AWS answers HEAD on a missing key with 403 when the credential has
	// no s3:ListBucket.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		store.handle(w, r)
	}))
	defer server.Close()
	files := stagedFiles()

	result, err := newTestClient(t, server, chunkedTransfer(TransferCompressionNone)).
		PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", stageSet(t, files, nil))
	require.NoError(t, err)
	stateRoot := t.TempDir()
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	require.NoError(t, newTestClient(t, server, TransferConfig{}).
		PullCheckpoint(context.Background(), stateRoot, reference, result.ManifestRef, result.ArtifactDigest))
	got, err := os.ReadFile(filepath.Join(imageDir(stateRoot, reference), nativeRootfsCacheName))
	require.NoError(t, err)
	require.True(t, bytes.Equal(files["rootfs.ext4"], got))
}

// hasMarker reports whether path carries a verified-cache marker at all.
func hasMarker(path string) bool {
	_, err := getXattr(path, verifiedMarkerAttr)
	return err == nil
}
