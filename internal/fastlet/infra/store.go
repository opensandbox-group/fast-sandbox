package infra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const DefaultMaxArtifactBytes int64 = 512 << 20

var (
	ErrDigestMismatch    = errors.New("Infra artifact digest mismatch")
	ErrArtifactCorrupted = errors.New("Infra artifact cache entry is corrupted")
	ErrArtifactTooLarge  = errors.New("Infra artifact exceeds size limit")
)

type PreparedArtifact struct {
	Digest     string `json:"digest"`
	PodPath    string `json:"podPath"`
	HostPath   string `json:"hostPath"`
	Size       int64  `json:"size"`
	CacheHit   bool   `json:"cacheHit"`
	Executable bool   `json:"executable"`
}

// ArtifactStore is a Fastlet-local content-addressed store. PodRoot is where
// Fastlet writes; HostRoot is the equivalent path visible to host containerd.
type ArtifactStore struct {
	mu       sync.Mutex
	podRoot  string
	hostRoot string
	maxBytes int64
}

func NewArtifactStore(podRoot, hostRoot string) (*ArtifactStore, error) {
	if !filepath.IsAbs(podRoot) || !filepath.IsAbs(hostRoot) {
		return nil, errors.New("Infra artifact Pod and host roots must be absolute")
	}
	return &ArtifactStore{
		podRoot: filepath.Clean(podRoot), hostRoot: filepath.Clean(hostRoot), maxBytes: DefaultMaxArtifactBytes,
	}, nil
}

func (s *ArtifactStore) SetMaxBytes(maxBytes int64) {
	if maxBytes > 0 {
		s.maxBytes = maxBytes
	}
}

