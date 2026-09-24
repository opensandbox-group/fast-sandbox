package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// kvmDeviceInfo resolves a real character device FileInfo for the alignment
// tests: charDeviceRdev reads the *syscall.Stat_t behind os.Stat, so fakes
// cannot exercise the rebind decision. /dev/null and /dev/zero exist on every
// supported platform and carry distinct device numbers.
func kvmDeviceInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	_, ok := charDeviceRdev(info)
	require.True(t, ok, "%s must be a character device", path)
	return info
}

func TestAlignJailKVMInode(t *testing.T) {
	nullInfo := kvmDeviceInfo(t, "/dev/null")
	zeroInfo := kvmDeviceInfo(t, "/dev/zero")
	nullRdev, ok := charDeviceRdev(nullInfo)
	require.True(t, ok)
	zeroRdev, ok := charDeviceRdev(zeroInfo)
	require.True(t, ok)
	require.NotEqual(t, nullRdev, zeroRdev, "test devices must carry distinct device numbers")

	// stat resolves the host path to nullInfo and everything else to
	// zeroInfo, so the jail inode (zero) differs from the host (null)
	// unless both sides map to the same device.
	stat := func(path string) (os.FileInfo, error) {
		if path == "/dev/kvm" {
			return nullInfo, nil
		}
		return zeroInfo, nil
	}

	t.Run("matching device numbers keep the jail inode untouched", func(t *testing.T) {
		sameStat := func(string) (os.FileInfo, error) { return nullInfo, nil }
		rebinds := 0
		rebind := func(string, uint64) error { rebinds++; return nil }
		require.NoError(t, alignJailKVMInode(sameStat, rebind, hostKVMDevicePath, "/jail/dev/kvm"))
		require.Zero(t, rebinds)
	})

	t.Run("a differently bound jail inode is rebound to the host numbers", func(t *testing.T) {
		var reboundPath string
		var reboundRdev uint64
		rebind := func(path string, rdev uint64) error {
			reboundPath, reboundRdev = path, rdev
			return nil
		}
		require.NoError(t, alignJailKVMInode(stat, rebind, "/dev/kvm", "/jail/dev/kvm"))
		require.Equal(t, "/jail/dev/kvm", reboundPath)
		require.Equal(t, nullRdev, reboundRdev)
	})

	t.Run("rebind failures propagate", func(t *testing.T) {
		boom := errors.New("mknod broke")
		require.ErrorIs(t, alignJailKVMInode(stat, func(string, uint64) error { return boom },
			"/dev/kvm", "/jail/dev/kvm"), boom)
	})

	t.Run("host device errors fail explicitly", func(t *testing.T) {
		boom := errors.New("stat broke")
		err := alignJailKVMInode(func(string) (os.FileInfo, error) { return nil, boom },
			func(string, uint64) error { return nil }, hostKVMDevicePath, "/jail/dev/kvm")
		require.ErrorIs(t, err, boom)
		require.Contains(t, err.Error(), "stat host KVM device")
	})

	t.Run("a missing jail inode fails instead of guessing", func(t *testing.T) {
		jailMissing := func(path string) (os.FileInfo, error) {
			if path == "/jail/dev/kvm" {
				return nil, os.ErrNotExist
			}
			return nullInfo, nil
		}
		err := alignJailKVMInode(jailMissing, func(string, uint64) error { return nil },
			hostKVMDevicePath, "/jail/dev/kvm")
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Contains(t, err.Error(), "stat jailed KVM device")
	})

	t.Run("non-character devices fail explicitly", func(t *testing.T) {
		regular, err := os.CreateTemp(t.TempDir(), "regular")
		require.NoError(t, err)
		require.NoError(t, regular.Close())
		info, err := os.Stat(regular.Name())
		require.NoError(t, err)
		_, ok := charDeviceRdev(info)
		require.False(t, ok, "regular files must not carry a device number")

		fileStat := func(string) (os.FileInfo, error) { return info, nil }
		err = alignJailKVMInode(fileStat, func(string, uint64) error { return nil },
			hostKVMDevicePath, "/jail/dev/kvm")
		require.Contains(t, err.Error(), "host KVM device")
		err = alignJailKVMInode(func(path string) (os.FileInfo, error) {
			if path == hostKVMDevicePath {
				return nullInfo, nil
			}
			return info, nil
		}, func(string, uint64) error { return nil }, hostKVMDevicePath, "/jail/dev/kvm")
		require.Contains(t, err.Error(), "jailed KVM device")
	})
}

func TestJailedKVMDevicePath(t *testing.T) {
	jailRoot := filepath.Join(string(filepath.Separator), "srv", "jailer", "firecracker", "id", "root")
	require.Equal(t, filepath.Join(jailRoot, "dev", "kvm"), jailedKVMDevicePath(jailRoot))
}
