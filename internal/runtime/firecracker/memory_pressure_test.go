package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryPressurePolicyRejectsInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{"unknown":1}`, `{} {}`, `{"recoverySeconds":0}`,
		`{"cgroupHighPercent":70,"cgroupLowPercent":80}`, `{"maxPSISomeAvg10":101}`,
	} {
		t.Run(raw, func(t *testing.T) {
			require.Error(t, (&Driver{}).ConfigureMemoryPressure(context.Background(), raw))
		})
	}
}

func TestPSIPressureRequiresContinuousRecovery(t *testing.T) {
	now := time.Now()
	s := memoryPressureSample{available: 10000, current: 20, limit: 100, psi: 12}
	g := &memoryPressureGate{
		policy: MemoryPressurePolicy{NodeMinAvailableBytes: 1000, CgroupHighPercent: 90, CgroupLowPercent: 80, MaxPSISomeAvg10: 10, RecoverySeconds: 5},
		read:   func() (memoryPressureSample, error) { return s, nil },
	}
	g.sample(now)
	require.ErrorContains(t, g.check(now), "PSI")
	s.psi = 4
	g.sample(now.Add(time.Second))
	s.psi = 6 // below the block threshold but above the recovery threshold
	g.sample(now.Add(5 * time.Second))
	s.psi = 2
	g.sample(now.Add(10 * time.Second))
	g.sample(now.Add(14 * time.Second))
	require.Error(t, g.check(now.Add(14*time.Second)))
	g.sample(now.Add(15 * time.Second))
	require.NoError(t, g.check(now.Add(15*time.Second)))
}

func TestMemoryPressureHysteresisAndTelemetryFailure(t *testing.T) {
	now := time.Now()
	s := memoryPressureSample{available: 10000, current: 20, limit: 100}
	var readErr error
	g := &memoryPressureGate{
		policy: MemoryPressurePolicy{NodeMinAvailableBytes: 1000, CgroupHighPercent: 90, CgroupLowPercent: 80, MaxPSISomeAvg10: 10, RecoverySeconds: 5},
		read:   func() (memoryPressureSample, error) { return s, readErr },
	}
	g.sample(now)
	require.NoError(t, g.check(now))
	s.current = 95
	g.sample(now)
	require.ErrorContains(t, g.check(now), "high watermark")
	s.current = 85
	g.sample(now.Add(10 * time.Second))
	require.Error(t, g.check(now.Add(10*time.Second)))
	s.current = 75
	g.sample(now.Add(11 * time.Second))
	g.sample(now.Add(15 * time.Second))
	require.Error(t, g.check(now.Add(15*time.Second)))
	g.sample(now.Add(16 * time.Second))
	require.NoError(t, g.check(now.Add(16*time.Second)))
	require.ErrorContains(t, g.check(now.Add(20*time.Second)), "stale")
	readErr = errors.New("missing budget")
	g.sample(now.Add(21 * time.Second))
	require.ErrorContains(t, g.check(now.Add(21*time.Second)), "telemetry unavailable")
	readErr = nil
	s.available = 500
	g.sample(now.Add(22 * time.Second))
	require.ErrorContains(t, g.check(now.Add(22*time.Second)), "node available")
}

func TestMemoryPressureReadsOwningCgroupAndAncestors(t *testing.T) {
	root := t.TempDir()
	proc, cg := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	write := func(path, contents string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	}
	write(filepath.Join(proc, "meminfo"), "MemAvailable: 1234 kB\n")
	write(filepath.Join(proc, "pressure/memory"), "some avg10=2.50 avg60=0.00 total=1\nfull avg10=0.00\n")
	write(filepath.Join(proc, "self/cgroup"), "0::/pod/container\n")
	write(filepath.Join(cg, "pod/container/memory.current"), "20\n")
	write(filepath.Join(cg, "pod/container/memory.max"), "100\n")
	write(filepath.Join(cg, "pod/memory.current"), "180\n")
	write(filepath.Join(cg, "pod/memory.max"), "200\n")
	s, err := readMemoryPressureAt(proc, cg, true)
	require.NoError(t, err)
	require.Equal(t, uint64(1234*1024), s.available)
	require.Equal(t, uint64(180), s.current)
	require.Equal(t, uint64(200), s.limit)
	require.Equal(t, 2.5, s.psi)
	// Private cgroup namespace: the process path is not beneath the visible mount.
	write(filepath.Join(cg, "memory.current"), "30\n")
	write(filepath.Join(cg, "memory.max"), "100\n")
	write(filepath.Join(proc, "self/cgroup"), "0::/outside\n")
	s, err = readMemoryPressureAt(proc, cg, true)
	require.NoError(t, err)
	require.Equal(t, uint64(30), s.current)
	require.NoError(t, os.Remove(filepath.Join(cg, "memory.max")))
	_, err = readMemoryPressureAt(proc, cg, true)
	require.ErrorContains(t, err, "unavailable")
	write(filepath.Join(cg, "memory.max"), "100\n")
	require.NoError(t, os.Remove(filepath.Join(proc, "pressure/memory")))
	_, err = readMemoryPressureAt(proc, cg, true)
	require.Error(t, err, "required PSI must fail closed")
	_, err = readMemoryPressureAt(proc, cg, false)
	require.NoError(t, err, "explicitly disabled PSI does not require its interface")
}

func TestCgroupBudgetDoesNotEscapeVisibleMount(t *testing.T) {
	root := t.TempDir()
	proc, cg := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "self"), 0o755))
	require.NoError(t, os.MkdirAll(cg, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "self/cgroup"), []byte("0::/../foreign\n"), 0o600))
	// Even if an outside directory happens to contain cgroup-shaped files,
	// only the visible mount is a valid fallback.
	foreign := filepath.Join(root, "foreign")
	require.NoError(t, os.MkdirAll(foreign, 0o755))
	for path, value := range map[string]string{
		filepath.Join(cg, "memory.current"): "30", filepath.Join(cg, "memory.max"): "100",
		filepath.Join(foreign, "memory.current"): "95", filepath.Join(foreign, "memory.max"): "100",
	} {
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	current, limit, err := readCgroupBudget(proc, cg)
	require.NoError(t, err)
	require.Equal(t, uint64(30), current)
	require.Equal(t, uint64(100), limit)
}
