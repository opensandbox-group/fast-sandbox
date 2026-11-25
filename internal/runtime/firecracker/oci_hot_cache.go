package firecracker

import (
	"context"
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
	"oras.land/oras-go/v2/registry"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
)

const (
	ociHotCheckpointDir = "oci/hot-checkpoints"
	ociHotPinsDir       = "oci/hot-pins"

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
	ociHotPinLockSuffix      = ".lock"
	ociHotCacheLockPollDelay = 10 * time.Millisecond

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

func ociHotPinsRoot(stateRoot string) string {
	return filepath.Join(stateRoot, ociHotPinsDir)
}

func ociHotCheckpointPath(stateRoot, artifactDigest string) string {
	return filepath.Join(ociHotCheckpointRoot(stateRoot), artifactDigest)
}

// ociReferenceMatchesDigest reports whether reference is digest-pinned to
// sha256:<artifactDigest>. Hot cache lookup requires this match so rootfs and
// memory state can never be mixed across checkpoints of the same sandbox.
func ociReferenceMatchesDigest(reference, artifactDigest string) bool {
	if !validImageDigest(artifactDigest) {
		return false
	}
	parsed, err := registry.ParseReference(strings.TrimPrefix(strings.TrimSpace(reference), ocitemplate.Prefix))
	if err != nil {
		return false
	}
	return parsed.Reference == "sha256:"+artifactDigest
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

func lockOCIHotCacheWithContext(ctx context.Context, root string, how int) (func(), error) {
	for {
		unlock, err := lockOCIHotCache(root, how|unix.LOCK_NB)
		if err == nil || !errors.Is(err, errHotCacheBusy) {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ociHotCacheLockPollDelay):
		}
	}
}

func lockOCIHotPin(pinDir string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(pinDir), 0o700); err != nil {
		return nil, fmt.Errorf("prepare hot checkpoint pin root: %w", err)
	}
	lockPath := pinDir + ociHotPinLockSuffix
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open hot checkpoint pin lock: %w", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errHotCacheBusy
		}
		return nil, fmt.Errorf("lock hot checkpoint pin: %w", err)
	}
	return func() {
		if err := os.RemoveAll(pinDir); err == nil {
			_ = os.Remove(lockPath)
		}
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// reclaimOrphanedOCIHotPins removes hot-pin directories left behind by a
// crashed Fastlet process so their hard links do not keep hot-cache entries
// pinned permanently. Active pins hold an exclusive flock on <pinDir>.lock.
func reclaimOrphanedOCIHotPins(stateRoot string) {
	pinsRoot := ociHotPinsRoot(stateRoot)
	items, err := os.ReadDir(pinsRoot)
	if err != nil {
		return
	}
	for _, item := range items {
		name := item.Name()
		if !strings.HasSuffix(name, ociHotPinLockSuffix) {
			continue
		}
		lockPath := filepath.Join(pinsRoot, name)
		pinDir := strings.TrimSuffix(lockPath, ociHotPinLockSuffix)
		lockFile, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lockFile.Close()
			continue
		}
		if err := os.RemoveAll(pinDir); err == nil {
			_ = os.Remove(lockPath)
		}
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
	}
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

	target := ociHotCheckpointPath(stateRoot, artifactDigest)
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
	if err := trimOCIHotCacheLocked(stateRoot, root, maxBytes, ociHotCheckpointMaxEntries-1, ociHotCheckpointMinFreeBytes, entryBytes, ociHotCheckpointCacheTTL()); err != nil {
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
// live pins (nlink > 1). The returned release function removes pinDir and
// releases its crash-recovery lock. Returns (false, nil, nil) on miss,
// expiration, contention, or cross-filesystem link failure so the caller falls
// back to OCI snapfiles.
func pinOCIHotCheckpoint(stateRoot, artifactDigest, pinDir string) (bool, func(), error) {
	if !validImageDigest(artifactDigest) || ociHotCheckpointCacheMaxBytes() <= 0 {
		return false, nil, nil
	}
	root := ociHotCheckpointRoot(stateRoot)
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	unlock, err := lockOCIHotCache(root, unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, errHotCacheBusy) {
			return false, nil, nil
		}
		return false, nil, err
	}
	defer unlock()

	entryDir := ociHotCheckpointPath(stateRoot, artifactDigest)
	info, err := os.Stat(entryDir)
	if err != nil || !info.IsDir() {
		return false, nil, nil
	}
	if ttl := ociHotCheckpointCacheTTL(); ttl > 0 && time.Since(info.ModTime()) > ttl {
		return false, nil, nil
	}
	if !isValidHotCheckpointDir(entryDir) {
		return false, nil, nil
	}

	releasePin, err := lockOCIHotPin(pinDir)
	if err != nil {
		if errors.Is(err, errHotCacheBusy) {
			return false, nil, nil
		}
		return false, nil, err
	}
	success := false
	defer func() {
		if !success {
			releasePin()
		}
	}()

	_ = os.RemoveAll(pinDir)
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		return false, nil, fmt.Errorf("create hot checkpoint pin dir: %w", err)
	}

	optionalFiles, err := presentOptionalBundles(entryDir)
	if err != nil {
		return false, nil, nil
	}
	files := append(append([]string(nil), ociHotRequiredFiles...), optionalFiles...)
	for _, name := range files {
		if err := os.Link(filepath.Join(entryDir, name), filepath.Join(pinDir, name)); err != nil {
			klog.V(2).InfoS("hot checkpoint pin link failed; falling back to OCI snapfiles",
				"digest", artifactDigest, "file", name, "err", err)
			return false, nil, nil
		}
	}
	touchPath(entryDir)
	success = true
	return true, releasePin, nil
}

