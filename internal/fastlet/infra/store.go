package infra

import (
	"archive/tar"
	"compress/gzip"
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

type PreparedSource struct {
	Digest   string `json:"digest"`
	PodRoot  string `json:"podRoot"`
	HostRoot string `json:"hostRoot"`
	CacheHit bool   `json:"cacheHit"`
}

type PreparedSourcePath struct {
	PodPath  string
	HostPath string
}

func (s PreparedSource) Resolve(sourcePath string) (PreparedSourcePath, error) {
	if !filepath.IsAbs(sourcePath) {
		return PreparedSourcePath{}, fmt.Errorf("source path %q must be absolute", sourcePath)
	}
	resolvedRoot, err := filepath.EvalSymlinks(s.PodRoot)
	if err != nil {
		return PreparedSourcePath{}, fmt.Errorf("resolve verified artifact root: %w", err)
	}
	podPath := filepath.Join(resolvedRoot, strings.TrimPrefix(filepath.Clean(sourcePath), string(filepath.Separator)))
	resolved, err := filepath.EvalSymlinks(podPath)
	if err != nil {
		return PreparedSourcePath{}, err
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return PreparedSourcePath{}, fmt.Errorf("source path %q escapes the verified artifact root", sourcePath)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return PreparedSourcePath{}, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return PreparedSourcePath{}, fmt.Errorf("source path %q is neither a regular file nor a directory", sourcePath)
	}
	relative, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return PreparedSourcePath{}, err
	}
	return PreparedSourcePath{
		PodPath:  resolved,
		HostPath: filepath.Join(s.HostRoot, relative),
	}, nil
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

// StageTree publishes a verified filesystem tree. Archive streams are
// gzip-compressed tar files whose complete byte stream must match digest. OCI
// streams are uncompressed rootfs tar streams; the resolver is responsible for
// resolving the digest-pinned image before providing them.
func (s *ArtifactStore) StageTree(
	ctx context.Context,
	digest string,
	gzipArchive bool,
	verifyStreamDigest bool,
	open func() (io.ReadCloser, error),
) (PreparedSource, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return PreparedSource{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if source, ok, err := s.lookupTreeLocked(ctx, hexDigest); ok || err != nil {
		return source, err
	}
	reader, err := open()
	if err != nil {
		return PreparedSource{}, err
	}
	defer reader.Close()

	base := filepath.Join(s.podRoot, "trees", "sha256", hexDigest)
	if err := os.MkdirAll(filepath.Dir(base), 0755); err != nil {
		return PreparedSource{}, err
	}
	temporary, err := os.MkdirTemp(filepath.Dir(base), ".tree-*")
	if err != nil {
		return PreparedSource{}, err
	}
	defer os.RemoveAll(temporary)

	stream := io.Reader(&readerWithContext{ctx: ctx, reader: reader})
	var hasher hashWriter
	if verifyStreamDigest {
		hasher.writer = sha256.New()
		stream = io.TeeReader(stream, hasher.writer)
	}
	var archiveReader *tar.Reader
	if gzipArchive {
		compressed, err := gzip.NewReader(stream)
		if err != nil {
			return PreparedSource{}, fmt.Errorf("open Infra gzip archive: %w", err)
		}
		defer compressed.Close()
		archiveReader = tar.NewReader(compressed)
	} else {
		archiveReader = tar.NewReader(stream)
	}
	root := filepath.Join(temporary, "root")
	if err := os.MkdirAll(root, 0755); err != nil {
		return PreparedSource{}, err
	}
	if err := extractTarSafely(ctx, archiveReader, root, s.maxBytes); err != nil {
		return PreparedSource{}, err
	}
	if verifyStreamDigest {
		// Drain gzip trailers and any remaining compressed input before
		// comparing the complete archive digest.
		if _, err := io.Copy(io.Discard, stream); err != nil {
			return PreparedSource{}, err
		}
		actual := "sha256:" + hex.EncodeToString(hasher.writer.Sum(nil))
		if actual != digest {
			return PreparedSource{}, fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, digest, actual)
		}
	}
	if err := os.WriteFile(filepath.Join(temporary, ".complete"), []byte(digest+"\n"), 0444); err != nil {
		return PreparedSource{}, err
	}
	if err := os.Rename(temporary, base); err != nil {
		return PreparedSource{}, err
	}
	return s.preparedSource(hexDigest, false), nil
}

type hashWriter struct {
	writer interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func (s *ArtifactStore) lookupTreeLocked(ctx context.Context, hexDigest string) (PreparedSource, bool, error) {
	if err := ctx.Err(); err != nil {
		return PreparedSource{}, false, err
	}
	source := s.preparedSource(hexDigest, true)
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(source.PodRoot), ".complete"))
	if errors.Is(err, os.ErrNotExist) {
		return PreparedSource{}, false, nil
	}
	if err != nil {
		return PreparedSource{}, false, err
	}
	if strings.TrimSpace(string(marker)) != source.Digest {
		return PreparedSource{}, false, fmt.Errorf("%w: invalid tree completion marker", ErrArtifactCorrupted)
	}
	info, err := os.Stat(source.PodRoot)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("%w: tree root is not a directory", ErrArtifactCorrupted)
		}
		return PreparedSource{}, false, err
	}
	return source, true, nil
}

