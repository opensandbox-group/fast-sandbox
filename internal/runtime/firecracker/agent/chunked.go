package agent

// chunked.go implements the chunked artifact-set transfer (see
// internal/artifacts/chunks.go for the format): publish splits each staged
// artifact into fixed-size chunks, drops all-zero chunks, and uploads the
// rest in parallel to content-addressed keys, skipping chunks the store
// already holds; pull fetches the listed chunks in parallel into a sparse
// file. A 3.5 GiB snapshot set typically carries ~240 MiB of non-zero data,
// so the bytes on the wire — not request parallelism — are what this saves.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
)

const (
	// TransferFormatLegacy publishes whole files (the original layout).
	TransferFormatLegacy = "legacy"
	// TransferFormatChunked publishes chunked, zero-elided sets.
	TransferFormatChunked = "chunked"

	// TransferCompressionNone stores chunks raw.
	TransferCompressionNone = "none"
	// TransferCompressionZstd stores chunks zstd-compressed.
	TransferCompressionZstd = "zstd"

	// defaultTransferConcurrency: with zero chunks elided a set is a few
	// hundred MiB at most, so 8 streams already hide per-request latency;
	// more only adds loss behind a rate-policed link.
	defaultTransferConcurrency = 8
	// maxChunkIndexBytes caps the downloaded chunk index (a 64 GiB fully
	// dense file is ~16k entries, well under this).
	maxChunkIndexBytes = 32 << 20
)

// TransferConfig is the agent's artifact transfer mode. Format is the one
// install-time switch: legacy (the default) keeps every publish, pull and
// cache path byte-for-byte on the original whole-file behaviour; chunked
// turns on the chunked publish together with the cache and connection
// optimizations that go with it (the verified-cache marker, sparse and
// fsynced cache writes, the pooled store transport, single-flighted cache
// ensures). Pulls understand chunked sets in either mode, so switching back
// to legacy keeps every snapshot already taken restorable.
type TransferConfig struct {
	Format      string
	Compression string
	Concurrency int
	ChunkSize   int64
}

func (t TransferConfig) normalized() TransferConfig {
	if t.Format == "" {
		t.Format = TransferFormatLegacy
	}
	if t.Compression == "" {
		t.Compression = TransferCompressionNone
	}
	if t.Concurrency <= 0 {
		t.Concurrency = defaultTransferConcurrency
	}
	if t.ChunkSize <= 0 {
		t.ChunkSize = artifacts.DefaultChunkSize
	}
	return t
}

// Chunked reports whether the chunked transfer mode is on.
func (t TransferConfig) Chunked() bool {
	return t.normalized().Format == TransferFormatChunked
}

// Validate rejects unknown modes (a typo must not silently fall back).
func (t TransferConfig) Validate() error {
	t = t.normalized()
	switch t.Format {
	case TransferFormatLegacy, TransferFormatChunked:
	default:
		return fmt.Errorf("unknown transfer format %q (want %s or %s)", t.Format, TransferFormatLegacy, TransferFormatChunked)
	}
	switch t.Compression {
	case TransferCompressionNone, TransferCompressionZstd:
	default:
		return fmt.Errorf("unknown transfer compression %q (want %s or %s)", t.Compression, TransferCompressionNone, TransferCompressionZstd)
	}
	if t.Format == TransferFormatLegacy && t.Compression != TransferCompressionNone {
		return fmt.Errorf("transfer compression %q requires format %s", t.Compression, TransferFormatChunked)
	}
	if t.ChunkSize%(64<<10) != 0 {
		return fmt.Errorf("transfer chunk size %d must be a multiple of 64KiB", t.ChunkSize)
	}
	return nil
}

// WithTransfer sets the transfer configuration of the client.
func WithTransfer(transfer TransferConfig) Option {
	return func(config *optionsConfig) { config.transfer = transfer }
}

var (
	zstdEncoderOnce sync.Once
	zstdEncoder     *zstd.Encoder
	zstdDecoderOnce sync.Once
	zstdDecoder     *zstd.Decoder
)

