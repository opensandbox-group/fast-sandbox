package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

// TestStageFileFailureCleanup 验证摘要不匹配、长度不匹配及 HTTP 响应中断时，
// 下载返回错误，且正式缓存和临时文件都不会残留。
func TestStageFileFailureCleanup(t *testing.T) {
	content := make([]byte, 128<<10)
	copy(content, "rootfs-header")
	cases := []struct {
		name        string
		size        int64
		digest      string
		interrupted bool
		wantError   string
	}{
		{"digest-mismatch", int64(len(content)), sha256Hex([]byte("different-rootfs")), false, "digest mismatch"},
		{"size-mismatch", int64(len(content) + 1), sha256Hex(content), false, "size mismatch"},
		{"interrupted", int64(len(content)), sha256Hex(content), true, "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				if tc.interrupted {
					_, _ = w.Write(content[:len(content)/2])
					return
				}
				_, _ = w.Write(content)
			}))
			defer server.Close()
			client := &Client{s3: &s3Client{endpoint: server.URL, bucket: testBucket, region: "huatuo-region", http: server.Client()}}
			dir := t.TempDir()
			file := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sizeBytes: tc.size, sha256: tc.digest}
			err := stageFile(context.Background(), client, dir, file.publish, file)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("stageFile error = %v, want %q", err, tc.wantError)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Errorf("read cache directory: %v", err)
				return
			}
			if len(entries) != 0 {
				t.Errorf("failed download left %d files", len(entries))
			}
		})
	}
}

// TestCopySparse 验证短读取不会破坏分块与摘要，尾部数据不会被截断；
// 同时覆盖随数据返回 EOF、读取中断、定位/写入失败和空文件截断失败。
func TestCopySparse(t *testing.T) {
	content := make([]byte, (1<<20)+23)
	copy(content, "rootfs-header")
	copy(content[1<<19:], "rootfs-middle")
	copy(content[len(content)-7:], "trailer")
	cases := []struct {
		name    string
		reader  io.Reader
		closed  bool
		wantErr error
	}{
		{name: "short-reads", reader: iotest.HalfReader(bytes.NewReader(content))},
		{name: "data-with-eof", reader: iotest.DataErrReader(bytes.NewReader(content))},
		{name: "read-error", reader: io.MultiReader(bytes.NewReader(content), iotest.ErrReader(io.ErrClosedPipe)), wantErr: io.ErrClosedPipe},
		{name: "unexpected-eof", reader: io.MultiReader(bytes.NewReader(content), iotest.ErrReader(io.ErrUnexpectedEOF)), wantErr: io.ErrUnexpectedEOF},
		{name: "seek-error", reader: bytes.NewReader(make([]byte, 32<<10)), closed: true, wantErr: os.ErrClosed},
		{name: "write-error", reader: bytes.NewReader(content), closed: true, wantErr: os.ErrClosed},
		{name: "truncate-error", reader: bytes.NewReader(nil), closed: true, wantErr: os.ErrClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "rootfs-*")
			if err != nil {
				t.Errorf("create output: %v", err)
				return
			}
			defer output.Close()
			if tc.closed {
				if err := output.Close(); err != nil {
					t.Errorf("close output: %v", err)
					return
				}
			}
			digest := sha256.New()
			written, err := copySparse(output, io.TeeReader(tc.reader, digest))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("copy error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil || err != nil {
				return
			}
			if written != int64(len(content)) {
				t.Errorf("copied %d bytes, want %d", written, len(content))
			}
			if hex.EncodeToString(digest.Sum(nil)) != sha256Hex(content) {
				t.Error("input digest changed")
			}
			actual, err := os.ReadFile(output.Name())
			if err != nil {
				t.Errorf("read output: %v", err)
				return
			}
			if !bytes.Equal(actual, content) {
				t.Error("output differs from input")
			}
		})
	}
}

// TestFileMatchesSizeFastPath verifies the stat-first size check: a file
// with the wrong size is rejected without reading (the fast path), a file
// with the right size but wrong content is rejected by the digest, and an
// exact match passes.
func TestFileMatchesSizeFastPath(t *testing.T) {
	content := bytes.Repeat([]byte{0xab}, 1<<20)
	file := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(content), sizeBytes: int64(len(content))}
	path := filepath.Join(t.TempDir(), "rootfs.img")

	// Missing file.
	match, err := fileMatches(path, file)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.False(t, match)

	// Wrong size: rejected by stat, no content read needed.
	require.NoError(t, os.WriteFile(path, []byte("tiny"), cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match)

	// Right size, wrong content: caught by the digest.
	wrong := bytes.Repeat([]byte{0x00}, len(content))
	require.NoError(t, os.WriteFile(path, wrong, cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match)

	// Exact match.
	require.NoError(t, os.WriteFile(path, content, cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
}

// TestCacheComplete checks the committed-pull predicate: any missing,
// resized, or corrupted file (or a missing manifest) invalidates the cache.
func TestCacheComplete(t *testing.T) {
	artifacts := map[string][]byte{
		"rootfs.ext4":  bytes.Repeat([]byte{0xab}, 1<<16),
		"vmstate.snap": []byte("vmstate"),
		"memory.snap":  []byte("memory"),
	}
	manifestPayload := testManifest(artifacts)
	document, err := parseManifest(manifestPayload)
	require.NoError(t, err)
	files, err := document.nativeFiles()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))

	// No manifest: not complete.
	complete, err := cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// Manifest but no files: not complete.
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), manifestPayload, cacheFileMode))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// A file with the wrong size: not complete (fast path).
	for _, file := range files[:1] {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.cache), []byte("tiny"), cacheFileMode))
	}
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// All files exact: complete.
	for _, file := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.cache), artifacts[file.publish], cacheFileMode))
	}
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.True(t, complete)

	// Any single corrupt file breaks the commit.
	require.NoError(t, os.WriteFile(filepath.Join(dir, files[1].cache), []byte("tampered"), cacheFileMode))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)
}
