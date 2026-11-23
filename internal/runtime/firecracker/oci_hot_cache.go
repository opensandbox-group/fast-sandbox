package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
)

const (
	ociHotCheckpointDir = "oci/hot-checkpoints"

	ociHotCheckpointCacheSizeEnv = "FAST_SANDBOX_OCI_HOT_CHECKPOINT_CACHE_SIZE_BYTES"
	ociHotCheckpointCacheTTLEnv  = "FAST_SANDBOX_OCI_HOT_CHECKPOINT_CACHE_TTL"

	defaultOCIHotCheckpointCacheBytes int64  = 16 << 30 // 16 GiB
	defaultOCIHotCheckpointCacheTTL          = 30 * time.Minute
	ociHotCheckpointMaxEntries               = 64
	ociHotCheckpointMinFreeBytes      uint64 = 1 << 30 // 1 GiB

	ociHotCacheHighWatermarkPct = 80
	ociHotCacheLowWatermarkPct  = 60
	ociHotCacheAlarmPct         = 75

	ociHotCacheLockName      = ".cache.lock"
	ociHotCacheStagingPrefix = ".staging-"

	ociVMStateFileName = "vmstate.bin"
	ociMemfileName     = "memfile"
)

var (
	errHotCacheBusy = errors.New("hot checkpoint cache busy")

	ociHotRequiredFiles = []string{ociVMStateFileName, ociMemfileName, snapshotManifestName}
)

// ociHotCheckpointCacheMaxBytes returns the configured byte budget for raw
// same-node OCI checkpoints. Zero disables admission and lookup.
func ociHotCheckpointCacheMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(ociHotCheckpointCacheSizeEnv))
	if raw == "" {
		return defaultOCIHotCheckpointCacheBytes
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		klog.Warningf("invalid %s=%q; using default %d", ociHotCheckpointCacheSizeEnv, raw, defaultOCIHotCheckpointCacheBytes)
		return defaultOCIHotCheckpointCacheBytes
	}
	return parsed
}

// ociHotCheckpointCacheTTL returns the idle TTL for raw hot checkpoint entries.
// Non-positive values disable age-based expiration.
func ociHotCheckpointCacheTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv(ociHotCheckpointCacheTTLEnv))
	if raw == "" {
		return defaultOCIHotCheckpointCacheTTL
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		klog.Warningf("invalid %s=%q; using default %s", ociHotCheckpointCacheTTLEnv, raw, defaultOCIHotCheckpointCacheTTL)
		return defaultOCIHotCheckpointCacheTTL
	}
	return parsed
}

func ociHotCheckpointRoot(stateRoot string) string {
	return filepath.Join(stateRoot, ociHotCheckpointDir)
}

func ociHotCheckpointPath(stateRoot, artifactDigest string) string {
	return filepath.Join(ociHotCheckpointRoot(stateRoot), artifactDigest)
}

func touchPath(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

func validateRegularNonEmpty(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, fmt.Errorf("%s is not a non-empty regular file", filepath.Base(path))
	}
	return info, nil
}

func presentOptionalBundles(dir string) ([]string, error) {
	var found []string
	for _, name := range optionalStateBundleNames {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			found = append(found, name)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", name, err)
		}
	}
	return found, nil
}

func lockOCIHotCache(root string, how int) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("prepare hot checkpoint cache root: %w", err)
	}
	lockPath := filepath.Join(root, ociHotCacheLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open hot checkpoint cache lock: %w", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), how); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errHotCacheBusy
		}
		return nil, fmt.Errorf("lock hot checkpoint cache: %w", err)
	}
	return func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// retainOCIHotCheckpoint moves the freshly dumped, uncompressed checkpoint
