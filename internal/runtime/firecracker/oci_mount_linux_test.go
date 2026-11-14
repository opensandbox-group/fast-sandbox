//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOCIInstanceBindsSnapshotSpillBeforeLaunch(t *testing.T) {
	if os.Getenv("FSB_TEST_MOUNTS") != "1" {
		t.Skip("requires a privileged Linux mount namespace")
	}
	stateRoot := t.TempDir()
	driver := &Driver{config: firecrackerConfigForTest(t, stateRoot)}
	driver.config.JailerPath = "/usr/bin/jailer"
	vmmBinary := filepath.Join(stateRoot, "bin", "firecracker")
	require.NoError(t, os.MkdirAll(filepath.Dir(vmmBinary), 0o750))
	require.NoError(t, os.WriteFile(vmmBinary, []byte("vmm"), 0o750))
	spill := t.TempDir()
	t.Setenv("FAST_SANDBOX_SNAPSHOT_SPILL_DIR", spill)
	require.NoError(t, os.WriteFile(filepath.Join(spill, "visible-before-launch"), []byte("spill"), 0600))
	files := make([]string, 3)
	for i, name := range []string{"rootfs", "vmstate", "memory"} {
		files[i] = filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(files[i], []byte(name), 0600))
	}
	_, jail, _, err := driver.prepareOCIInstance("test-oci-spill", t.TempDir(), files[0], files[1], files[2], vmmBinary)
	require.NoError(t, err)
	defer driver.removeJailRoot("test-oci-spill")
	raw, err := os.ReadFile(filepath.Join(jail, jailerSpillDirName, "visible-before-launch"))
	require.NoError(t, err)
	require.Equal(t, "spill", string(raw))
}
