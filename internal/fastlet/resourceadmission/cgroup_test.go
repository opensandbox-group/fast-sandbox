package resourceadmission

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCgroupReaderReadsV2MemoryAndNodeSignals(t *testing.T) {
	root := t.TempDir()
	procCgroup := filepath.Join(root, "self-cgroup")
	cgroupRoot := filepath.Join(root, "cgroup")
	cgroupPath := filepath.Join(cgroupRoot, "kubepods", "pod-a")
	meminfo := filepath.Join(root, "meminfo")
	require.NoError(t, os.MkdirAll(cgroupPath, 0o755))
	writeTestFile(t, procCgroup, "0::/kubepods/pod-a\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.current"), "734003200\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.max"), "max\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.events"), "low 0\nhigh 3\nmax 1\noom 2\noom_kill 1\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.pressure"), "some avg10=1.25 avg60=0.50 avg300=0.10 total=10\n")
	writeTestFile(t, meminfo, "MemTotal:       1000000 kB\nMemAvailable:    250000 kB\n")

	reader, err := NewCgroupReader(procCgroup, cgroupRoot, meminfo, 1024*1024*1024)
	require.NoError(t, err)
	sample, err := reader.Read()
	require.NoError(t, err)
	require.Equal(t, uint64(734003200), sample.MemoryCurrentBytes)
	require.Equal(t, uint64(1024*1024*1024), sample.MemoryLimitBytes)
	require.Equal(t, uint64(1), sample.OOMKills)
	require.Equal(t, 1.25, sample.MemoryPSIAvg10)
	require.Equal(t, 0.25, sample.HostAvailableRatio)
	require.True(t, sample.HostMemoryObserved)
}

func TestReadNodeAvailableRatioDistinguishesZeroFromMissing(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	writeTestFile(t, meminfo, "MemTotal:       1000000 kB\nMemAvailable:          0 kB\n")
	ratio, observed, err := readNodeAvailableRatio(meminfo)
	require.NoError(t, err)
	require.True(t, observed)
	require.Zero(t, ratio)

	writeTestFile(t, meminfo, "MemTotal:       1000000 kB\n")
	ratio, observed, err = readNodeAvailableRatio(meminfo)
	require.NoError(t, err)
	require.False(t, observed)
	require.Zero(t, ratio)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// Checkpoint and restore I/O leaves tens of GiB of clean, unmapped page cache in
// the Fastlet Pod cgroup (seen on bare metal: 31 GiB of 34 GiB). That cache is
// reclaimable and must not count; mapped VM memory, tmpfs spill and dirty or
// in-flight pages must.
func TestCgroupReaderExcludesReclaimableFileCache(t *testing.T) {
	const gib = uint64(1) << 30
	cgroupPath := t.TempDir()
	writeTestFile(t, filepath.Join(cgroupPath, "memory.current"), strconv.FormatUint(34*gib, 10)+"\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.max"), strconv.FormatUint(39*gib, 10)+"\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.events"), "oom_kill 0\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.stat"), fmt.Sprintf(
		"anon %d\nfile %d\nfile_mapped %d\nshmem %d\nfile_dirty %d\nfile_writeback %d\n",
		2*gib, 31*gib, 4*gib, 3*gib, gib, 0))
	reader, err := NewCgroupPathReader(cgroupPath, filepath.Join(cgroupPath, "no-meminfo"), 0)
	require.NoError(t, err)
	sample, err := reader.Read()
	require.NoError(t, err)
	// reclaimable = file - mapped - shmem - dirty = 31 - 4 - 3 - 1 = 23 GiB
	require.Equal(t, 11*gib, sample.MemoryCurrentBytes)

	// only cache: never below zero
	writeTestFile(t, filepath.Join(cgroupPath, "memory.current"), strconv.FormatUint(5*gib, 10)+"\n")
	writeTestFile(t, filepath.Join(cgroupPath, "memory.stat"), fmt.Sprintf("file %d\n", 6*gib))
	sample, err = reader.Read()
	require.NoError(t, err)
	require.Zero(t, sample.MemoryCurrentBytes)

	// no memory.stat: raw memory.current
	require.NoError(t, os.Remove(filepath.Join(cgroupPath, "memory.stat")))
	sample, err = reader.Read()
	require.NoError(t, err)
	require.Equal(t, 5*gib, sample.MemoryCurrentBytes)
}
