package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
		got, err := resolveKernel(dir, test.version, test.override)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, test.filename), got)
	}
	_, err := resolveKernel(t.TempDir(), "vmlinux.bin", "")
	require.ErrorContains(t, err, "not found")
}

func TestResolveKernelRejectsMetadataTraversal(t *testing.T) {
	for _, version := range []string{"../other", "../../../../etc/passwd", "/etc/passwd", ".", "..", ""} {
		if _, err := resolveKernel(t.TempDir(), version, ""); err == nil {
			t.Fatalf("accepted kernel version %q", version)
		}
	}
	if _, err := resolveKernel(t.TempDir(), "6.1.177", t.TempDir()); err == nil {
		t.Fatal("accepted directory as kernel")
	}
}