// Stage verifies an immutable expected digest before atomically publishing the
// artifact. A valid existing entry is reused without reading the source.
func (s *ArtifactStore) Stage(ctx context.Context, expectedDigest string, executable bool, open func() (io.ReadCloser, error)) (PreparedArtifact, error) {
	hexDigest, err := parseDigest(expectedDigest)
	if err != nil {
		return PreparedArtifact{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prepared, ok, err := s.lookupLocked(ctx, hexDigest, executable); ok || err != nil {
		return prepared, err
	}
	if open == nil {
		return PreparedArtifact{}, errors.New("Infra artifact source is required on cache miss")
	}
	reader, err := open()
	if err != nil {
		return PreparedArtifact{}, err
	}
	defer reader.Close()
	return s.writeLocked(ctx, reader, expectedDigest, executable)
}

// ImportTrusted stores a platform binary shipped in the Fastlet image. Its
// digest is calculated locally and the resulting content address is returned.
func (s *ArtifactStore) ImportTrusted(ctx context.Context, reader io.Reader, executable bool) (PreparedArtifact, error) {
	if reader == nil {
		return PreparedArtifact{}, errors.New("trusted artifact source is required")
	}
	if err := os.MkdirAll(s.podRoot, 0755); err != nil {
		return PreparedArtifact{}, err
	}
	temporary, err := os.CreateTemp(s.podRoot, ".trusted-*")
	if err != nil {
		return PreparedArtifact{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hasher := sha256.New()
	size, copyErr := copyBounded(ctx, io.MultiWriter(temporary, hasher), reader, s.maxBytes)
	closeErr := temporary.Close()
	if copyErr != nil {
		return PreparedArtifact{}, copyErr
	}
	if closeErr != nil {
		return PreparedArtifact{}, closeErr
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	file, err := os.Open(temporaryPath)
	if err != nil {
		return PreparedArtifact{}, err
	}
	defer file.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if prepared, ok, lookupErr := s.lookupLocked(ctx, hexDigest, executable); ok || lookupErr != nil {
		return prepared, lookupErr
	}
	prepared, err := s.writeLocked(ctx, io.LimitReader(file, size), digest, executable)
	return prepared, err
}

func (s *ArtifactStore) Lookup(ctx context.Context, digest string) (PreparedArtifact, bool, error) {
	return s.LookupMode(ctx, digest, false)
}

func (s *ArtifactStore) LookupMode(ctx context.Context, digest string, executable bool) (PreparedArtifact, bool, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return PreparedArtifact{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(ctx, hexDigest, executable)
}

func (s *ArtifactStore) lookupLocked(ctx context.Context, hexDigest string, executable bool) (PreparedArtifact, bool, error) {
	if err := ctx.Err(); err != nil {
		return PreparedArtifact{}, false, err
	}
	podPath, hostPath := s.paths(hexDigest, executable)
	info, err := os.Lstat(podPath)
	if errors.Is(err, os.ErrNotExist) {
		return PreparedArtifact{}, false, nil
	}
	if err != nil {
		return PreparedArtifact{}, false, err
	}
	if !info.Mode().IsRegular() {
		return PreparedArtifact{}, false, fmt.Errorf("%w: %s is not a regular file", ErrArtifactCorrupted, podPath)
	}
	file, err := os.Open(podPath)
	if err != nil {
		return PreparedArtifact{}, false, err
	}
	actual, _, hashErr := digestReader(ctx, file, s.maxBytes)
	closeErr := file.Close()
	if hashErr != nil {
		return PreparedArtifact{}, false, hashErr
	}
	if closeErr != nil {
		return PreparedArtifact{}, false, closeErr
	}
	if actual != "sha256:"+hexDigest {
		return PreparedArtifact{}, false, fmt.Errorf("%w: expected sha256:%s, got %s", ErrArtifactCorrupted, hexDigest, actual)
	}
	return PreparedArtifact{Digest: actual, PodPath: podPath, HostPath: hostPath, Size: info.Size(), CacheHit: true, Executable: executable}, true, nil
}

func (s *ArtifactStore) writeLocked(ctx context.Context, reader io.Reader, expectedDigest string, executable bool) (PreparedArtifact, error) {
	hexDigest, err := parseDigest(expectedDigest)
	if err != nil {
		return PreparedArtifact{}, err
	}
	podPath, hostPath := s.paths(hexDigest, executable)
	if err := os.MkdirAll(filepath.Dir(podPath), 0755); err != nil {
		return PreparedArtifact{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(podPath), ".partial-*")
	if err != nil {
		return PreparedArtifact{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hasher := sha256.New()
	size, copyErr := copyBounded(ctx, io.MultiWriter(temporary, hasher), reader, s.maxBytes)
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	if closeErr := temporary.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return PreparedArtifact{}, copyErr
	}
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != expectedDigest {
		return PreparedArtifact{}, fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, expectedDigest, actualDigest)
	}
	mode := os.FileMode(0444)
	if executable {
		mode = 0555
	}
	if err := os.Chmod(temporaryPath, mode); err != nil {
		return PreparedArtifact{}, err
	}
	if err := os.Rename(temporaryPath, podPath); err != nil {
		return PreparedArtifact{}, err
	}
	return PreparedArtifact{Digest: actualDigest, PodPath: podPath, HostPath: hostPath, Size: size, Executable: executable}, nil
}

func (s *ArtifactStore) paths(hexDigest string, executable bool) (string, string) {
	variant := "data"
	if executable {
		variant = "executable"
	}
	relative := filepath.Join("blobs", "sha256", hexDigest, variant)
	return filepath.Join(s.podRoot, relative), filepath.Join(s.hostRoot, relative)
}

func parseDigest(digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("invalid sha256 digest %q", digest)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("invalid sha256 digest %q: %w", digest, err)
	}
	return hexDigest, nil
}

func digestReader(ctx context.Context, reader io.Reader, maxBytes int64) (string, int64, error) {
	hasher := sha256.New()
	size, err := copyBounded(ctx, hasher, reader, maxBytes)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func copyBounded(ctx context.Context, writer io.Writer, reader io.Reader, maxBytes int64) (int64, error) {
	contextReader := &readerWithContext{ctx: ctx, reader: reader}
	limited := &io.LimitedReader{R: contextReader, N: maxBytes + 1}
	size, err := io.Copy(writer, limited)
	if err != nil {
		return size, err
	}
	if size > maxBytes {
		return size, ErrArtifactTooLarge
	}
	return size, nil
}

type readerWithContext struct {
	ctx    context.Context
	reader io.Reader
}

func (r *readerWithContext) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
