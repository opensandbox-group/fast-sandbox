package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const blobCacheMaxFiles = 4096
const blobCacheMinFreeBytes = 1 << 30

// Default watermarks from the node-local cache eviction spec: eviction starts
// when an admission would push ledger usage above 80% of the byte budget,
// drives usage down to 60%, and the alarm gauge fires above 75%. The target
// leaves room for the sweep to finish ahead of new admissions instead of
// re-triggering at the boundary on every admission.
const (
	defaultBlobCacheEvictTriggerPercent = 80
	defaultBlobCacheEvictTargetPercent  = 60
	defaultBlobCacheWarnPercent         = 75
)

// blobCacheWatermarks are derived from the configured byte budget. A
// non-positive budget disables admission and watermark-driven eviction.
type blobCacheWatermarks struct {
	trigger int64
	target  int64
	warn    int64
}

func (c *config) blobCacheWatermarks() blobCacheWatermarks {
	scale := func(percent int) int64 { return blobCachePercentOf(c.blobCacheMaxBytes, percent) }
	return blobCacheWatermarks{
		trigger: scale(c.blobCacheEvictTriggerPercent),
		target:  scale(c.blobCacheEvictTargetPercent),
		warn:    scale(c.blobCacheWarnPercent),
	}
}

// blobCachePercentOf computes percent of total without overflowing on budgets
// near the int64 limit.
func blobCachePercentOf(total int64, percent int) int64 {
	if total <= 0 || percent <= 0 {
		return 0
	}
	return (total/100)*int64(percent) + (total%100)*int64(percent)/100
}

// validateBlobCacheWatermarks enforces the ordered three-tier scheme: the
// target is at or below the alarm and the alarm at or below the trigger. All
// three at 100 reproduces the single-threshold behavior of evicting only down
// to the hard budget.
func validateBlobCacheWatermarks(trigger, target, warn int) error {
	for _, percent := range []int{trigger, target, warn} {
		if percent < 0 || percent > 100 {
			return fmt.Errorf("blob-cache watermarks must be percentages between 0 and 100")
		}
	}
	if target > warn || warn > trigger {
		return fmt.Errorf("blob-cache watermarks must satisfy target <= warn <= trigger (got target=%d, warn=%d, trigger=%d)", target, warn, trigger)
	}
	return nil
}

// Capacity recovery also reclaims retained entries after admission is disabled.
func newTrimBlobCacheCommand() *cobra.Command {
	var minFree uint64
	command := &cobra.Command{Use: "trim-blob-cache", Short: "Reclaim unpinned checkpoint blobs for snapshot staging", Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(blobCacheRoot(), "sha256")); os.IsNotExist(err) {
				return nil
			} else if err != nil {
				return err
			}
			unlock, err := tryLock(blobCacheRoot(), "cache")
			if err != nil {
				return err
			}
			defer unlock()
			return trimBlobCache(blobCacheLimits{maxBytes: global.blobCacheMaxBytes, maxFiles: blobCacheMaxFiles, minFree: minFree})
		}}
	command.Flags().Uint64Var(&minFree, "min-free-bytes", 0, "required free bytes on the cache/staging filesystem")
	_ = command.MarkFlagRequired("min-free-bytes")
	return command
}

func blobCacheRoot() string {
	if global.blobCacheDir != "" {
		return global.blobCacheDir
	}
	return filepath.Join(global.stateDir, ".blob-cache")
}

func blobCachePath(sum digest.Digest) string {
	return filepath.Join(blobCacheRoot(), "sha256", sum.Encoded())
}

