//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// TestStageFileSparse 验证下载后内容、长度和摘要不变，且全零区域不全量占盘；
// 覆盖空文件、全零文件、非零文件、尾部不足一块的零数据及零/非零交替数据。
func TestStageFileSparse(t *testing.T) {
	const size = 2 << 20
	trailing := make([]byte, size+17)
	copy(trailing, "rootfs-header")
	mixed := make([]byte, size)
	copy(mixed[1<<18:], bytes.Repeat([]byte("rootfs-data"), 4096))
	copy(mixed[1<<20:], bytes.Repeat([]byte("snapshot-data"), 4096))
	cases := []struct {
		name    string
		content []byte
		sparse  bool
	}{
		{"empty", nil, false},
		{"all-zero", make([]byte, size), true},
		{"nonzero", bytes.Repeat([]byte("rootfs-data"), 8192), false},
		{"trailing-zero", trailing, true},
		{"alternating", mixed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{"rootfs.ext4": tc.content}}
			client := &Client{s3: testS3Client(t, store)}
			dir := t.TempDir()
			file := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(tc.content), sizeBytes: int64(len(tc.content))}
			if err := stageFile(context.Background(), client, dir, file.publish, file); err != nil {
				t.Errorf("stageFile: %v", err)
				return
			}
			target := filepath.Join(dir, file.cache)
			content, err := os.ReadFile(target)
			if err != nil {
				t.Errorf("read cache: %v", err)
				return
			}
			if !bytes.Equal(content, tc.content) {
				t.Error("cached bytes differ from input")
			}
			match, err := fileMatches(target, file)
			if err != nil || !match {
				t.Errorf("size/digest verification: match=%v err=%v", match, err)
			}
			// APFS may allocate seek/truncate gaps after a write; the runtime
			// requires Linux, where allocation is checked independently of bytes.
			if tc.sparse && runtime.GOOS == "linux" {
				info, err := os.Stat(target)
				if err != nil {
					t.Errorf("stat cache: %v", err)
					return
				}
				allocated := info.Sys().(*syscall.Stat_t).Blocks * 512
				if allocated >= int64(len(tc.content))/2 {
					t.Errorf("zero regions allocated: got %d bytes for %d logical bytes", allocated, len(tc.content))
				}
			}
		})
	}
}
