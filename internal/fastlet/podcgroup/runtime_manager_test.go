package podcgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRuntimeManagerBoundsAggregateAndSandboxLeaves(t *testing.T) {
	root := t.TempDir()
	podRelative := filepath.Join("kubepods", "burstable", "pod"+testPodUID)
	podPath := filepath.Join(root, podRelative)
	aggregatePath := filepath.Join(podPath, "fast-sandbox")
	require.NoError(t, os.MkdirAll(aggregatePath, 0o755))
	prepareFakeV2Controls(t, root)
	prepareFakeV2Controls(t, podPath)
	prepareFakeV2Controls(t, aggregatePath)
	require.NoError(t, os.WriteFile(filepath.Join(aggregatePath, "memory.oom.group"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(aggregatePath, "memory.swap.max"), nil, 0o600))

	manager, err := NewRuntimeManager(root, testPodUID, RuntimeBudget{
		CPU: resource.MustParse("2"), Memory: resource.MustParse("1Gi"),
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare())
	require.Equal(t, "/"+filepath.ToSlash(filepath.Join(podRelative, "fast-sandbox")), manager.AggregatePath())
	requireControl(t, aggregatePath, "cpu.max", "200000 100000")
	requireControl(t, aggregatePath, "memory.max", "1073741824")
	requireControl(t, aggregatePath, "memory.swap.max", "0")
	requireControl(t, aggregatePath, "memory.oom.group", "0")

	first, err := manager.EnsureSandbox("sandbox-a", SandboxLimits{
		CPU: resource.MustParse("2"), CPURequest: resource.MustParse("250m"), PIDs: 128,
	})
	require.NoError(t, err)
	second, err := manager.EnsureSandbox("sandbox-b", SandboxLimits{
		CPU: resource.MustParse("2"), CPURequest: resource.MustParse("500m"), PIDs: 128,
	})
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	for _, leaf := range []string{first, second} {
		requireControl(t, leaf, "cpu.max", "200000 100000")
		requireControl(t, leaf, "memory.max", "max")
		requireControl(t, leaf, "pids.max", "128")
	}
	require.NoError(t, os.WriteFile(filepath.Join(first, "cgroup.events"), []byte("populated 1\n"), 0o600))
	require.Error(t, manager.ConfirmAbsent("sandbox-a"))
	require.NoError(t, os.WriteFile(filepath.Join(first, "cgroup.events"), []byte("populated 0\n"), 0o600))
	require.NoError(t, manager.ConfirmAbsent("sandbox-a"))
	require.NoError(t, os.WriteFile(filepath.Join(first, "cgroup.procs"), nil, 0o600))
	pids, exists, err := manager.SandboxProcesses("sandbox-a")
	require.NoError(t, err)
	require.True(t, exists)
	require.Empty(t, pids)
	require.NoError(t, os.WriteFile(filepath.Join(first, "cgroup.procs"), []byte("17\n23\n"), 0o600))
	pids, exists, err = manager.SandboxProcesses("sandbox-a")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, []int{17, 23}, pids)
	_, exists, err = manager.SandboxProcesses("missing")
	require.NoError(t, err)
	require.False(t, exists)
	requireControl(t, first, "cpu.weight", "10")
	requireControl(t, second, "cpu.weight", "20")
}

func TestRuntimeManagerRejectsMissingControllersAndCgroupV1(t *testing.T) {
	root := t.TempDir()
	podPath := filepath.Join(root, "kubepods", "pod"+testPodUID)
	require.NoError(t, os.MkdirAll(podPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(podPath, "cgroup.controllers"), []byte("cpu memory"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(podPath, "cgroup.subtree_control"), nil, 0o600))
	manager, err := NewRuntimeManager(root, testPodUID, RuntimeBudget{
		CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"),
	})
	require.NoError(t, err)
	require.ErrorContains(t, manager.Prepare(), "pids")

	v1Root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(v1Root, "memory", "kubepods", "pod"+testPodUID), 0o755))
	_, err = NewRuntimeManager(v1Root, testPodUID, RuntimeBudget{
		CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"),
	})
	require.ErrorContains(t, err, "requires cgroup v2")
}

func TestDisableSwapWithoutPerCgroupControl(t *testing.T) {
	dir := t.TempDir()
	swaps := filepath.Join(dir, "swaps")
	header := "Filename Type Size Used Priority\n"
	require.NoError(t, os.WriteFile(swaps, []byte(header), 0o600))
	require.NoError(t, disableSwap(dir, swaps))
	require.NoError(t, os.WriteFile(swaps, []byte(header+"/dev/sda2 partition 65536 0 -2\n"), 0o600))
	require.ErrorContains(t, disableSwap(dir, swaps), "swap is available")
}

func prepareFakeV2Controls(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(path, "cgroup.controllers"), []byte("cpu memory pids"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(path, "memory.swap.max"), nil, 0o600))
}

func requireControl(t *testing.T, path, name, expected string) {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(path, name))
	require.NoError(t, err)
	require.Equal(t, expected, strings.TrimSpace(string(value)))
}