// reclaimOCIHotCheckpoints evicts unpinned raw hot checkpoint entries when
// snapshot staging needs free disk space, even if new admission is disabled.
func reclaimOCIHotCheckpoints(ctx context.Context, stateRoot string, minFree int64) error {
	root := ociHotCheckpointRoot(stateRoot)
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	unlock, err := lockOCIHotCacheWithContext(ctx, root, unix.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	requiredFree := uint64(0)
	if minFree > 0 {
		requiredFree = uint64(minFree)
	}
	return trimOCIHotCacheLocked(stateRoot, root, ociHotCheckpointCacheMaxBytes(), ociHotCheckpointMaxEntries, requiredFree, 0, ociHotCheckpointCacheTTL())
}

type hotCacheEntry struct {
	path    string
	bytes   int64
	modTime time.Time
	pinned  bool
}

func trimOCIHotCacheLocked(stateRoot, root string, maxBytes int64, maxEntries int, minFree uint64, incomingBytes int64, ttl time.Duration) error {
	reclaimOrphanedOCIHotPins(stateRoot)
	entries, total, err := scanHotCheckpointEntries(root)
	if err != nil {
		return err
	}
	remaining, total, err := reapExpiredHotEntries(entries, total, ttl)
	if err != nil {
		return err
	}
	avail, err := dirFreeBytes(root)
	if err != nil {
		return err
	}

	count := len(remaining)
	highWatermark := maxBytes * ociHotCacheHighWatermarkPct / 100
	needEvict := total+incomingBytes > maxBytes ||
		(maxBytes > 0 && total+incomingBytes > highWatermark) ||
		count > maxEntries ||
		avail < minFree
	if needEvict {
		targetBytes := hotCacheEvictTarget(maxBytes, incomingBytes)
		total, count, avail, err = evictHotEntriesToTarget(root, remaining, total, targetBytes, maxEntries, avail, minFree)
		if err != nil {
			return err
		}
	}

	if maxBytes > 0 && total > maxBytes*ociHotCacheAlarmPct/100 {
		klog.Warningf("OCI hot checkpoint cache usage remains high after eviction: used=%d bytes budget=%d bytes entries=%d", total, maxBytes, count)
	}
	if total+incomingBytes > maxBytes || count > maxEntries || avail < minFree {
		pinnedCount := 0
		for _, e := range remaining {
			if e.pinned {
				pinnedCount++
			}
		}
		return fmt.Errorf("hot checkpoint cache budget or free-disk reserve exhausted (used=%d incoming=%d budget=%d free=%d minFree=%d pinned=%d)",
			total, incomingBytes, maxBytes, avail, minFree, pinnedCount)
	}
	return nil
}

func scanHotCheckpointEntries(root string) ([]hotCacheEntry, int64, error) {
	items, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
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
		entries = append(entries, hotCacheEntry{path: fullPath, bytes: size, modTime: info.ModTime(), pinned: pinned})
		total += size
	}
	return entries, total, nil
}

func reapExpiredHotEntries(entries []hotCacheEntry, total int64, ttl time.Duration) ([]hotCacheEntry, int64, error) {
	if ttl <= 0 {
		return entries, total, nil
	}
	now := time.Now()
	remaining := make([]hotCacheEntry, 0, len(entries))
	for _, e := range entries {
		if !e.pinned && now.Sub(e.modTime) > ttl {
			if err := os.RemoveAll(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, total, err
			}
			total -= e.bytes
			continue
		}
		remaining = append(remaining, e)
	}
	return remaining, total, nil
}

func hotCacheEvictTarget(maxBytes, incomingBytes int64) int64 {
	targetBytes := maxBytes - incomingBytes
	lowWatermark := maxBytes * ociHotCacheLowWatermarkPct / 100
	if maxBytes > 0 && lowWatermark < targetBytes {
		targetBytes = lowWatermark
	}
	if targetBytes < 0 {
		return 0
	}
	return targetBytes
}

func evictHotEntriesToTarget(root string, entries []hotCacheEntry, total, targetBytes int64, maxEntries int, avail, minFree uint64) (int64, int, uint64, error) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})
	count := len(entries)
	for _, e := range entries {
		if total <= targetBytes && count <= maxEntries && avail >= minFree {
			break
		}
		if e.pinned {
			continue
		}
		if err := os.RemoveAll(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return total, count, avail, err
		}
		total -= e.bytes
		count--
		var err error
		avail, err = dirFreeBytes(root)
		if err != nil {
			return total, count, avail, err
		}
	}
	return total, count, avail, nil
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
		if info.Size() > 0 {
			present[item.Name()] = true
		}
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