// artifacts from staging into the node-local hot cache using same-filesystem
// renames so the warm kernel page cache is preserved without copying. Callers
// treat failures as best-effort; registry publication remains authoritative.
func retainOCIHotCheckpoint(stateRoot, artifactDigest, staging string) error {
	if !validImageDigest(artifactDigest) {
		return fmt.Errorf("invalid hot checkpoint digest %q", artifactDigest)
	}
	maxBytes := ociHotCheckpointCacheMaxBytes()
	if maxBytes <= 0 {
		return nil
	}
	root := ociHotCheckpointRoot(stateRoot)
	unlock, err := lockOCIHotCache(root, unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		return err
	}
	defer unlock()

	target := filepath.Join(root, artifactDigest)
	if isValidHotCheckpointDir(target) {
		touchPath(target)
		return nil
	}
	_ = os.RemoveAll(target)

	mappings := []struct{ srcName, dstName string }{
		{vmstateSnapshotName, ociVMStateFileName},
		{memorySnapshotName, ociMemfileName},
		{snapshotManifestName, snapshotManifestName},
	}
	optionalFiles, err := presentOptionalBundles(staging)
	if err != nil {
		return err
	}
	for _, name := range optionalFiles {
		mappings = append(mappings, struct{ srcName, dstName string }{name, name})
	}

	var entryBytes int64
	for _, m := range mappings {
		info, err := validateRegularNonEmpty(filepath.Join(staging, m.srcName))
		if err != nil {
			return fmt.Errorf("staged %w", err)
		}
		entryBytes += info.Size()
	}
	if entryBytes > maxBytes {
		return fmt.Errorf("checkpoint size %d exceeds hot cache budget %d", entryBytes, maxBytes)
	}
	if err := trimOCIHotCacheLocked(root, maxBytes, ociHotCheckpointMaxEntries-1, ociHotCheckpointMinFreeBytes, entryBytes, ociHotCheckpointCacheTTL()); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp(root, ociHotCacheStagingPrefix+artifactDigest+"-")
	if err != nil {
		return fmt.Errorf("create hot checkpoint staging dir: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	for _, m := range mappings {
		if err := os.Rename(filepath.Join(staging, m.srcName), filepath.Join(tmpDir, m.dstName)); err != nil {
			return fmt.Errorf("move %s into hot checkpoint cache: %w", m.srcName, err)
		}
	}
	if err := os.Rename(tmpDir, target); err != nil {
		return fmt.Errorf("commit hot checkpoint entry: %w", err)
	}
	cleanupTmp = false
	touchPath(target)
	return nil
}

// pinOCIHotCheckpoint hard-links a cached raw checkpoint into pinDir under a
// shared cache lock so concurrent resumes never contend while eviction respects
// live pins (nlink > 1). Returns (false, nil) on miss, expiration, contention,
// or cross-filesystem link failure so the caller falls back to OCI snapfiles.
func pinOCIHotCheckpoint(stateRoot, artifactDigest, pinDir string) (bool, error) {
	if !validImageDigest(artifactDigest) || ociHotCheckpointCacheMaxBytes() <= 0 {
		return false, nil
	}
	root := ociHotCheckpointRoot(stateRoot)
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	unlock, err := lockOCIHotCache(root, unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, errHotCacheBusy) {
			return false, nil
		}
		return false, err
	}
	defer unlock()

	entryDir := filepath.Join(root, artifactDigest)
	info, err := os.Stat(entryDir)
	if err != nil || !info.IsDir() {
		return false, nil
	}
	if ttl := ociHotCheckpointCacheTTL(); ttl > 0 && time.Since(info.ModTime()) > ttl {
		return false, nil
	}
	if !isValidHotCheckpointDir(entryDir) {
		return false, nil
	}

	_ = os.RemoveAll(pinDir)
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		return false, fmt.Errorf("create hot checkpoint pin dir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(pinDir)
		}
	}()

	optionalFiles, err := presentOptionalBundles(entryDir)
	if err != nil {
		return false, nil
	}
	files := append(append([]string(nil), ociHotRequiredFiles...), optionalFiles...)
	for _, name := range files {
		if err := os.Link(filepath.Join(entryDir, name), filepath.Join(pinDir, name)); err != nil {
			klog.V(2).InfoS("hot checkpoint pin link failed; falling back to OCI snapfiles",
				"digest", artifactDigest, "file", name, "err", err)
			return false, nil
		}
	}
	touchPath(entryDir)
	success = true
	return true, nil
}

// reclaimOCIHotCheckpoints evicts unpinned raw hot checkpoint entries when
// snapshot staging needs free disk space, even if new admission is disabled.
func reclaimOCIHotCheckpoints(stateRoot string, minFree int64) error {
	root := ociHotCheckpointRoot(stateRoot)
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	unlock, err := lockOCIHotCache(root, unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		return err
	}
	defer unlock()
	requiredFree := uint64(0)
	if minFree > 0 {
		requiredFree = uint64(minFree)
	}
	return trimOCIHotCacheLocked(root, ociHotCheckpointCacheMaxBytes(), ociHotCheckpointMaxEntries, requiredFree, 0, ociHotCheckpointCacheTTL())
}

