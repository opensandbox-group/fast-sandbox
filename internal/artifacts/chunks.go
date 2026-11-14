package artifacts

// Chunked artifact sets: a large artifact (rootfs, guest memory) is split
// into fixed-size chunks; all-zero chunks are omitted (they are holes in the
// reassembled file) and every other chunk is stored once under a
// content-addressed key, so identical chunks across snapshots of the same
// template are uploaded and downloaded once.
//
// The chunk index (chunks.json) lives next to the manifest in the per-build
// namespace. The manifest binds it through its "chunks" field (ChunkIndexRef:
// ref + sha256 + size), so the trust chain is
//
//	index artifactDigest -> manifest -> chunks.json -> per-chunk sha256
//
// and a consumer never needs a whole-file re-hash. Sets without the field are
// whole-file sets and keep their original layout.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// ChunkIndexName is the chunk index document of a chunked set.
	ChunkIndexName = "chunks.json"
	// ChunkIndexVersion is the only chunk index schema version.
	ChunkIndexVersion = 1
	// DefaultChunkSize is 4 MiB: the RGW stripe size and the DART block
	// size, small enough to skip most zero ranges of a guest memory dump.
	DefaultChunkSize = 4 << 20
	// ManifestChunksField is the manifest field that binds the chunk index.
	ManifestChunksField = "chunks"
	// ChunkStorePrefix is the store-relative prefix of content-addressed
	// chunk objects.
	ChunkStorePrefix = "chunks/sha256"

	// ChunkEncodingRaw stores the chunk bytes as-is.
	ChunkEncodingRaw = ""
	// ChunkEncodingZstd stores the chunk zstd-compressed.
	ChunkEncodingZstd = "zstd"
)

// ChunkIndex is the chunk index document (chunks.json).
type ChunkIndex struct {
	Version   int                       `json:"version"`
	ChunkSize int64                     `json:"chunkSize"`
	Files     map[string]ChunkIndexFile `json:"files"`
}

// ChunkIndexFile lists the non-zero chunks of one artifact. Offsets missing
// from Chunks are all-zero ranges.
type ChunkIndexFile struct {
	SizeBytes int64   `json:"sizeBytes"`
	Chunks    []Chunk `json:"chunks"`
}

// Chunk is one stored, non-zero chunk. SHA256 covers the raw (decoded) chunk
// bytes and names the object; Encoding and StoredSize describe the stored
// representation.
type Chunk struct {
	Offset     int64  `json:"off"`
	Length     int64  `json:"len"`
	SHA256     string `json:"sha256"`
	Encoding   string `json:"enc,omitempty"`
	StoredSize int64  `json:"storedSize"`
}

// ChunkIndexRef is the manifest's binding of the chunk index.
type ChunkIndexRef struct {
	Version   int    `json:"version"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// ChunkObjectKey returns the store-relative key of a chunk object.
func ChunkObjectKey(sum, encoding string) string {
	key := ChunkStorePrefix + "/" + sum[:2] + "/" + sum
	if encoding == ChunkEncodingZstd {
		key += ".zst"
	}
	return key
}

// MarshalChunkIndex serializes the index deterministically (chunks sorted
// by offset, map keys sorted by encoding/json), so a retried publish of the
// same staged set produces identical bytes and an identical manifest digest.
func MarshalChunkIndex(index ChunkIndex) ([]byte, error) {
	for name, file := range index.Files {
		sort.Slice(file.Chunks, func(i, j int) bool { return file.Chunks[i].Offset < file.Chunks[j].Offset })
		index.Files[name] = file
	}
	payload, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// ParseChunkIndex decodes and validates a chunk index: every chunk lies
// inside its file, is aligned to the chunk size, and carries a well-formed
// digest and encoding.
func ParseChunkIndex(payload []byte) (ChunkIndex, error) {
	var index ChunkIndex
	if err := json.Unmarshal(payload, &index); err != nil {
		return ChunkIndex{}, fmt.Errorf("decode chunk index: %w", err)
	}
	if index.Version != ChunkIndexVersion {
		return ChunkIndex{}, fmt.Errorf("unsupported chunk index version %d", index.Version)
	}
	if index.ChunkSize <= 0 {
		return ChunkIndex{}, fmt.Errorf("chunk index has invalid chunk size %d", index.ChunkSize)
	}
	for name, file := range index.Files {
		for _, chunk := range file.Chunks {
			if chunk.Offset < 0 || chunk.Offset%index.ChunkSize != 0 || chunk.Length <= 0 ||
				chunk.Length > index.ChunkSize || chunk.Offset+chunk.Length > file.SizeBytes {
				return ChunkIndex{}, fmt.Errorf("chunk index %s: chunk at %d+%d out of bounds", name, chunk.Offset, chunk.Length)
			}
			if !validHexDigest(chunk.SHA256) {
				return ChunkIndex{}, fmt.Errorf("chunk index %s: invalid chunk digest at %d", name, chunk.Offset)
			}
			if chunk.Encoding != ChunkEncodingRaw && chunk.Encoding != ChunkEncodingZstd {
				return ChunkIndex{}, fmt.Errorf("chunk index %s: unknown chunk encoding %q", name, chunk.Encoding)
			}
			if chunk.StoredSize <= 0 {
				return ChunkIndex{}, fmt.Errorf("chunk index %s: invalid stored size at %d", name, chunk.Offset)
			}
		}
	}
	return index, nil
}

// zeroBlock backs IsZero: bytes.Equal against it runs the SIMD memequal.
var zeroBlock = make([]byte, 1<<20)

// IsZero reports whether every byte of block is zero.
func IsZero(block []byte) bool {
	for len(block) > 0 {
		n := min(len(block), len(zeroBlock))
		if !bytes.Equal(block[:n], zeroBlock[:n]) {
			return false
		}
		block = block[n:]
	}
	return true
}

func validHexDigest(sum string) bool {
	if len(sum) != 64 {
		return false
	}
	for _, character := range sum {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
