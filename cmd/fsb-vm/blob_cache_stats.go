package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The node-local blob cache follows the eviction spec in issue #196: three
// watermarks, cumulative eviction and lookup counters, and an alarm gauge that
// reads allocated disk bytes instead of the byte ledger used by eviction.
// Eviction may keep using the ledger as its working state; the state published
// here keys the alarm on actual disk usage so a policy that is losing still
// leaves a signal while there is disk left.
//
// Counters are cumulative for the lifetime of the file; derive rates by
// differencing successive snapshots. Resident gauges refresh whenever an
// eviction pass scans the cache; lookup counters refresh on every lookup.
const (
	blobCacheStatsVersion = 1
	blobCacheStatsName    = "stats.json"

	blobCacheStateDisabled  = "disabled"
	blobCacheStateOK        = "ok"
	blobCacheStateWarn      = "warn"
	blobCacheStateExhausted = "exhausted"
)

type blobCacheStats struct {
	Version int `json:"version"`
	// UpdatedAt is the time this snapshot was persisted.
	UpdatedAt time.Time `json:"updated_at"`
	// State is disabled, ok, warn, or exhausted. Warn means allocated disk
	// bytes exceed the alarm watermark; exhausted means a pass could not meet
	// the hard byte, file-count, or free-space budget because live device
	// files could not be evicted.
	State       string                 `json:"state"`
	BudgetBytes int64                  `json:"budget_bytes"`
	Watermarks  blobCacheStatWatermark `json:"watermarks"`
	Cache       blobCacheStatUsage     `json:"cache"`
	Counters    blobCacheStatCounters  `json:"counters"`
	LastTrim    *blobCacheStatTrim     `json:"last_trim,omitempty"`
}

type blobCacheStatWatermark struct {
	TriggerBytes int64 `json:"trigger_bytes"`
	TargetBytes  int64 `json:"target_bytes"`
	WarnBytes    int64 `json:"warn_bytes"`
}

type blobCacheStatUsage struct {
	Entries     int   `json:"entries"`
	Bytes       int64 `json:"bytes"`
	DiskBytes   int64 `json:"disk_bytes"`
	PinnedCount int   `json:"pinned_entries"`
	PinnedBytes int64 `json:"pinned_bytes"`
	// LedgerDiskDeltaBytes is ledger bytes minus allocated disk bytes, the
	// reconciliation term between policy state and disk truth.
	LedgerDiskDeltaBytes int64 `json:"ledger_disk_delta_bytes"`
	UsagePercent         int   `json:"usage_percent"`
	DiskUsagePercent     int   `json:"disk_usage_percent"`
}

type blobCacheStatCounters struct {
	Admissions       int64 `json:"admissions_total"`
	AdmissionBytes   int64 `json:"admission_bytes_total"`
	Evictions        int64 `json:"evictions_total"`
	EvictedBytes     int64 `json:"evicted_bytes_total"`
	EvictionPasses   int64 `json:"eviction_passes_total"`
	EvictionFailures int64 `json:"eviction_failures_total"`
	LookupHits       int64 `json:"lookup_hits_total"`
	LookupMisses     int64 `json:"lookup_misses_total"`
}

type blobCacheStatTrim struct {
	At             time.Time `json:"at"`
	DurationMillis int64     `json:"duration_ms"`
	EvictedEntries int       `json:"evicted_entries"`
	EvictedBytes   int64     `json:"evicted_bytes"`
	// ReachedTarget reports whether a triggered pass drove ledger usage at or
	// below the target watermark. It stays true for passes that did not
	// trigger eviction.
	ReachedTarget bool `json:"reached_target"`
}

func blobCacheStatsPath() string {
	return filepath.Join(blobCacheRoot(), blobCacheStatsName)
}

// loadBlobCacheStats returns the last snapshot, or an empty one when the file
// is absent, corrupt, or from another schema version. The cache is best effort
// and may be lost on node failure, so counters restart rather than fail a
// cache operation.
func loadBlobCacheStats() blobCacheStats {
	var stats blobCacheStats
	raw, err := os.ReadFile(blobCacheStatsPath())
	if err != nil || json.Unmarshal(raw, &stats) != nil || stats.Version != blobCacheStatsVersion {
		return blobCacheStats{Version: blobCacheStatsVersion}
	}
	return stats
}

// storeBlobCacheStats replaces the snapshot atomically so a crash cannot leave
// a half-written file behind.
func storeBlobCacheStats(stats blobCacheStats) error {
	root := blobCacheRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(root, ".stats-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after a successful rename
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, blobCacheStatsPath())
}

// updateBlobCacheStats applies mutate to the latest snapshot and persists it
// best effort. Observability must never fail an admission or a lookup.
func updateBlobCacheStats(mutate func(*blobCacheStats)) {
	stats := loadBlobCacheStats()
	mutate(&stats)
	stats.Version = blobCacheStatsVersion
	stats.UpdatedAt = time.Now().UTC()
	_ = storeBlobCacheStats(stats)
}

func blobCacheUsagePercent(bytes, budget int64) int {
	if budget <= 0 {
		return 0
	}
	return int(bytes * 100 / budget)
}

func blobCacheUsageSnapshot(scan *blobCacheScan, budget int64) blobCacheStatUsage {
	return blobCacheStatUsage{
		Entries:              scan.count,
		Bytes:                scan.bytes,
		DiskBytes:            scan.diskBytes,
		PinnedCount:          scan.pinnedCount,
		PinnedBytes:          scan.pinnedBytes,
		LedgerDiskDeltaBytes: scan.bytes - scan.diskBytes,
		UsagePercent:         blobCacheUsagePercent(scan.bytes, budget),
		DiskUsagePercent:     blobCacheUsagePercent(scan.diskBytes, budget),
	}
}

// blobCacheState selects the published state after a pass. The alarm gauge is
// allocated disk usage, so ledger overcounts cannot raise a false alarm and
// orphaned bytes cannot hide behind a ledger undercount.
func blobCacheState(scan *blobCacheScan, watermarks blobCacheWatermarks, budget int64, hardOK bool) string {
	switch {
	case !hardOK:
		return blobCacheStateExhausted
	case budget <= 0:
		return blobCacheStateDisabled
	case scan.diskBytes > watermarks.warn:
		return blobCacheStateWarn
	default:
		return blobCacheStateOK
	}
}

func recordBlobCacheTrim(scan *blobCacheScan, evictedEntries int, evictedBytes int64, started time.Time, reachedTarget bool, state string) {
	budget := global.blobCacheMaxBytes
	watermarks := global.blobCacheWatermarks()
	updateBlobCacheStats(func(stats *blobCacheStats) {
		stats.State = state
		stats.BudgetBytes = budget
		stats.Watermarks = blobCacheStatWatermark{
			TriggerBytes: watermarks.trigger,
			TargetBytes:  watermarks.target,
			WarnBytes:    watermarks.warn,
		}
		stats.Cache = blobCacheUsageSnapshot(scan, budget)
		stats.Counters.EvictionPasses++
		stats.Counters.Evictions += int64(evictedEntries)
		stats.Counters.EvictedBytes += evictedBytes
		if state == blobCacheStateExhausted {
			stats.Counters.EvictionFailures++
		}
		stats.LastTrim = &blobCacheStatTrim{
			At:             started.UTC(),
			DurationMillis: time.Since(started).Milliseconds(),
			EvictedEntries: evictedEntries,
			EvictedBytes:   evictedBytes,
			ReachedTarget:  reachedTarget,
		}
	})
}
