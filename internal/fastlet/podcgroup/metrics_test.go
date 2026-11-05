package podcgroup

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestRuntimeMetricsExposeUnlimitedLeafAndPSI(t *testing.T) {
	root := t.TempDir()
	manager := &RuntimeManager{root: root, layout: Layout{PodPath: "/pod"}, leaves: make(map[string]string)}
	pool := manager.AggregateFilesystemPath()
	leaf := filepath.Join(pool, "sandbox-a")
	manager.leaves["sandbox-a"] = leaf
	require.NoError(t, os.MkdirAll(leaf, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pool, "memory.max"), []byte("1024\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "memory.max"), []byte("max\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "memory.pressure"), []byte("some avg10=0.25 avg60=0.10 avg300=0.05 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"), 0o600))
	registry := prometheus.NewRegistry()
	require.NoError(t, manager.RegisterMetrics(registry))
	families, err := registry.Gather()
	require.NoError(t, err)
	samples := make(map[string]map[string]float64)
	for _, family := range families {
		if family.GetName() != "fast_sandbox_runtime_cgroup_memory_limit_bytes" && family.GetName() != "fast_sandbox_runtime_cgroup_memory_psi_some_avg10" {
			continue
		}
		samples[family.GetName()] = make(map[string]float64)
		for _, metric := range family.Metric {
			require.Len(t, metric.Label, 1)
			require.Equal(t, "sandbox_uid", metric.Label[0].GetName())
			samples[family.GetName()][metric.Label[0].GetValue()] = metric.GetGauge().GetValue()
		}
	}
	limits := samples["fast_sandbox_runtime_cgroup_memory_limit_bytes"]
	require.Len(t, limits, 2, "both the bounded pool and unlimited leaf must report their limits")
	require.Equal(t, float64(1024), limits[""])
	require.True(t, math.IsInf(limits["sandbox-a"], 1))
	require.Equal(t, map[string]float64{"sandbox-a": 0.25}, samples["fast_sandbox_runtime_cgroup_memory_psi_some_avg10"])
}
