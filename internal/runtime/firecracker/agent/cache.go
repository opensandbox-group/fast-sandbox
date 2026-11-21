package agent

// Cache layout:
//
//	<StateRoot>/images/<sha256(image)>/
//	├── rootfs.img      # ← published rootfs.ext4
//	├── vmstate.snap
//	├── memory.snap
//	└── manifest.json   # commit point, written last
//
// The imageKey derivation must stay byte-identical to the driver's cache
// key (internal/runtime/firecracker/images.go) so the driver's
// resolveRootfsImage and GC work on the pulled artifacts unchanged.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

const (
	imageCacheDir = "images"
	manifestName  = "manifest.json"
	cacheFileMode = 0o640
	cacheDirMode  = 0o750

	// nativeRootfsCacheName is the cache name of the published rootfs
	// artifact (rootfs.ext4), kept byte-identical to the driver's
	// rootfsImageName so the existing resolveRootfsImage consumer works
	// on pulled artifacts unchanged.
	nativeRootfsCacheName = "rootfs.img"

	// verifiedDigestSettleWindow guards against coarse filesystem mtime
	// ticks (typically 1-4ms on Linux CONFIG_HZ): a file whose mtime is
	// still within the current clock quantum could accept an in-place
	// mutation without advancing mtime. Only files whose mtime has already
	// settled past this window (or were backdated at commit time) are
	// memoized in the verified-digest cache.
	verifiedDigestSettleWindow = 100 * time.Millisecond

	// committedFileMtimeBackdate backdates an immutable cache artifact's
	// mtime immediately before atomic rename into the cache so its stat
	// identity is settled immediately: any subsequent in-place write
	// advances mtime to the current kernel clock and invalidates the
	// cached digest even within the same filesystem timestamp tick.
	committedFileMtimeBackdate = 2 * time.Second

	// maxVerifiedDigestCacheEntries caps the process-local verified-digest
	// map so long-lived fastlet/agent processes never grow unboundedly.
	maxVerifiedDigestCacheEntries = 4096
)

type fileIdentity struct {
	dev       uint64
	ino       uint64
	size      int64
	mtimeNano int64
	ctimeNano int64
}

type verifiedDigestEntry struct {
	id     fileIdentity
	sha256 string
}

type verifiedDigestCache struct {
	mu      sync.RWMutex
	entries map[string]verifiedDigestEntry
}

var (
	defaultVerifiedDigests = &verifiedDigestCache{entries: make(map[string]verifiedDigestEntry)}
	// fileMatchesHashCount counts full-file SHA-256 reads performed by
	// fileMatches (inspected by unit tests to verify cache hits).
	fileMatchesHashCount atomic.Int64
)

func (c *verifiedDigestCache) matches(path string, id fileIdentity, expected string) bool {
	cleaned := filepath.Clean(path)
	c.mu.RLock()
	entry, ok := c.entries[cleaned]
	c.mu.RUnlock()
	return ok && entry.id == id && entry.sha256 == expected
}

func (c *verifiedDigestCache) record(path string, id fileIdentity, digest string) {
	cleaned := filepath.Clean(path)
	c.mu.Lock()
	if len(c.entries) >= maxVerifiedDigestCacheEntries {
		c.entries = make(map[string]verifiedDigestEntry)
	}
	c.entries[cleaned] = verifiedDigestEntry{id: id, sha256: digest}
	c.mu.Unlock()
}

func (c *verifiedDigestCache) invalidate(path string) {
	cleaned := filepath.Clean(path)
	c.mu.Lock()
	delete(c.entries, cleaned)
	c.mu.Unlock()
}

// PrepareCommittedCacheFile backdates a newly verified artifact's mtime
// before committing it into the node-local cache so its stat identity is
// immediately settled against coarse filesystem timestamp ticks.
func PrepareCommittedCacheFile(path string) {
	backdated := time.Now().Add(-committedFileMtimeBackdate)
	_ = os.Chtimes(path, backdated, backdated)
}

// RecordVerifiedCacheFile memoizes the verified SHA-256 of an immutable
// committed cache file so subsequent ImageReady checks skip re-hashing
// unchanged multi-GiB artifacts.
func RecordVerifiedCacheFile(path, digest string) {
	if digest == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	recordVerifiedFile(path, info, digest, time.Now())
}

func recordVerifiedFile(path string, info os.FileInfo, digest string, now time.Time) {
	if now.Sub(info.ModTime()) < verifiedDigestSettleWindow {
		return
	}
	id, ok := fileIdentityFromInfo(info)
	if !ok {
		return
	}
	defaultVerifiedDigests.record(path, id, digest)
}

// imageKey derives the content-addressed cache key of an image reference.
func imageKey(image string) string {
	digest := sha256.Sum256([]byte(image))
	return hex.EncodeToString(digest[:])
}

// imageDir returns the cache directory of an image reference.
func imageDir(stateRoot, image string) string {
	return filepath.Join(stateRoot, imageCacheDir, imageKey(image))
}

// ImageRootfsPath returns the cached native rootfs path of an image
// reference. The derivation is byte-identical to the driver's image cache
// key (internal/runtime/firecracker/images.go), so the runtime-agent server
// and the driver resolve the same file.
func ImageRootfsPath(stateRoot, image string) string {
	return filepath.Join(imageDir(stateRoot, image), nativeRootfsCacheName)
}

// ImageReady reports whether the image reference has a committed pull in the
// local cache: the commit-point manifest plus a native file set whose
// digests match.
func ImageReady(stateRoot, image string) (bool, error) {
	return cacheComplete(imageDir(stateRoot, image))
}