// retainBlob runs under the cache lock. Only the publisher's verified, private
// staging files may enter this cache. Cross-filesystem rename is a cache miss;
// never turn the optimization into a second copy on the publication path.
func retainBlob(source string, sum digest.Digest, size int64) error {
	target := blobCachePath(sum)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() || info.Size() != size {
			return fmt.Errorf("conflicting local cache entry %s", sum)
		}
		now := time.Now()
		return os.Chtimes(target, now, now)
	} else if !os.IsNotExist(err) {
		return err
	}
	if size > global.blobCacheMaxBytes {
		return fmt.Errorf("checkpoint blob exceeds local cache budget")
	}
	// Reserve the incoming quota before admission: once this fill would cross
	// the trigger watermark, the pre-pass drives the cache to the target so
	// eviction finishes ahead of concurrent admissions instead of chasing
	// them after the fact.
	if err := trimBlobCache(blobCacheLimits{
		maxBytes: global.blobCacheMaxBytes - size,
		maxFiles: blobCacheMaxFiles - 1,
		minFree:  blobCacheMinFreeBytes,
		reserve:  size,
	}); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("retain checkpoint blob: %w", err)
	}
	now := time.Now()
	if err := os.Chtimes(target, now, now); err != nil {
		return err
	}
	// Keep the resident gauges usable between eviction passes by applying the
	// admission delta to the last snapshot.
	var disk int64
	haveDisk := false
	if info, err := os.Stat(target); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			disk, haveDisk = stat.Blocks*512, true
		}
	}
	updateBlobCacheStats(func(stats *blobCacheStats) {
		stats.Counters.Admissions++
		stats.Counters.AdmissionBytes += size
		stats.Cache.Entries++
		stats.Cache.Bytes += size
		if haveDisk {
			stats.Cache.DiskBytes += disk
		}
		stats.Cache.LedgerDiskDeltaBytes = stats.Cache.Bytes - stats.Cache.DiskBytes
		stats.Cache.UsagePercent = blobCacheUsagePercent(stats.Cache.Bytes, stats.BudgetBytes)
		stats.Cache.DiskUsagePercent = blobCacheUsagePercent(stats.Cache.DiskBytes, stats.BudgetBytes)
	})
	return nil
}

// blobCacheLimits bound one eviction pass. maxBytes, maxFiles, and minFree are
// hard budgets whose violation is an error; reserve is the byte quota an
// imminent admission will consume when deciding whether to trigger.
type blobCacheLimits struct {
	maxBytes int64
	maxFiles int
	minFree  uint64
	reserve  int64
}

// blobCacheScan is one eviction pass working set. bytes is the policy ledger;
// diskBytes (allocated blocks) feeds only the alarm gauge and the
// reconciliation delta, so ledger overcounts cannot raise false alarms.
type blobCacheScan struct {
	entries     []blobCacheEntry
	count       int
	bytes       int64
	diskBytes   int64
	pinnedCount int
	pinnedBytes int64
}

type blobCacheEntry struct {
	path      string
	size      int64
	diskBytes int64
	pinned    bool
	modified  time.Time
}

func scanBlobCache(root string) (*blobCacheScan, error) {
	items, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return &blobCacheScan{}, nil
	}
	if err != nil {
		return nil, err
	}
	scan := &blobCacheScan{}
	for _, item := range items {
		info, err := item.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		entry := blobCacheEntry{
			path:     filepath.Join(root, item.Name()),
			size:     info.Size(),
			modified: info.ModTime(),
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			entry.diskBytes = stat.Blocks * 512
			entry.pinned = stat.Nlink > 1
		}
		if entry.pinned {
			scan.pinnedCount++
			scan.pinnedBytes += entry.size
		}
		scan.entries = append(scan.entries, entry)
		scan.bytes += entry.size
		scan.diskBytes += entry.diskBytes
	}
	scan.count = len(scan.entries)
	return scan, nil
}

