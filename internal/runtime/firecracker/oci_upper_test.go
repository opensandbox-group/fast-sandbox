package firecracker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"fast-sandbox/internal/ocitemplate"
)

func TestCaptureOCIUpperPreservesSparsePrivateChangesAndParent(t *testing.T) {
	root, staging := t.TempDir(), t.TempDir()
	home := filepath.Join(root, "oci/sandboxes/test")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "upper"), 0750))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "mnt"), 0750))
	data := filepath.Join(home, "upper/writable_data")
	index := filepath.Join(home, "upper/writable_index")
	file, err := os.Create(data)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(1<<30))
	_, err = file.WriteAt([]byte("changed block"), 128<<20)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.WriteFile(index, []byte("frozen mapping"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "mnt/manifest.json"), mustPreparedManifest(t), 0600))
	config := map[string]any{"repoBlobUrl": "https://r.example/v2/templates/base/blobs", "lowers": []map[string]any{{"digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "size": 123}}, "upper": map[string]string{"data": data, "index": index}}
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	path := filepath.Join(home, "rootfs.config.v1.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	state := map[string]any{"format_version": 2, "sandbox_id": "test", "rootfs_base_layers": 1}
	raw, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), raw, 0600))
	require.NoError(t, captureOCIUpper(filepath.Join(root, "oci"), "test", staging))
	upper, err := ocitemplate.ReadRootfsUpper(staging)
	require.NoError(t, err)
	require.Equal(t, "r.example/templates/base", upper.Repository)
	require.Equal(t, int64(1<<30), upper.VirtualSizeBytes)
	require.Len(t, upper.Lowers, 1)
	require.Equal(t, 1, upper.BaseLayers)
	// A failed upload can leave the original VM running without changing its
	// frozen capture: later guest writes cannot leak into that checkpoint.
	require.NoError(t, os.WriteFile(index, []byte("later mapping"), 0600))
	original, err := os.OpenFile(data, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = original.WriteAt([]byte("later writes!"), 128<<20)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	captured, err := os.Open(filepath.Join(staging, ocitemplate.UpperDataName))
	require.NoError(t, err)
	defer captured.Close()
	buf := make([]byte, len("changed block"))
	_, err = captured.ReadAt(buf, 128<<20)
	require.NoError(t, err)
	require.Equal(t, "changed block", string(buf))
	raw, err = os.ReadFile(filepath.Join(staging, ocitemplate.UpperIndexName))
	require.NoError(t, err)
	require.Equal(t, "frozen mapping", string(raw))
	_, err = os.Stat(filepath.Join(staging, publishedRootfsName))
	require.True(t, os.IsNotExist(err))
	// Invalid parent metadata is rejected before copying anything.
	config["lowers"] = []map[string]any{{"digest": "sha256:bad", "size": 123}}
	raw, err = json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.Error(t, captureOCIUpper(filepath.Join(root, "oci"), "test", t.TempDir()))
}

func TestCopyUpperSparseFallbackPreservesHolesAndTail(t *testing.T) {
	for _, allZero := range []bool{false, true} {
		t.Run(fmt.Sprintf("zero=%t", allZero), func(t *testing.T) {
			source, err := os.Create(filepath.Join(t.TempDir(), "source"))
			require.NoError(t, err)
			defer source.Close()
			length := int64(32<<20) + 123
			require.NoError(t, source.Truncate(length))
			if !allZero {
				for _, offset := range []int64{0, 4093, 10 << 20, length - 5} {
					_, err = source.WriteAt([]byte("proof"), offset)
					require.NoError(t, err)
				}
			}
			output, err := os.Create(filepath.Join(t.TempDir(), "copy"))
			require.NoError(t, err)
			defer output.Close()
			require.NoError(t, output.Truncate(length))
			require.NoError(t, copyUpperExtentsWithSeek(source, output, length, func(int, int64, int) (int64, error) { return 0, unix.EINVAL }))
			expected := sha256.New()
			actual := sha256.New()
			_, err = source.Seek(0, io.SeekStart)
			require.NoError(t, err)
			_, err = output.Seek(0, io.SeekStart)
			require.NoError(t, err)
			_, err = io.Copy(expected, source)
			require.NoError(t, err)
			_, err = io.Copy(actual, output)
			require.NoError(t, err)
			require.Equal(t, expected.Sum(nil), actual.Sum(nil))
			info, err := output.Stat()
			require.NoError(t, err)
			require.Equal(t, length, info.Size())
			require.Less(t, info.Sys().(*syscall.Stat_t).Blocks*512, int64(1<<20), "zero-filled holes must remain sparse")
		})
	}
}

func mustPreparedManifest(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(preparedMetadata(t))
	require.NoError(t, err)
	return raw
}