// chunkEncoder returns the shared encoder (EncodeAll is safe for
// concurrent use).
func chunkEncoder() *zstd.Encoder {
	zstdEncoderOnce.Do(func() {
		zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	})
	return zstdEncoder
}

// chunkDecoder returns the shared decoder (DecodeAll is safe for
// concurrent use).
func chunkDecoder() *zstd.Decoder {
	zstdDecoderOnce.Do(func() {
		zstdDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
	return zstdDecoder
}

// chunkJob is one chunk of one file.
type chunkJob struct {
	file   string
	offset int64
	length int64
}

// runChunkJobs runs fn over jobs on n workers and returns the first error;
// remaining jobs are skipped once one fails.
func runChunkJobs(ctx context.Context, n int, jobs []chunkJob, fn func(context.Context, chunkJob) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		next     atomic.Int64
		once     sync.Once
		firstErr error
		wg       sync.WaitGroup
	)
	for range min(n, len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) || ctx.Err() != nil {
					return
				}
				if err := fn(ctx, jobs[i]); err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// publishStats summarizes one chunked publish for the log line.
type publishStats struct {
	logicalBytes  int64
	zeroChunks    int64
	dataChunks    int64
	reusedChunks  int64
	uploadedBytes int64
}

// publishChunks uploads the non-zero chunks of the staged artifacts and
// returns the chunk index describing them. Chunk objects are content
// addressed and immutable, so an existing key (HEAD 200, or uploaded by this
// client before) is reused without re-uploading.
func (c *Client) publishChunks(ctx context.Context, dir string, names []string) (artifacts.ChunkIndex, publishStats, error) {
	transfer := c.transfer.normalized()
	index := artifacts.ChunkIndex{Version: artifacts.ChunkIndexVersion, ChunkSize: transfer.ChunkSize, Files: map[string]artifacts.ChunkIndexFile{}}
	stats := publishStats{}
	handles := map[string]*os.File{}
	defer func() {
		for _, handle := range handles {
			_ = handle.Close()
		}
	}()
	var jobs []chunkJob
	for _, name := range names {
		handle, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return index, stats, err
		}
		handles[name] = handle
		info, err := handle.Stat()
		if err != nil {
			return index, stats, err
		}
		index.Files[name] = artifacts.ChunkIndexFile{SizeBytes: info.Size(), Chunks: []artifacts.Chunk{}}
		stats.logicalBytes += info.Size()
		// Chunks entirely inside a hole are zero without reading them; the
		// rest are read and zero-checked (allocated zeros are common too).
		offsets, ok := dataChunkOffsets(handle, info.Size(), transfer.ChunkSize)
		if !ok {
			offsets = offsets[:0]
			for offset := int64(0); offset < info.Size(); offset += transfer.ChunkSize {
				offsets = append(offsets, offset)
			}
		}
		total := (info.Size() + transfer.ChunkSize - 1) / transfer.ChunkSize
		stats.zeroChunks += total - int64(len(offsets))
		for _, offset := range offsets {
			jobs = append(jobs, chunkJob{file: name, offset: offset, length: min(transfer.ChunkSize, info.Size()-offset)})
		}
	}

	encoding := artifacts.ChunkEncodingRaw
	if transfer.Compression == TransferCompressionZstd {
		encoding = artifacts.ChunkEncodingZstd
	}
	buffers := sync.Pool{New: func() any { buffer := make([]byte, transfer.ChunkSize); return &buffer }}
	var mu sync.Mutex
	err := runChunkJobs(ctx, transfer.Concurrency, jobs, func(ctx context.Context, job chunkJob) error {
		bufferRef := buffers.Get().(*[]byte)
		defer buffers.Put(bufferRef)
		block := (*bufferRef)[:job.length]
		if _, err := handles[job.file].ReadAt(block, job.offset); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read %s at %d: %w", job.file, job.offset, err)
		}
		if artifacts.IsZero(block) {
			atomic.AddInt64(&stats.zeroChunks, 1)
			return nil
		}
		digest := sha256.Sum256(block)
		sum := hex.EncodeToString(digest[:])
		storedSize, reused, err := c.storeChunk(ctx, sum, encoding, block)
		if err != nil {
			return err
		}
		atomic.AddInt64(&stats.dataChunks, 1)
		if reused {
			atomic.AddInt64(&stats.reusedChunks, 1)
		} else {
			atomic.AddInt64(&stats.uploadedBytes, storedSize)
		}
		mu.Lock()
		file := index.Files[job.file]
		file.Chunks = append(file.Chunks, artifacts.Chunk{
			Offset: job.offset, Length: job.length, SHA256: sum, Encoding: encoding, StoredSize: storedSize,
		})
		index.Files[job.file] = file
		mu.Unlock()
		return nil
	})
	return index, stats, err
}

