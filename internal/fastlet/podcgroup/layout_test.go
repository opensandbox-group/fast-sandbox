package podcgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testPodUID = "873c13da-c645-461e-93f0-dfc24f63a6ad"

func TestDiscoverV2Cgroupfs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "kubepods", "burstable", "pod"+testPodUID), 0o755))

	layout, err := Discover(root, testPodUID)
	require.NoError(t, err)
	require.Equal(t, Layout{Version: VersionV2, PodPath: "/kubepods/burstable/pod" + testPodUID}, layout)

	path, err := layout.SandboxPath("sandbox/a")
	require.NoError(t, err)
	require.Equal(t, "/kubepods/burstable/pod"+testPodUID+"/fast-sandbox/fsb-34b27753b28275d8", path)
	require.Equal(t, layout.PodPath+"/fast-sandbox-shims", layout.ShimPath())
	require.NoError(t, layout.EnsureShimGroup(root))
	_, err = os.Stat(filepath.Join(root, "kubepods", "burstable", "pod"+testPodUID, "fast-sandbox-shims"))
	require.NoError(t, err)
}

func TestDiscoverV2Systemd(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids"), 0o600))
	uid := "873c13da_c645_461e_93f0_dfc24f63a6ad"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod"+uid+".slice"), 0o755))

	layout, err := Discover(root, testPodUID)
	require.NoError(t, err)
	require.True(t, layout.Systemd)
	require.Equal(t, VersionV2, layout.Version)

	path, err := layout.SandboxSystemdPath("sandbox-a")
	require.NoError(t, err)
	require.Equal(t, "kubepods-burstable-pod"+uid+".slice:fast-sandbox:fsb-315bbe38938f7266", path)

	filesystemPath, err := layout.SandboxPath("sandbox-a")
	require.NoError(t, err)
	require.Equal(t,
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+uid+".slice/fast-sandbox/fsb-315bbe38938f7266",
		filesystemPath,
	)
}

func TestRemoveSandboxGroupsRemovesPlainAndKataLeaves(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids"), 0o600))
	layout := Layout{Version: VersionV2, PodPath: "/kubepods/burstable/pod" + testPodUID}
	path, err := layout.SandboxPath("sandbox-a")
	require.NoError(t, err)
	plain := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/")))
	kata := filepath.Join(filepath.Dir(plain), "kata_"+filepath.Base(plain))
	require.NoError(t, os.MkdirAll(plain, 0o755))
	require.NoError(t, os.MkdirAll(kata, 0o755))

	require.NoError(t, layout.RemoveSandboxGroups(root, "sandbox-a"))
	_, err = os.Stat(plain)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(kata)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestVerifySandboxProcessesAbsentFindsSystemdScope(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.procs"), nil, 0600))
	pod := filepath.Join(root, "kubepods.slice", "kubepods-pod"+strings.ReplaceAll(testPodUID, "-", "_")+".slice")
	leaf := filepath.Join(pod, "fast-sandbox-"+sandboxCgroupName("sandbox-a")+".scope")
	require.NoError(t, os.MkdirAll(leaf, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "cgroup.events"), []byte("populated 1\n"), 0600))
	require.ErrorContains(t, VerifySandboxProcessesAbsent(root, testPodUID, "sandbox-a", ""), "populated")
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "cgroup.events"), []byte("populated 0\n"), 0600))
	require.NoError(t, VerifySandboxProcessesAbsent(root, testPodUID, "sandbox-a", ""))
	require.NoError(t, os.RemoveAll(pod))
	literal := filepath.Join(root, "system.slice", filepath.Base(pod)+":fast-sandbox:"+sandboxCgroupName("sandbox-a"))
	require.NoError(t, os.MkdirAll(literal, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(literal, "cgroup.events"), []byte("populated 1\n"), 0600))
	require.ErrorContains(t, VerifySandboxProcessesAbsent(root, testPodUID, "sandbox-a", ""), "populated")
}

func TestVerifySandboxProcessesAbsentChecksV1Children(t *testing.T) {
	root := t.TempDir()
	hierarchy := filepath.Join(root, "pids")
	leaf := filepath.Join(hierarchy, "k8s.io", "sandbox-a")
	child := filepath.Join(leaf, "child")
	require.NoError(t, os.MkdirAll(child, 0755))
	for _, group := range []string{hierarchy, leaf} {
		require.NoError(t, os.WriteFile(filepath.Join(group, "cgroup.procs"), nil, 0600))
	}
	file := filepath.Join(child, "cgroup.procs")
	require.NoError(t, os.WriteFile(file, []byte("123\n"), 0600))
	require.ErrorContains(t, VerifySandboxProcessesAbsent(root, "", "sandbox-a", "/k8s.io/sandbox-a"), "still contains processes")
	require.NoError(t, os.WriteFile(file, nil, 0600))
	require.NoError(t, VerifySandboxProcessesAbsent(root, "", "sandbox-a", "/k8s.io/sandbox-a"))
}

func TestDiscoverV1AcrossControllers(t *testing.T) {
	root := t.TempDir()
	relative := filepath.Join("kubepods", "burstable", "pod"+testPodUID)
	for _, controller := range []string{"cpuset,cpu,cpuacct", "memory", "pids"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, controller, relative), 0o755))
	}

	layout, err := Discover(root, testPodUID)
	require.NoError(t, err)
	require.Equal(t, VersionV1, layout.Version)
	require.Equal(t, "/"+filepath.ToSlash(relative), layout.PodPath)
	require.NoError(t, layout.EnsureShimGroup(root))
	for _, controller := range []string{"cpuset,cpu,cpuacct", "memory", "pids"} {
		_, err = os.Stat(filepath.Join(root, controller, relative, "fast-sandbox-shims"))
		require.NoError(t, err)
	}
}

func TestDiscoverFallbackScansProviderPrefix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu"), 0o600))
	uid := "873c13da_c645_461e_93f0_dfc24f63a6ad"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "provider.slice", "kubelet-kubepods-burstable-pod"+uid+".slice"), 0o755))

	layout, err := Discover(root, testPodUID)
	require.NoError(t, err)
	require.Equal(t, "/provider.slice/kubelet-kubepods-burstable-pod"+uid+".slice", layout.PodPath)
	require.True(t, layout.Systemd)
}

func TestDiscoverRejectsInvalidOrAmbiguousIdentity(t *testing.T) {
	_, err := Discover(t.TempDir(), "../pod")
	require.ErrorContains(t, err, "invalid character")

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu"), 0o600))
	target := "pod" + testPodUID
	require.NoError(t, os.MkdirAll(filepath.Join(root, "one", target), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "two", target), 0o755))
	_, err = Discover(root, testPodUID)
	require.ErrorContains(t, err, "multiple cgroups")
}
