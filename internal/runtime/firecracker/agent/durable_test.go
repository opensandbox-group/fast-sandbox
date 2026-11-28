package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// durabilityProbe replaces the fsync primitives for one test and records
// what each commit synced and in which state the cache was at that moment.
type durabilityProbe struct {
	mu    sync.Mutex
	files []string // base names of synced files
	dirs  []string // synced directories
	// committedAtFileSync records, per synced file, whether its committed
	// cache name already existed when it was synced.
	committedAtFileSync map[string]bool
	// manifestAtDirSync records, per synced directory, whether the commit
	// point existed when the directory was synced.
	manifestAtDirSync map[string]bool
}

func installDurabilityProbe(t *testing.T) *durabilityProbe {
	t.Helper()
	probe := &durabilityProbe{committedAtFileSync: map[string]bool{}, manifestAtDirSync: map[string]bool{}}
	previousFile, previousDir := fsyncFile, fsyncDir
	fsyncFile = func(file *os.File) error {
		base := filepath.Base(file.Name())
		committed := strings.SplitN(base, ".tmp-", 2)[0]
		_, err := os.Stat(filepath.Join(filepath.Dir(file.Name()), committed))
		probe.mu.Lock()
		probe.files = append(probe.files, base)
		probe.committedAtFileSync[committed] = err == nil
		probe.mu.Unlock()
		return previousFile(file)
	}
	fsyncDir = func(dir string) error {
		_, err := os.Stat(filepath.Join(dir, manifestName))
		probe.mu.Lock()
		probe.dirs = append(probe.dirs, dir)
		probe.manifestAtDirSync[dir] = err == nil
		probe.mu.Unlock()
		return previousDir(dir)
	}
	t.Cleanup(func() { fsyncFile, fsyncDir = previousFile, previousDir })
	return probe
}

func TestPullImageCommitIsDurable(t *testing.T) {
	_, client, _, _ := publishFixture(t)
	probe := installDurabilityProbe(t)
	root := t.TempDir()

	require.NoError(t, (&Client{s3: client}).PullImage(context.Background(), root, testImage))

	dir := imageDir(root, testImage)
	// Every artifact and the manifest are fsynced through their temporary
	// file, before the committed name exists.
	for _, name := range []string{"rootfs.img", "vmstate.snap", "memory.snap", manifestName} {
		committed, synced := probe.committedAtFileSync[name]
		require.True(t, synced, "%s is fsynced before its commit", name)
		require.False(t, committed, "%s is fsynced before the rename commits it", name)
	}
	for _, base := range probe.files {
		require.Contains(t, base, ".tmp-", "only temporary files are synced")
	}
	// One directory fsync after the commit point persists every rename.
	require.Equal(t, []string{dir}, probe.dirs)
	require.True(t, probe.manifestAtDirSync[dir], "the directory is synced after the manifest rename")
}

func TestPullImageResumeSyncsOnlyNewFiles(t *testing.T) {
	_, client, _, artifacts := publishFixture(t)
	root := t.TempDir()
	dir := imageDir(root, testImage)
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))
	// rootfs.img survived an interrupted pull: the resume keeps it and only
	// syncs what it downloads, then commits the set with one directory sync.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rootfs.img"), artifacts["rootfs.ext4"], cacheFileMode))
	probe := installDurabilityProbe(t)

	require.NoError(t, (&Client{s3: client}).PullImage(context.Background(), root, testImage))

	_, resynced := probe.committedAtFileSync["rootfs.img"]
	require.False(t, resynced, "a matching file kept by the resume is not rewritten")
	for _, name := range []string{"vmstate.snap", "memory.snap", manifestName} {
		_, synced := probe.committedAtFileSync[name]
		require.True(t, synced, name)
	}
	require.Equal(t, []string{dir}, probe.dirs)
}

func TestPullImageFailsWhenCommitCannotSync(t *testing.T) {
	_, client, _, _ := publishFixture(t)
	root := t.TempDir()
	previousDir := fsyncDir
	fsyncDir = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { fsyncDir = previousDir })

	err := (&Client{s3: client}).PullImage(context.Background(), root, testImage)
	require.ErrorIs(t, err, os.ErrPermission, "a commit that cannot be made durable fails the pull")
}