func (s *ArtifactStore) preparedSource(hexDigest string, cacheHit bool) PreparedSource {
	relative := filepath.Join("trees", "sha256", hexDigest, "root")
	return PreparedSource{
		Digest:   "sha256:" + hexDigest,
		PodRoot:  filepath.Join(s.podRoot, relative),
		HostRoot: filepath.Join(s.hostRoot, relative),
		CacheHit: cacheHit,
	}
}

func extractTarSafely(ctx context.Context, reader *tar.Reader, root string, maxBytes int64) error {
	var extracted int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Infra archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("infra archive path %q escapes extraction root", header.Name)
		}
		target := filepath.Join(root, name)
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("infra archive path %q escapes extraction root", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			extracted += header.Size
			if header.Size < 0 || extracted > maxBytes {
				return ErrArtifactTooLarge
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		case tar.TypeSymlink:
			link, resolved, err := safeSymlinkTarget(root, target, header.Linkname)
			if err != nil {
				return fmt.Errorf("infra archive symlink %q: %w", header.Name, err)
			}
			if !pathWithinRoot(root, resolved) {
				return fmt.Errorf("infra archive symlink %q escapes extraction root", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case tar.TypeLink:
			link := filepath.Clean(filepath.FromSlash(header.Linkname))
			if filepath.IsAbs(link) {
				link = strings.TrimPrefix(link, string(filepath.Separator))
			}
			if link == "." || link == ".." || strings.HasPrefix(link, ".."+string(filepath.Separator)) {
				return fmt.Errorf("infra archive hard link %q escapes extraction root", header.Name)
			}
			resolved := filepath.Join(root, link)
			if !pathWithinRoot(root, resolved) {
				return fmt.Errorf("infra archive hard link %q escapes extraction root", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Link(resolved, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("infra archive entry %q has unsupported type %d", header.Name, header.Typeflag)
		}
	}
}

// safeSymlinkTarget preserves rootfs semantics without allowing a link to
// resolve through the host filesystem. OCI root filesystems commonly contain
// absolute links such as /bin/arch -> /usr/bin/arch. Inside the artifact these
// are rooted at the image root, so rewrite them to an equivalent relative link
// under the verified extraction root.
func safeSymlinkTarget(root, target, linkname string) (string, string, error) {
	if linkname == "" {
		return "", "", errors.New("has an empty target")
	}
	link := filepath.Clean(filepath.FromSlash(linkname))
	if filepath.IsAbs(link) {
		resolved := filepath.Join(root, strings.TrimPrefix(link, string(filepath.Separator)))
		relative, err := filepath.Rel(filepath.Dir(target), resolved)
		if err != nil {
			return "", "", err
		}
		return relative, resolved, nil
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), link))
	return link, resolved, nil
}

func pathWithinRoot(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
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