// CachedManifestDigest returns the sha256 digest of the committed local
// manifest of an image reference. The digest matches the index
// artifactDigest the pull verified, so callers can trust it without a
// network request. An image without a committed manifest reports false.
func CachedManifestDigest(stateRoot, image string) (string, bool, error) {
	dir := imageDir(stateRoot, image)
	payload, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if _, err := parseManifest(payload); err != nil {
		return "", false, nil //nolint:nilerr // a corrupt cached manifest means no committed pull; treat as not ready
	}
	return sha256Hex(payload), true, nil
}

// cacheComplete reports whether the image directory holds a committed pull:
// the local manifest plus a native file set whose digests match. It is the
// idempotency check: a complete cache is never touched again (the trade-off
// is documented on Client.PullImage).
func cacheComplete(dir string) (bool, error) {
	payload, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	document, err := parseManifest(payload)
	if err != nil {
		return false, nil //nolint:nilerr // a corrupt cached manifest means the pull is not complete
	}
	files, err := document.downloadFiles()
	if err != nil {
		return false, nil //nolint:nilerr // an incomplete native file set means the pull is not complete
	}
	for _, file := range files {
		match, err := fileMatches(filepath.Join(dir, file.cache), file)
		if err != nil || !match {
			return false, nil //nolint:nilerr // any mismatch or read failure means the pull is not complete; the next pull redoes it
		}
	}
	return true, nil
}

// stageFile ensures the artifact is in the cache with the expected size and
// digest: an existing matching file is skipped (resume semantics), a
// mismatching file is deleted and re-pulled, and the download lands in a
// temporary file that is renamed into place only after the whole content
// verifies. The object is fetched through the client, which routes artifact
// bytes over the P2P gateway when configured (falling back to direct S3).
func stageFile(ctx context.Context, c *Client, dir, storeKey string, file nativeFile) error {
	target := filepath.Join(dir, file.cache)
	match, err := fileMatches(target, file)
	if err == nil && match {
		return nil
	}
	if err == nil {
		// Corrupt cache entry: drop it before re-pulling. The local
		// manifest is left alone; the commit-point check decides whether
		// the whole pull needs redoing.
		klog.InfoS("corrupt cache entry detected; dropping it and re-pulling", "path", target)
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	body, err := c.getArtifact(ctx, storeKey)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(dir, file.cache+".tmp-*")
	if err != nil {
		return err
	}
	keep := false
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !keep {
			_ = os.Remove(tmp.Name())
		}
	}()
	digest := sha256.New()
	written, err := io.Copy(tmp, io.TeeReader(body, digest))
	if err != nil {
		return fmt.Errorf("download %s: %w", file.publish, err)
	}
	if written != file.sizeBytes {
		return fmt.Errorf("artifact %s size mismatch: got %d bytes, want %d", file.publish, written, file.sizeBytes)
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if sum != file.sha256 {
		return fmt.Errorf("artifact %s digest mismatch: got %s, want %s", file.publish, sum, file.sha256)
	}
	if err := tmp.Chmod(cacheFileMode); err != nil {
		return err
	}
	// Flush before the rename so the committed name never points at a
	// half-written file; the deferred cleanup skips the already-closed file.
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	PrepareCommittedCacheFile(tmp.Name())
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	keep = true
	RecordVerifiedCacheFile(target, sum)
	return nil
}

// commitManifest writes the downloaded manifest as the pull commit point:
// its presence with matching files means the pull is complete. The write is
// atomic so a crash never leaves a half-written manifest.
func commitManifest(dir string, payload []byte) error {
	tmp, err := os.CreateTemp(dir, manifestName+".tmp-*")
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(payload); err != nil {
		return err
	}
	if err := tmp.Chmod(cacheFileMode); err != nil {
		return err
	}
	// Flush before the rename so the committed name never points at a
	// half-written file; the deferred cleanup skips the already-closed file.
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	return os.Rename(tmp.Name(), filepath.Join(dir, manifestName))
}

// fileMatches reports whether the file exists with the expected size and
// sha256. The size is compared first via stat and unchanged settled files
// hit a process-local stat-identity cache (dev/ino/size/mtime/ctime), so
// repeated warm readiness checks avoid re-hashing multi-GiB artifacts while
// still catching any replacement or in-place mutation.
func fileMatches(path string, file nativeFile) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		defaultVerifiedDigests.invalidate(path)
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != file.sizeBytes {
		defaultVerifiedDigests.invalidate(path)
		return false, nil
	}
	statID, hasStatID := fileIdentityFromInfo(info)
	if hasStatID && defaultVerifiedDigests.matches(path, statID, file.sha256) {
		return true, nil
	}
	input, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer input.Close()
	beforeInfo, err := input.Stat()
	if err != nil {
		return false, err
	}
	if !beforeInfo.Mode().IsRegular() || beforeInfo.Size() != file.sizeBytes {
		defaultVerifiedDigests.invalidate(path)
		return false, nil
	}
	fileMatchesHashCount.Add(1)
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return false, err
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if sum != file.sha256 {
		defaultVerifiedDigests.invalidate(path)
		return false, nil
	}
	afterInfo, err := input.Stat()
	if err == nil {
		beforeID, okBefore := fileIdentityFromInfo(beforeInfo)
		afterID, okAfter := fileIdentityFromInfo(afterInfo)
		if hasStatID && okBefore && okAfter && statID == beforeID && beforeID == afterID {
			recordVerifiedFile(path, afterInfo, sum, time.Now())
		}
	}
	return true, nil
}
