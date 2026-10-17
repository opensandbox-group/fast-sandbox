package resourceadmission

import (
	"os"
	"path/filepath"
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
	require.Equal(t, 0.25, sample.NodeAvailableRatio)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
