package firecrackerconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
)

func TestRootfsBootUsesWritebackAndNoSavedCPUState(t *testing.T) {
	m := artifacts.VMManifest{Machine: artifacts.VMMachine{VCPUCount: 2, MemoryMiB: 512, RootfsDriveID: "rootfs"}, Kernel: artifacts.VMKernel{Cmdline: "root=/dev/vda rw"}}
	ops, err := RootfsBoot(m, "/kernel", "rootfs.dev", "", "")
	require.NoError(t, err)
	machine, err := json.Marshal(ops[1].Body)
	require.NoError(t, err)
	require.NotContains(t, string(machine), "cpu_template")
	drive, err := json.Marshal(ops[2].Body)
	require.NoError(t, err)
	require.Contains(t, string(drive), `"cache_type":"Writeback"`)
	for _, op := range ops {
		require.NotContains(t, op.Path, "snapshot")
	}
	_, err = RootfsBoot(m, "/kernel", "rootfs.dev", "tap0", "")
	require.Error(t, err)
}

func TestResolveKernelDefaultBuilderFilename(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"vmlinux.bin", "vmlinux-6.1.0", "custom-kernel"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("kernel"), 0600))
	}
	for _, test := range []struct{ version, override, filename string }{
		{"vmlinux.bin", "", "vmlinux.bin"},
		{"6.1.0", "", "vmlinux-6.1.0"},
		{"vmlinux.bin", filepath.Join(dir, "custom-kernel"), "custom-kernel"},
	} {
		got, err := ResolveKernel(dir, test.version, test.override)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, test.filename), got)
	}
	_, err := ResolveKernel(t.TempDir(), "vmlinux.bin", "")
	require.ErrorContains(t, err, "not found")
}

func TestResolveKernelMakesRelativeOverrideAbsolute(t *testing.T) {
	dir := t.TempDir()
	relativeDir := filepath.Join(dir, "kernels")
	require.NoError(t, os.MkdirAll(relativeDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(relativeDir, "custom-kernel"), []byte("kernel"), 0o600))

	oldWorkingDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldWorkingDir)) })

	got, err := ResolveKernel("/unused", "vmlinux.bin", filepath.Join("kernels", "custom-kernel"))
	require.NoError(t, err)
	expected, err := filepath.Abs(filepath.Join("kernels", "custom-kernel"))
	require.NoError(t, err)
	require.Equal(t, expected, got)
	require.True(t, filepath.IsAbs(got))
}

func TestResolveKernelRejectsMetadataTraversal(t *testing.T) {
	for _, version := range []string{"../other", "../../../../etc/passwd", "/etc/passwd", ".", "..", ""} {
		if _, err := ResolveKernel(t.TempDir(), version, ""); err == nil {
			t.Fatalf("accepted kernel version %q", version)
		}
	}
	if _, err := ResolveKernel(t.TempDir(), "6.1.177", t.TempDir()); err == nil {
		t.Fatal("accepted directory as kernel")
	}
}
