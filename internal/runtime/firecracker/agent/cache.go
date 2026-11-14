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

	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
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
)

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

// ImageReadyVerified is ImageReady for the chunked transfer mode: a file
// whose verified-cache marker still describes it is trusted without being
// re-hashed (see marker.go).
func ImageReadyVerified(stateRoot, image string) (bool, error) {
	return cacheCompleteWith(imageDir(stateRoot, image), true)
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
	return cacheCompleteWith(dir, false)
}

// cacheCompleteWith is cacheComplete; marked trusts verified-cache markers
// (chunked transfer mode) instead of re-hashing every file.
func cacheCompleteWith(dir string, marked bool) (bool, error) {
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
	files, err := document.nativeFiles()
	if err != nil {
		return false, nil //nolint:nilerr // an incomplete native file set means the pull is not complete
	}
	for _, file := range files {
		match, err := verifyCacheFile(filepath.Join(dir, file.cache), file, marked)
		if err != nil || !match {
			return false, nil //nolint:nilerr // any mismatch or read failure means the pull is not complete; the next pull redoes it
		}
	}
	return true, nil
}

// verifyCacheFile checks one cached file: a full fileMatches, or with
// marked the marker-fronted fileVerified.
func verifyCacheFile(path string, file nativeFile, marked bool) (bool, error) {
	if marked {
		return fileVerified(path, file)
	}
	return fileMatches(path, file)
}

// fileVerified is fileMatches with the verified-cache marker in front: a
// file whose marker still describes it is trusted without reading it, and a
// file that verifies the slow way gets a marker for the next check.
func fileVerified(path string, file nativeFile) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() != file.sizeBytes {
		return false, nil
	}
	if markerMatches(path, info, file.sha256) {
		return true, nil
	}
	match, err := fileMatches(path, file)
	if err == nil && match {
		markVerified(path, file.sha256)
	}
	return match, err
}

// stageFile ensures the artifact is in the cache with the expected size and
// digest: an existing matching file is skipped (resume semantics), a
// mismatching file is deleted and re-pulled, and the download lands in a
// temporary file that is renamed into place only after the whole content
// verifies. The object is fetched through the client, which routes artifact
// bytes over the P2P gateway when configured (falling back to direct S3).
func stageFile(ctx context.Context, c *Client, dir, storeKey string, file nativeFile) error {
	chunked := c.transfer.Chunked()
	target := filepath.Join(dir, file.cache)
	match, err := verifyCacheFile(target, file, chunked)
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
	var sink io.Writer = tmp
	if chunked {
		// All-zero blocks become holes: the logical content (and digest)
		// is unchanged, but empty disk and RAM ranges never hit the disk.
		sink = sparseWriter{file: tmp}
	}
	written, err := io.Copy(sink, io.TeeReader(body, digest))
	if err != nil {
		return fmt.Errorf("download %s: %w", file.publish, err)
	}
	if written != file.sizeBytes {
		return fmt.Errorf("artifact %s size mismatch: got %d bytes, want %d", file.publish, written, file.sizeBytes)
	}
	if chunked {
		// A trailing hole has no write behind it: set the logical size.
		if err := tmp.Truncate(written); err != nil {
			return err
		}
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if sum != file.sha256 {
		return fmt.Errorf("artifact %s digest mismatch: got %s, want %s", file.publish, sum, file.sha256)
	}
	// The deferred cleanup skips the file finishCacheFile closes.
	closed = true
	if err := finishCacheFile(tmp, target, file.sha256, chunked); err != nil {
		return err
	}
	keep = true
	return nil
}

// finishCacheFile commits a verified temporary cache file: mode, then the
// close that flushes it before the rename so the committed name never
// points at a half-written file. With marked (chunked transfer mode) the
// file is also fsynced and gets its verified-cache marker: the marker
// replaces the per-lease re-hash, so nothing would catch a torn file later.
// The file is closed on every path.
func finishCacheFile(tmp *os.File, target, sum string, marked bool) error {
	if err := tmp.Chmod(cacheFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if marked {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if marked {
		markVerified(tmp.Name(), sum)
	}
	return os.Rename(tmp.Name(), target)
}

// sparseBlock is the zero-detection granularity of sparse cache writes.
const sparseBlock = 64 << 10

// sparseWriter writes a byte stream into a file, seeking over all-zero
// 64 KiB-aligned blocks instead of writing them. io.Copy hands it reads of
// arbitrary size; blocks are judged on the slices it receives.
type sparseWriter struct{ file *os.File }

func (w sparseWriter) Write(payload []byte) (int, error) {
	total := len(payload)
	for len(payload) > 0 {
		n := min(len(payload), sparseBlock)
		block := payload[:n]
		if artifacts.IsZero(block) {
			if _, err := w.file.Seek(int64(n), io.SeekCurrent); err != nil {
				return total - len(payload), err
			}
		} else if _, err := w.file.Write(block); err != nil {
			return total - len(payload), err
		}
		payload = payload[n:]
	}
	return total, nil
}

// syncDir fsyncs a directory so renames into it are durable.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// commitManifest writes the downloaded manifest as the pull commit point:
// its presence with matching files means the pull is complete. The write is
// atomic so a crash never leaves a half-written manifest. With durable
// (chunked transfer mode) the manifest is fsynced and one directory fsync
// makes the artifact renames and the commit point durable together.
func commitManifest(dir string, payload []byte, durable bool) error {
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
	if durable {
		if err := tmp.Sync(); err != nil {
			return err
		}
	}
	// Flush before the rename so the committed name never points at a
	// half-written file; the deferred cleanup skips the already-closed file.
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(tmp.Name(), filepath.Join(dir, manifestName)); err != nil {
		return err
	}
	if !durable {
		return nil
	}
	return syncDir(dir)
}

// fileMatches reports whether the file exists with the expected size and
// sha256. The size is compared first via stat: hashing a multi-GiB rootfs
// on every idempotency check would defeat the point of the warm cache, so
// the full digest is only computed when the size matches.
func fileMatches(path string, file nativeFile) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() != file.sizeBytes {
		return false, nil
	}
	input, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer input.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return false, err
	}
	return hex.EncodeToString(digest.Sum(nil)) == file.sha256, nil
}