// trimBlobCache evicts the least recently selected unpinned entries until the
// hard budgets hold, and once ledger usage (including the reserved admission
// quota) crosses the trigger watermark, until it reaches the target watermark.
// Live pinned files are retained: their bytes stay counted against the budget,
// so a triggered pass that cannot reach the target still reports the pin
// fraction through the stats snapshot instead of unlinking files whose disk
// space would not actually be reclaimed. Callers hold the cache lock, shared
// with pin creation and other publishers.
func trimBlobCache(limits blobCacheLimits) error {
	started := time.Now()
	root := filepath.Join(blobCacheRoot(), "sha256")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	scan, err := scanBlobCache(root)
	if err != nil {
		return err
	}
	sort.Slice(scan.entries, func(i, j int) bool { return scan.entries[i].modified.Before(scan.entries[j].modified) })
	watermarks := global.blobCacheWatermarks()
	watermarksEnabled := global.blobCacheMaxBytes > 0
	hardOK := func() (bool, error) {
		var stat unix.Statfs_t
		if err := unix.Statfs(root, &stat); err != nil {
			return false, err
		}
		return scan.bytes <= limits.maxBytes && scan.count <= limits.maxFiles && stat.Bavail*uint64(stat.Bsize) >= limits.minFree, nil
	}
	triggered := watermarksEnabled && scan.bytes+limits.reserve > watermarks.trigger
	evictedEntries := 0
	var evictedBytes int64
	for _, item := range scan.entries {
		ok, err := hardOK()
		if err != nil {
			return err
		}
		soft := triggered && scan.bytes > watermarks.target
		if ok && !soft {
			break
		}
		if item.pinned {
			// Links are created only under the same lock; teardown may remove
			// a link concurrently, making this snapshot conservative for one
			// pass.
			continue
		}
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		scan.bytes -= item.size
		scan.diskBytes -= item.diskBytes
		scan.count--
		evictedEntries++
		evictedBytes += item.size
	}
	ok, err := hardOK()
	if err != nil {
		return err
	}
	state := blobCacheState(scan, watermarks, global.blobCacheMaxBytes, ok)
	reachedTarget := !triggered || scan.bytes <= watermarks.target
	recordBlobCacheTrim(scan, evictedEntries, evictedBytes, started, reachedTarget, state)
	if !ok {
		return fmt.Errorf("local blob cache budget or free-disk reserve exhausted; live device files retained")
	}
	if state == blobCacheStateWarn {
		fmt.Fprintf(os.Stderr, "blob cache: allocated disk usage %d bytes exceeds the %d%% alarm watermark (%d bytes); %d pinned bytes retained\n",
			scan.diskBytes, global.blobCacheWarnPercent, watermarks.warn, scan.pinnedBytes)
	}
	return nil
}

// Pins live beside the device config. Existing teardown removes that directory
// only after unmount and device release, including crash recovery and shared
// snapfiles. No process-local reference count or daemon extension is needed.
func cachedLowers(home string, image resolvedImage) []OverlayBDBSConfigLower {
	lowers := lowersFor(image)
	if global.blobCacheMaxBytes <= 0 {
		return lowers
	}
	unlock, err := tryLock(blobCacheRoot(), "cache")
	if err != nil {
		return lowers
	}
	defer unlock()
	hits, misses := 0, 0
	for i := range lowers {
		lower := &lowers[i]
		sum := digest.Digest(lower.Digest)
		if sum.Validate() != nil || sum.Algorithm() != digest.SHA256 {
			continue
		}
		source := blobCachePath(sum)
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() || info.Size() != lower.Size {
			misses++
			continue
		}
		pin := filepath.Join(home, ".local-blobs", sum.Encoded())
		if err := os.MkdirAll(filepath.Dir(pin), 0700); err != nil {
			misses++
			continue
		}
		if err := os.Link(source, pin); err != nil {
			existing, statErr := os.Lstat(pin)
			if statErr != nil || !os.SameFile(info, existing) {
				misses++
				continue
			}
		}
		lower.File = pin
		hits++
		now := time.Now()
		_ = os.Chtimes(source, now, now)
	}
	if hits+misses > 0 {
		updateBlobCacheStats(func(stats *blobCacheStats) {
			stats.Counters.LookupHits += int64(hits)
			stats.Counters.LookupMisses += int64(misses)
		})
	}
	return lowers
}