type hotCacheEntry struct {
	path    string
	bytes   int64
	modTime time.Time
	pinned  bool
}

func trimOCIHotCacheLocked(root string, maxBytes int64, maxEntries int, minFree uint64, incomingBytes int64, ttl time.Duration) error {
	items, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var entries []hotCacheEntry
	var total int64
	for _, item := range items {
		name := item.Name()
		if name == ociHotCacheLockName {
			continue
		}
		fullPath := filepath.Join(root, name)
		if strings.HasPrefix(name, ociHotCacheStagingPrefix) {
			// Exclusive lock guarantees no concurrent writer owns this staging dir.
			_ = os.RemoveAll(fullPath)
			continue
		}
		if !item.IsDir() || !validImageDigest(name) {
			continue
		}
		info, err := item.Info()
		if err != nil {
			continue
		}
		size, pinned, ok := inspectHotCheckpointEntry(fullPath)
		if !ok {
			if !pinned {
				_ = os.RemoveAll(fullPath)
			}
			continue
		}
		entries = append(entries, hotCacheEntry{
			path:    fullPath,
			bytes:   size,
			modTime: info.ModTime(),
			pinned:  pinned,
		})
		total += size
	}

	now := time.Now()
	count := len(entries)
	remaining := make([]hotCacheEntry, 0, len(entries))
	for _, e := range entries {
		if ttl > 0 && !e.pinned && now.Sub(e.modTime) > ttl {
			if err := os.RemoveAll(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			total -= e.bytes
			count--
			continue
		}
		remaining = append(remaining, e)
	}

	avail, err := dirFreeBytes(root)
	if err != nil {
		return err
	}

	highWatermark := maxBytes * ociHotCacheHighWatermarkPct / 100
	lowWatermark := maxBytes * ociHotCacheLowWatermarkPct / 100
	needEvict := total+incomingBytes > maxBytes ||
		(maxBytes > 0 && total > highWatermark) ||
		count > maxEntries ||
		avail < minFree

	if needEvict {
		targetBytes := maxBytes - incomingBytes
		if maxBytes > 0 && lowWatermark < targetBytes {
			targetBytes = lowWatermark
		}
		if targetBytes < 0 {
			targetBytes = 0
		}
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].modTime.Before(remaining[j].modTime)
		})
		for _, e := range remaining {
			if total <= targetBytes && count <= maxEntries && avail >= minFree {
				break
			}
			if e.pinned {
				continue
			}
			if err := os.RemoveAll(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			total -= e.bytes
			count--
			avail, err = dirFreeBytes(root)
			if err != nil {
				return err
			}
		}
	}

	if maxBytes > 0 && total > maxBytes*ociHotCacheAlarmPct/100 {
		klog.Warningf("OCI hot checkpoint cache usage remains high after eviction: used=%d bytes budget=%d bytes entries=%d", total, maxBytes, count)
	}
	if total+incomingBytes > maxBytes || count > maxEntries || avail < minFree {
		return fmt.Errorf("hot checkpoint cache budget or free-disk reserve exhausted; pinned entries retained (used=%d incoming=%d budget=%d free=%d minFree=%d)",
			total, incomingBytes, maxBytes, avail, minFree)
	}
	return nil
}

func dirFreeBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func isValidHotCheckpointDir(dir string) bool {
	for _, name := range ociHotRequiredFiles {
		if _, err := validateRegularNonEmpty(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, snapshotManifestName))
	if err != nil {
		return false
	}
	manifest, err := artifacts.DecodeVMManifest(raw)
	return err == nil && !manifest.IsRootfsOnly()
}

func inspectHotCheckpointEntry(dir string) (totalBytes int64, pinned bool, valid bool) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, false
	}
	present := map[string]bool{}
	for _, item := range items {
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		totalBytes += info.Size()
		present[item.Name()] = true
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
			pinned = true
		}
	}
	valid = true
	for _, req := range ociHotRequiredFiles {
		if !present[req] {
			valid = false
			break
		}
	}
	return totalBytes, pinned, valid
}
