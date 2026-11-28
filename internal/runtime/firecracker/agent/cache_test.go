package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFileMatchesSizeFastPath verifies the stat-first size check: a file
// with the wrong size is rejected without reading (the fast path), a file
// with the right size but wrong content is rejected by the digest, and an
// exact match passes.
func TestFileMatchesSizeFastPath(t *testing.T) {
	content := bytes.Repeat([]byte{0xab}, 1<<20)
	file := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(content), sizeBytes: int64(len(content))}
	path := filepath.Join(t.TempDir(), "rootfs.img")

	// Missing file.
	match, err := fileMatches(path, file)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.False(t, match)

	// Wrong size: rejected by stat, no content read needed.
	require.NoError(t, os.WriteFile(path, []byte("tiny"), cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match)

	// Right size, wrong content: caught by the digest.
	wrong := bytes.Repeat([]byte{0x00}, len(content))
	require.NoError(t, os.WriteFile(path, wrong, cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match)

	// Exact match.
	require.NoError(t, os.WriteFile(path, content, cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
}

// TestCacheComplete checks the committed-pull predicate: any missing,
// resized, or corrupted file (or a missing manifest) invalidates the cache.
func TestCacheComplete(t *testing.T) {
	artifacts := map[string][]byte{
		"rootfs.ext4":  bytes.Repeat([]byte{0xab}, 1<<16),
		"vmstate.snap": []byte("vmstate"),
		"memory.snap":  []byte("memory"),
	}
	manifestPayload := testManifest(artifacts)
	document, err := parseManifest(manifestPayload)
	require.NoError(t, err)
	files, err := document.nativeFiles()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))

	// No manifest: not complete.
	complete, err := cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// Manifest but no files: not complete.
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), manifestPayload, cacheFileMode))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// A file with the wrong size: not complete (fast path).
	for _, file := range files[:1] {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.cache), []byte("tiny"), cacheFileMode))
	}
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// All files exact: complete.
	for _, file := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.cache), artifacts[file.publish], cacheFileMode))
	}
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.True(t, complete)

	// Any single corrupt file breaks the commit.
	require.NoError(t, os.WriteFile(filepath.Join(dir, files[1].cache), []byte("tampered"), cacheFileMode))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)
}

func TestFileMatchesVerifiedDigestCache(t *testing.T) {
	content := bytes.Repeat([]byte{0x5a}, 64<<10)
	file := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(content), sizeBytes: int64(len(content))}
	path := filepath.Join(t.TempDir(), "rootfs.img")

	// A freshly written file whose mtime has not settled past the coarse-tick
	// guard is not memoized, so immediate same-tick mutations cannot hide.
	require.NoError(t, os.WriteFile(path, content, cacheFileMode))
	before := fileMatchesHashCount.Load()
	match, err := fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
	require.Equal(t, before+1, fileMatchesHashCount.Load())

	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
	require.Equal(t, before+2, fileMatchesHashCount.Load())

	// Once committed with PrepareCommittedCacheFile (backdated mtime), the
	// first verified hash populates the stat-identity cache and subsequent
	// checks skip full-file hashing.
	PrepareCommittedCacheFile(path)
	before = fileMatchesHashCount.Load()
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
	require.Equal(t, before+1, fileMatchesHashCount.Load())

	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
	require.Equal(t, before+1, fileMatchesHashCount.Load(), "warm hit must not re-hash unchanged file")

	// Even an immediate same-size in-place WriteAt advances mtime away from
	// the backdated value and invalidates the cache deterministically.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{content[0] ^ 0xff}, 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match, "same-size in-place corruption must invalidate verified digest cache")

	// RecordVerifiedCacheFile pre-warms the cache from the verified open
	// descriptor after rename so the very first check hits without hashing.
	tmpFile, err := os.Create(path + ".replacement")
	require.NoError(t, err)
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Chmod(cacheFileMode))
	PrepareCommittedCacheFile(tmpFile.Name())
	require.NoError(t, os.Rename(tmpFile.Name(), path))
	RecordVerifiedCacheFile(path, tmpFile, file.sha256)
	require.NoError(t, tmpFile.Close())

	before = fileMatchesHashCount.Load()
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
	require.Equal(t, before, fileMatchesHashCount.Load(), "pre-recorded committed file must hit without hashing")
}

// TestRecordVerifiedCacheFileInterleavedReplacementNeverHits verifies that
// interleaving a same-size replacement between rename and digest recording
// cannot bind the old SHA to the replacement inode.
func TestRecordVerifiedCacheFileInterleavedReplacementNeverHits(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rootfs.img")
	contentA := bytes.Repeat([]byte{0xaa}, 64<<10)
	contentB := bytes.Repeat([]byte{0xbb}, 64<<10)
	fileA := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(contentA), sizeBytes: int64(len(contentA))}
	fileB := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(contentB), sizeBytes: int64(len(contentB))}

	// Writer A stages and renames inode A into target while keeping its fd open.
	tmpA, err := os.Create(filepath.Join(dir, "rootfs.tmp-a"))
	require.NoError(t, err)
	defer tmpA.Close()
	_, err = tmpA.Write(contentA)
	require.NoError(t, err)
	PrepareCommittedCacheFile(tmpA.Name())
	require.NoError(t, os.Rename(tmpA.Name(), target))

	// Writer B concurrently replaces target with a same-size inode B before
	// Writer A records its verified digest.
	tmpB, err := os.Create(filepath.Join(dir, "rootfs.tmp-b"))
	require.NoError(t, err)
	_, err = tmpB.Write(contentB)
	require.NoError(t, err)
	PrepareCommittedCacheFile(tmpB.Name())
	require.NoError(t, os.Rename(tmpB.Name(), target))
	require.NoError(t, tmpB.Close())

	// Writer A records against its own verified open fd (inode A).
	RecordVerifiedCacheFile(target, tmpA, fileA.sha256)

	// Target currently holds inode B, so checking against manifest A must miss
	// the cache, hash inode B, and reject it.
	before := fileMatchesHashCount.Load()
	match, err := fileMatches(target, fileA)
	require.NoError(t, err)
	require.False(t, match, "replacement inode B must never hit cached digest for A")
	require.Equal(t, before+1, fileMatchesHashCount.Load())

	// Checking against manifest B succeeds.
	match, err = fileMatches(target, fileB)
	require.NoError(t, err)
	require.True(t, match)
}

func TestTrackWrittenFileProofVoidedByLaterWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.snap")
	file, err := os.Create(path)
	require.NoError(t, err)
	_, err = file.WriteString("dump")
	require.NoError(t, err)
	proof, err := TrackWrittenFile(file, "digest-of-dump")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, "digest-of-dump", proof.SHA256())

	reopened, err := os.Open(path)
	require.NoError(t, err)
	require.True(t, proof.MatchesFile(reopened))
	require.NoError(t, reopened.Close())

	// Make sure the next write lands on a later timestamp tick.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("DUMP"), 0o640))
	reopened, err = os.Open(path)
	require.NoError(t, err)
	defer reopened.Close()
	require.False(t, proof.MatchesFile(reopened), "a write after the proof voids it")
}
