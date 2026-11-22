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
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
)

// Concurrent workers that meet the same new chunk collapse into one upload.
// That upload must be counted once in uploadedBytes, and every other
// occurrence as reused: singleflight's shared flag is also true for the
// caller that ran the upload, which made a coalesced upload count as zero
// bytes.
func TestPublishChunksCountsCoalescedUploadOnce(t *testing.T) {
	store := newFakeObjectStore()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the first look at a chunk long enough for the other workers
		// to pile up on the same key.
		if r.Method == http.MethodHead && strings.Contains(r.URL.Path, artifacts.ChunkStorePrefix) {
			time.Sleep(100 * time.Millisecond)
		}
		store.handle(w, r)
	}))
	defer server.Close()

	const copies = 8
	block := bytes.Repeat([]byte{0x5a}, testChunkSize)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rootfs.ext4"), bytes.Repeat(block, copies), 0o640))

	transfer := chunkedTransfer(TransferCompressionNone)
	transfer.Concurrency = copies
	client := newTestClient(t, server, transfer)
	index, stats, err := client.publishChunks(context.Background(), dir, []string{"rootfs.ext4"})
	require.NoError(t, err)
	require.Len(t, index.Files["rootfs.ext4"].Chunks, copies)

	key := "publish/" + artifacts.ChunkObjectKey(sha256Hex(block), artifacts.ChunkEncodingRaw)
	require.NotEmpty(t, store.object(key))
	require.Equal(t, int64(copies), stats.dataChunks)
	require.Equal(t, int64(len(store.object(key))), stats.uploadedBytes, "the one upload must be counted")
	require.Equal(t, int64(copies-1), stats.reusedChunks)
}