// storeChunk makes the chunk object present in the store and returns its
// stored size and whether an existing object was reused.
func (c *Client) storeChunk(ctx context.Context, sum, encoding string, block []byte) (int64, bool, error) {
	key := artifacts.ChunkObjectKey(sum, encoding)
	if size, ok := c.knownChunks.Load(key); ok {
		return size.(int64), true, nil
	}
	// Identical chunks inside one set (or concurrent publishes) race to the
	// same key: one worker uploads, the others share its outcome.
	type stored struct {
		size   int64
		reused bool
	}
	outcome, err, shared := c.chunkFlight.Do(key, func() (any, error) {
		size, reused, err := c.storeChunkOnce(ctx, key, encoding, block)
		return stored{size: size, reused: reused}, err
	})
	if err != nil {
		return 0, false, err
	}
	result := outcome.(stored)
	return result.size, result.reused || shared, nil
}

func (c *Client) storeChunkOnce(ctx context.Context, key, encoding string, block []byte) (int64, bool, error) {
	if size, ok := c.knownChunks.Load(key); ok {
		return size.(int64), true, nil
	}
	type head struct {
		size int64
		ok   bool
	}
	found, err := withChunkAttempts(ctx, "HEAD "+key, func(ctx context.Context) (head, error) {
		size, ok, err := c.s3.size(ctx, key)
		return head{size: size, ok: ok}, err
	})
	if err != nil {
		return 0, false, fmt.Errorf("check chunk %s: %w", key, err)
	}
	if size, ok := found.size, found.ok; ok {
		c.knownChunks.Store(key, size)
		return size, true, nil
	}
	payload := block
	if encoding == artifacts.ChunkEncodingZstd {
		payload = chunkEncoder().EncodeAll(block, make([]byte, 0, len(block)/2))
	}
	if err := c.s3.putBytes(ctx, key, payload); err != nil {
		return 0, false, fmt.Errorf("upload chunk %s: %w", key, err)
	}
	c.knownChunks.Store(key, int64(len(payload)))
	return int64(len(payload)), false, nil
}

// bindChunkIndex rewrites the staged manifest so it binds the chunk index
// (or, with ref == nil, drops a binding inherited from a source manifest:
// assembleSnapshotManifest copies every field of the image the sandbox
// booted from). The rewrite is atomic and deterministic; an unchanged
// document keeps its exact bytes. It returns the manifest bytes to publish.
func bindChunkIndex(dir string, ref *artifacts.ChunkIndexRef) ([]byte, error) {
	manifestPath := filepath.Join(dir, publishManifestName)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read staged manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	document := map[string]any{}
	if err := decoder.Decode(&document); err != nil {
		if ref == nil {
			// Nothing to strip from a document that is not a JSON object;
			// a whole-file set publishes the staged bytes as before.
			return original, nil
		}
		return nil, fmt.Errorf("decode staged manifest: %w", err)
	}
	_, had := document[artifacts.ManifestChunksField]
	if ref == nil && !had {
		return original, nil
	}
	delete(document, artifacts.ManifestChunksField)
	if ref != nil {
		document[artifacts.ManifestChunksField] = ref
	}
	// Same layout as artifacts.MarshalVMManifest (two-space indent and a
	// trailing newline); the generic map keeps every unknown field.
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	tmp := manifestPath + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil { //nolint:gosec // published manifest, world-readable like the original
		return nil, err
	}
	if err := os.Rename(tmp, manifestPath); err != nil {
		return nil, err
	}
	return payload, nil
}

