//go:build linux

package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestPlatformAlignJailKVMRebinds exercises the real mknod path: a jailer
// inode bound to a wrong misc minor is replaced by the host /dev/kvm device
// numbers, mirroring the vendor-kernel failure (issue #87). Skips on nodes
// without /dev/kvm and unprivileged environments without CAP_MKNOD.
func TestPlatformAlignJailKVMRebinds(t *testing.T) {
	hostInfo, err := os.Stat("/dev/kvm")
	if err != nil {
		t.Skip("requires /dev/kvm")
	}
	hostRdev, ok := charDeviceRdev(hostInfo)
	require.True(t, ok)

	jailRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(jailRoot, "dev"), 0o700))
	jailKVM := jailedKVMDevicePath(jailRoot)
	// Bind the inode to a misc minor that differs from the host (10:233
	// mirrors the jailer's hardcoded 10:232 without colliding on hosts
	// where KVM really is 232).
	if err := unix.Mknod(jailKVM, unix.S_IFCHR|0o600, int(unix.Mkdev(10, 233))); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skip("requires CAP_MKNOD")
		}
		require.NoError(t, err)
	}

	require.NoError(t, defaultAlignJailKVM("/dev/kvm", jailKVM))

	after, err := os.Stat(jailKVM)
	require.NoError(t, err)
	require.NotZero(t, after.Mode()&os.ModeCharDevice)
	reboundRdev, ok := charDeviceRdev(after)
	require.True(t, ok)
	require.Equal(t, hostRdev, reboundRdev)
}
