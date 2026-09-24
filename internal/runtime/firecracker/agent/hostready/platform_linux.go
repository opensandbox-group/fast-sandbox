//go:build linux

package hostready

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// fsTypeMagic maps the statfs f_type magic numbers of the filesystems the
// StateRoot realistically lives on.
var fsTypeMagic = map[int64]string{
	0x58465342: "xfs",
	0x9123683e: "btrfs",
	0xef53:     "ext4",
	0x01021994: "tmpfs",
	0x794c7630: "overlay",
	0xff534d42: "nfs",
}

// openDeviceRW opens a device node read-write (KVM requires O_RDWR; the
// TUN device needs it for TUNSETIFF).
func openDeviceRW(path string) error {
	fd, err := unix.Open(path, unix.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("device does not exist (modprobe kvm / missing device passthrough)")
		}
		return fmt.Errorf("open: %w", err)
	}
	return unix.Close(fd)
}

// statFS summarizes the filesystem of path.
func statFS(path string) (FsStat, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return FsStat{}, err
	}
	return FsStat{
		Type:       fsTypeMagic[int64(stats.Type)],
		FreeBytes:  int64(stats.Bavail) * int64(stats.Bsize),
		TotalBytes: int64(stats.Blocks) * int64(stats.Bsize),
	}, nil
}

// kernelRelease returns uname -r.
func kernelRelease() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", err
	}
	// Utsname fields are [65]byte on linux (int8 on some other GOOSes):
	// trim the NUL padding.
	return strings.TrimRight(string(uts.Release[:]), "\x00"), nil
}

// cpuFlags parses the flag lines out of /proc/cpuinfo (x86 "flags",
// arm64 "Features").
func cpuFlags() ([]string, error) {
	payload, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return nil, err
	}
	var flags []string
	for _, line := range strings.Split(string(payload), "\n") {
		if !strings.HasPrefix(line, "flags") && !strings.HasPrefix(line, "Features") {
			continue
		}
		_, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		flags = append(flags, strings.Fields(value)...)
	}
	if len(flags) == 0 {
		return nil, fmt.Errorf("no flag lines in /proc/cpuinfo")
	}
	return flags, nil
}

// memAvailable reads MemAvailableKB from /proc/meminfo (falling back to
// MemTotal on old kernels).
func memAvailable() (int64, error) {
	payload, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	values := map[string]int64{}
	for _, line := range strings.Split(string(payload), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[strings.TrimSpace(key)] = kib << 10
	}
	if available, ok := values["MemAvailable"]; ok {
		return available, nil
	}
	if total, ok := values["MemTotal"]; ok {
		return total, nil
	}
	return 0, fmt.Errorf("memAvailable/MemTotal missing in /proc/meminfo")
}

// reflinkProbe clones src onto dst with FICLONE and reports whether the
// filesystem supports the ioctl.
func reflinkProbe(dir string) (bool, error) {
	src, err := os.CreateTemp(dir, "reflink-src-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(src.Name())
	if _, err := src.WriteString("fast-sandbox reflink probe"); err != nil {
		src.Close()
		return false, err
	}
	src.Close()
	dst, err := os.CreateTemp(dir, "reflink-dst-*")
	if err != nil {
		return false, err
	}
	dst.Close()
	defer os.Remove(dst.Name())
	dstFd, err := unix.Open(dst.Name(), unix.O_WRONLY, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(dstFd)
	srcFd, err := unix.Open(src.Name(), unix.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(srcFd)
	if err := unix.IoctlFileClone(dstFd, srcFd); err != nil {
		return false, nil
	}
	return true, nil
}