// materializeChunked pulls a chunked set into dir: the chunk index
// (verified against the manifest binding), then every listed chunk in
// parallel into sparse temporary files, each chunk verified against its
// digest. The whole-file digests in the manifest are recorded as the
// verified markers: they are implied by the verified chain.
func (c *Client) materializeChunked(ctx context.Context, dir, buildDir string, ref artifacts.ChunkIndexRef, files []nativeFile) error {
	// Version 1 sets carry their index under one fixed name in the set's
	// namespace; anything else would address an object outside it.
	if ref.Ref != artifacts.ChunkIndexName {
		return fmt.Errorf("unsupported chunk index ref %q (want %s)", ref.Ref, artifacts.ChunkIndexName)
	}
	index, err := c.fetchChunkIndex(ctx, buildDir+"/"+ref.Ref, ref)
	if err != nil {
		return err
	}
	type target struct {
		file nativeFile
		tmp  *os.File
	}
	targets := map[string]*target{}
	defer func() {
		for _, entry := range targets {
			_ = entry.tmp.Close()
			_ = os.Remove(entry.tmp.Name())
		}
	}()
	var jobs []chunkJob
	var whole []nativeFile
	chunks := map[chunkJob]artifacts.Chunk{}
	for _, file := range files {
		entry, ok := index.Files[file.publish]
		if !ok {
			// Only the native artifacts are chunked; the optional state
			// bundles (vault, policy) are small whole-file objects.
			if !isChunkedArtifact(file.publish) {
				whole = append(whole, file)
				continue
			}
			return fmt.Errorf("chunk index is missing %s", file.publish)
		}
		if entry.SizeBytes != file.sizeBytes {
			return fmt.Errorf("chunk index %s size %d does not match the manifest size %d", file.publish, entry.SizeBytes, file.sizeBytes)
		}
		if match, err := verifyCacheFile(filepath.Join(dir, file.cache), file, c.transfer.Chunked()); err == nil && match {
			continue
		}
		tmp, err := os.CreateTemp(dir, file.cache+".tmp-*")
		if err != nil {
			return err
		}
		targets[file.publish] = &target{file: file, tmp: tmp}
		if err := tmp.Truncate(file.sizeBytes); err != nil {
			return err
		}
		for _, chunk := range entry.Chunks {
			job := chunkJob{file: file.publish, offset: chunk.Offset, length: chunk.Length}
			jobs = append(jobs, job)
			chunks[job] = chunk
		}
	}

	transfer := c.transfer.normalized()
	err = runChunkJobs(ctx, transfer.Concurrency, jobs, func(ctx context.Context, job chunkJob) error {
		chunk := chunks[job]
		block, err := c.fetchChunk(ctx, chunk)
		if err != nil {
			return err
		}
		if _, err := targets[job.file].tmp.WriteAt(block, chunk.Offset); err != nil {
			return fmt.Errorf("write %s at %d: %w", job.file, chunk.Offset, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for name, entry := range targets {
		if err := finishCacheFile(entry.tmp, filepath.Join(dir, entry.file.cache), entry.file.sha256, c.transfer.Chunked()); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		delete(targets, name)
	}
	return c.stageWholeFiles(ctx, dir, buildDir, whole)
}

// isChunkedArtifact reports whether a published name is one of the native
// artifacts a chunked set carries in its chunk index.
func isChunkedArtifact(name string) bool {
	for _, artifact := range publishArtifacts {
		if artifact == name {
			return true
		}
	}
	return false
}

// fetchChunkIndex downloads the chunk index and verifies it against the
// manifest binding.
func (c *Client) fetchChunkIndex(ctx context.Context, key string, ref artifacts.ChunkIndexRef) (artifacts.ChunkIndex, error) {
	body, err := c.s3.get(ctx, key)
	if err != nil {
		return artifacts.ChunkIndex{}, fmt.Errorf("fetch chunk index: %w", err)
	}
	defer body.Close()
	payload, err := io.ReadAll(io.LimitReader(body, maxChunkIndexBytes+1))
	if err != nil {
		return artifacts.ChunkIndex{}, fmt.Errorf("read chunk index: %w", err)
	}
	if len(payload) > maxChunkIndexBytes {
		return artifacts.ChunkIndex{}, fmt.Errorf("chunk index exceeds the %d-byte limit", maxChunkIndexBytes)
	}
	if int64(len(payload)) != ref.SizeBytes || sha256Hex(payload) != ref.SHA256 {
		return artifacts.ChunkIndex{}, fmt.Errorf("chunk index does not match the manifest binding")
	}
	return artifacts.ParseChunkIndex(payload)
}

// withChunkAttempts runs one chunk request with a per-attempt timeout,
// retrying transient failures (including a stalled connection, which the
// timeout drops so the retry dials a fresh one). A missing object is final.
func withChunkAttempts[T any](ctx context.Context, what string, attempt func(context.Context) (T, error)) (T, error) {
	var (
		zero    T
		lastErr error
	)
	for try := 0; try <= s3RetryAttempts; try++ {
		attemptCtx, cancel := context.WithTimeout(ctx, chunkAttemptTimeout)
		result, err := attempt(attemptCtx)
		cancel()
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if errors.Is(err, ErrObjectNotFound) {
			return zero, err
		}
		lastErr = err
		klog.InfoS("chunk request attempt failed, retrying", "request", what, "attempt", try+1, "err", err)
	}
	return zero, fmt.Errorf("%s: %w", what, lastErr)
}

// fetchChunk downloads, decodes and verifies one chunk.
func (c *Client) fetchChunk(ctx context.Context, chunk artifacts.Chunk) ([]byte, error) {
	key := artifacts.ChunkObjectKey(chunk.SHA256, chunk.Encoding)
	return withChunkAttempts(ctx, "GET "+key, func(ctx context.Context) ([]byte, error) {
		return c.fetchChunkOnce(ctx, key, chunk)
	})
}

func (c *Client) fetchChunkOnce(ctx context.Context, key string, chunk artifacts.Chunk) ([]byte, error) {
	body, err := c.getArtifact(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("fetch chunk %s: %w", key, err)
	}
	defer body.Close()
	stored, err := io.ReadAll(io.LimitReader(body, chunk.StoredSize+1))
	if err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", key, err)
	}
	if int64(len(stored)) != chunk.StoredSize {
		return nil, fmt.Errorf("chunk %s: got %d bytes, want %d", key, len(stored), chunk.StoredSize)
	}
	block := stored
	if chunk.Encoding == artifacts.ChunkEncodingZstd {
		block, err = chunkDecoder().DecodeAll(stored, make([]byte, 0, chunk.Length))
		if err != nil {
			return nil, fmt.Errorf("decode chunk %s: %w", key, err)
		}
	}
	if int64(len(block)) != chunk.Length {
		return nil, fmt.Errorf("chunk %s: decoded %d bytes, want %d", key, len(block), chunk.Length)
	}
	if digest := sha256.Sum256(block); hex.EncodeToString(digest[:]) != chunk.SHA256 {
		return nil, fmt.Errorf("chunk %s digest mismatch", key)
	}
	return block, nil
}

// logPublishStats reports what a chunked publish actually moved.
func logPublishStats(kind, key string, stats publishStats) {
	klog.InfoS("runtime-agent chunked publish stats", "kind", kind, "key", key,
		"logicalBytes", stats.logicalBytes, "zeroChunks", stats.zeroChunks, "dataChunks", stats.dataChunks,
		"reusedChunks", stats.reusedChunks, "uploadedBytes", stats.uploadedBytes)
}
