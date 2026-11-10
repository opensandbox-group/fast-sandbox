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
			return trimBlobCache(global.blobCacheMaxBytes, blobCacheMaxFiles, minFree)
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
	if err := trimBlobCache(global.blobCacheMaxBytes-size, blobCacheMaxFiles-1, blobCacheMinFreeBytes); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("retain checkpoint blob: %w", err)
	}
	now := time.Now()
	return os.Chtimes(target, now, now)
}

// Cache names remain while a device owns a hard link, so their bytes continue
// to count against the budget. A busy cache rejects new admission rather than
// unlinking live files whose disk space would not actually be reclaimed.
// Callers hold the cache lock, shared with pin creation and other publishers.
func trimBlobCache(maxBytes int64, maxFiles int, minFree uint64) error {
	root := filepath.Join(blobCacheRoot(), "sha256")
	items, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type entry struct {
		path string
		info os.FileInfo
	}
	var entries []entry
	var total int64
	for _, item := range items {
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		entries = append(entries, entry{filepath.Join(root, item.Name()), info})
		total += info.Size()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].info.ModTime().Before(entries[j].info.ModTime()) })
	count := len(entries)
	withinBudget := func() (bool, error) {
		var stat unix.Statfs_t
		if err := unix.Statfs(root, &stat); err != nil {
			return false, err
		}
		return total <= maxBytes && count <= maxFiles && stat.Bavail*uint64(stat.Bsize) >= minFree, nil
	}
	for _, item := range entries {
		ok, err := withinBudget()
		if err != nil || ok {
			return err
		}
		// Links are created only under the same lock; teardown may remove a
		// link concurrently, making this snapshot conservative for one pass.
		if item.info.Sys().(*syscall.Stat_t).Nlink > 1 {
			continue
		}
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= item.info.Size()
		count--
	}
	ok, err := withinBudget()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("local blob cache budget or free-disk reserve exhausted; live device files retained")
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
	for i := range lowers {
		lower := &lowers[i]
		sum := digest.Digest(lower.Digest)
		if sum.Validate() != nil || sum.Algorithm() != digest.SHA256 {
			continue
		}
		source := blobCachePath(sum)
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() || info.Size() != lower.Size {
			continue
		}
		pin := filepath.Join(home, ".local-blobs", sum.Encoded())
		if err := os.MkdirAll(filepath.Dir(pin), 0700); err != nil {
			continue
		}
		if err := os.Link(source, pin); err != nil {
			existing, statErr := os.Lstat(pin)
			if statErr != nil || !os.SameFile(info, existing) {
				continue
			}
		}
		lower.File = pin
		now := time.Now()
		_ = os.Chtimes(source, now, now)
	}
	return lowers
}
