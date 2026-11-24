//go:build linux && (amd64 || arm64)

package guestcontrol

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux UAPI definitions: include/uapi/linux/fs.h, _IOWR('X', 119/120, int).
// These ioctl encodings apply to the supported amd64 and arm64 Guests.
const (
	fsFreeze = 0xc0045877
	fsThaw   = 0xc0045878
)

// LinuxFilesystem holds a mount descriptor opened before protection begins.
// The caller must exclusively control freeze/thaw for this mount and keep the descriptor open until all operations and recovery finish.
type LinuxFilesystem struct {
	mount *os.File
	path  string
}

func OpenFilesystem(mountPath string) (*LinuxFilesystem, error) {
	f, err := os.Open(mountPath)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.IsDir() {
		f.Close()
		return nil, &os.PathError{Op: "open mount", Path: mountPath, Err: unix.ENOTDIR}
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &stat); err != nil {
		f.Close()
		return nil, err
	}
	if stat.Type != unix.EXT4_SUPER_MAGIC {
		f.Close()
		return nil, fmt.Errorf("only ext4 is supported")
	}
	resolved, err := filepath.EvalSymlinks(mountPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		f.Close()
		return nil, err
	}
	// FIFREEZE on any subdirectory freezes the entire filesystem; require an exact mount point to avoid accidentally freezing the parent root filesystem.
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		f.Close()
		return nil, err
	}
	found := false
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && unescape.Replace(fields[4]) == resolved {
			found = true
			break
		}
	}
	if !found {
		f.Close()
		return nil, fmt.Errorf("%s is not an exact mount point", resolved)
	}
	return &LinuxFilesystem{mount: f, path: resolved}, nil
}
func (f *LinuxFilesystem) Freeze() error {
	return unix.IoctlSetInt(int(f.mount.Fd()), fsFreeze, 0)
}
func (f *LinuxFilesystem) Thaw() error  { return unix.IoctlSetInt(int(f.mount.Fd()), fsThaw, 0) }
func (f *LinuxFilesystem) Close() error { return f.mount.Close() }

// ProtectionIdentity returns the mount verified at open time, not the caller's claimed path.
func (f *LinuxFilesystem) ProtectionIdentity() (string, string) { return f.path, "ext4" }
