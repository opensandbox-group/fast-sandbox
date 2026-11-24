package network

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

func detachNetNSAliases(path string) error {
	dir, name := filepath.Dir(path), filepath.Base(path)
	parent, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	mounts, err := mountinfo.GetMounts(func(m *mountinfo.Info) (bool, bool) {
		return m.FSType != "nsfs" || filepath.Base(m.Mountpoint) != name, false
	})
	if err != nil {
		return err
	}
	for _, m := range mounts {
		// Only detach this slot's file on another bind of the same directory.
		aliasParent, err := os.Stat(filepath.Dir(m.Mountpoint))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !os.SameFile(parent, aliasParent) {
			continue
		}
		if err := unix.Unmount(m.Mountpoint, unix.MNT_DETACH); err != nil &&
			!errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("detach network namespace mount %s: %w", m.Mountpoint, err)
		}
	}
	return nil
}
