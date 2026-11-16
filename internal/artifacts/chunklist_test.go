package artifacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// chunksOf indexes payload like the agent's publish does: chunkSize-aligned,
// all-zero chunks omitted.
func chunksOf(payload []byte, chunkSize int64) []Chunk {
	var chunks []Chunk
	for offset := int64(0); offset < int64(len(payload)); offset += chunkSize {
		block := payload[offset:min(offset+chunkSize, int64(len(payload)))]
		if IsZero(block) {
			continue
		}
		sum := sha256.Sum256(block)
		chunks = append(chunks, Chunk{Offset: offset, Length: int64(len(block)), SHA256: hex.EncodeToString(sum[:]), StoredSize: int64(len(block))})
	}
	return chunks
}

func TestChunkListDigestFromIndexMatchesFile(t *testing.T) {
	const chunkSize = 64 << 10
	shapes := map[string][]byte{
		"empty":        {},
		"all zero":     make([]byte, 5*chunkSize+17),
		"dense":        bytes.Repeat([]byte{0x5a}, 3*chunkSize),
		"partial tail": append(bytes.Repeat([]byte{1}, chunkSize), 2, 3, 4),
		"sparse":       append(append(make([]byte, 2*chunkSize), bytes.Repeat([]byte{9}, 100)...), make([]byte, 3*chunkSize)...),
	}
	for name, payload := range shapes {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			fromIndex := ChunkListSHA256(chunkSize, int64(len(payload)), chunksOf(payload, chunkSize))
			fromFile, err := FileChunkListSHA256(path, chunkSize, 4)
			if err != nil {
				t.Fatal(err)
			}
			if fromIndex != fromFile {
				t.Fatalf("index %s != file %s", fromIndex, fromFile)
			}
		})
	}
}

// A hole and explicit zero bytes are the same content and the same digest.
func TestChunkListDigestHolesEqualZeros(t *testing.T) {
	const chunkSize = 64 << 10
	dir := t.TempDir()
	dense := filepath.Join(dir, "dense")
	payload := append(make([]byte, 3*chunkSize), 7)
	if err := os.WriteFile(dense, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(dir, "sparse")
	file, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{7}, 3*chunkSize); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	a, err := FileChunkListSHA256(dense, chunkSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FileChunkListSHA256(sparse, chunkSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("hole %s != zeros %s", b, a)
	}
}

func TestChunkListDigestBindsEverything(t *testing.T) {
	const chunkSize = 64 << 10
	payload := append(bytes.Repeat([]byte{1}, chunkSize), bytes.Repeat([]byte{2}, chunkSize)...)
	base := ChunkListSHA256(chunkSize, int64(len(payload)), chunksOf(payload, chunkSize))
	changed := append([]byte(nil), payload...)
	changed[len(changed)-1] = 3
	reversed := chunksOf(payload, chunkSize)
	reversed[0], reversed[1] = reversed[1], reversed[0]
	for name, other := range map[string]string{
		"content":      ChunkListSHA256(chunkSize, int64(len(changed)), chunksOf(changed, chunkSize)),
		"file size":    ChunkListSHA256(chunkSize, int64(len(payload))+chunkSize, chunksOf(payload, chunkSize)),
		"chunk size":   ChunkListSHA256(2*chunkSize, int64(len(payload)), chunksOf(payload, chunkSize)),
		"zero chunk":   ChunkListSHA256(chunkSize, int64(len(payload)), chunksOf(payload, chunkSize)[:1]),
		"plain sha256": func() string { s := sha256.Sum256(payload); return hex.EncodeToString(s[:]) }(),
	} {
		if other == base {
			t.Fatalf("%s change kept the digest", name)
		}
	}
	if ChunkListSHA256(chunkSize, int64(len(payload)), reversed) != base {
		t.Fatal("chunk order in the index must not matter")
	}
	if err := (ChunkListDigest{Version: 1, ChunkSize: chunkSize, SHA256: base}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ChunkListDigest{{Version: 2, ChunkSize: chunkSize, SHA256: base}, {Version: 1, SHA256: base}, {Version: 1, ChunkSize: chunkSize, SHA256: "xyz"}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
