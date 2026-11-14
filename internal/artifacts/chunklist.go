package artifacts

// Chunk-list digests: the identity of a chunked native artifact that needs no
// whole-file hash. In chunked transfer mode every byte of a large artifact
// (rootfs, guest memory) is already covered by the chain
//
//	artifactDigest -> manifest -> chunks.json -> per-chunk sha256
//
// so a whole-file SHA-256 adds no integrity, yet computing it on the pause
// path costs ~10 s for a 4 GiB memory dump on CPUs without SHA extensions.
// The chunk-list digest binds the same content: the chunk size, the file
// size and, in offset order, every non-zero chunk's offset, length and
// sha256. Omitted chunks are all-zero ranges, exactly as in the chunk index.
// It is computable from a verified chunks.json without reading data, and
// from a local file by hashing only its non-zero chunks, in parallel.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// ChunkListDigestVersion is the only chunk-list digest version.
const ChunkListDigestVersion = 1

// chunkListDomain separates chunk-list digests from every other sha256.
const chunkListDomain = "fsb-chunklist-v1\x00"

// ChunkListDigest is a manifest file entry's chunk-list identity.
type ChunkListDigest struct {
	Version   int    `json:"version"`
	ChunkSize int64  `json:"chunkSize"`
	SHA256    string `json:"sha256"`
}

// Validate rejects a malformed digest.
func (d ChunkListDigest) Validate() error {
	if d.Version != ChunkListDigestVersion {
		return fmt.Errorf("unsupported chunk-list digest version %d", d.Version)
	}
	if d.ChunkSize <= 0 {
		return fmt.Errorf("chunk-list digest has invalid chunk size %d", d.ChunkSize)
	}
	if !validHexDigest(d.SHA256) {
		return fmt.Errorf("chunk-list digest has an invalid sha256")
	}
	return nil
}

// ChunkListSHA256 returns the chunk-list digest of a file of size bytes whose
// non-zero chunks (chunkSize-aligned, any order) are chunks.
func ChunkListSHA256(chunkSize, size int64, chunks []Chunk) string {
	sorted := append([]Chunk(nil), chunks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	digest := sha256.New()
	_, _ = io.WriteString(digest, chunkListDomain)
	var word [8]byte
	put := func(value int64) {
		binary.BigEndian.PutUint64(word[:], uint64(value)) //nolint:gosec // sizes and offsets are non-negative
		_, _ = digest.Write(word[:])
	}
	put(chunkSize)
	put(size)
	for _, chunk := range sorted {
		put(chunk.Offset)
		put(chunk.Length)
		sum, _ := hex.DecodeString(chunk.SHA256)
		_, _ = digest.Write(sum)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// FileChunkListSHA256 computes the chunk-list digest of a local file: it
// reads the file in chunkSize chunks with workers in parallel, skips
// all-zero chunks (holes read as zeros) and hashes the rest.
func FileChunkListSHA256(path string, chunkSize int64, workers int) (string, error) {
	if chunkSize <= 0 {
		return "", fmt.Errorf("invalid chunk size %d", chunkSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	count := (size + chunkSize - 1) / chunkSize
	workers = max(1, min(workers, int(min(count, 64))))
	type result struct {
		chunk Chunk
		data  bool
	}
	results := make([]result, count)
	offsets := make(chan int64)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			block := make([]byte, chunkSize)
			for offset := range offsets {
				length := min(chunkSize, size-offset)
				if _, err := file.ReadAt(block[:length], offset); err != nil && !errors.Is(err, io.EOF) {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("read %s at %d: %w", path, offset, err)
					}
					mu.Unlock()
					continue
				}
				if IsZero(block[:length]) {
					continue
				}
				sum := sha256.Sum256(block[:length])
				results[offset/chunkSize] = result{chunk: Chunk{Offset: offset, Length: length, SHA256: hex.EncodeToString(sum[:])}, data: true}
			}
		}()
	}
	for offset := int64(0); offset < size; offset += chunkSize {
		offsets <- offset
	}
	close(offsets)
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}
	chunks := make([]Chunk, 0, count)
	for _, entry := range results {
		if entry.data {
			chunks = append(chunks, entry.chunk)
		}
	}
	return ChunkListSHA256(chunkSize, size, chunks), nil
}
