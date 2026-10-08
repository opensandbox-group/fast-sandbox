//go:build linux

package firecracker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func stateRootFilesystemType(path string) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", err
	}
	switch stat.Type {
	case unix.XFS_SUPER_MAGIC:
		return "xfs", nil
	case unix.EXT4_SUPER_MAGIC:
		return "ext4", nil
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs", nil
	default:
		return fmt.Sprintf("filesystem type 0x%x", stat.Type), nil
	}
}
