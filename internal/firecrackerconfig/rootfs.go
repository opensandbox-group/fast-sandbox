// Package firecrackerconfig defines cold-boot device configuration shared by the CLI and Fastlet.
package firecrackerconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fast-sandbox/internal/artifacts"
)

type Operation struct {
	Path string
	Body any
}

// RootfsBoot only generates configuration; each entry point handles network allocation, jailer path translation, and InstanceStart.
func RootfsBoot(m artifacts.VMManifest, kernel, rootfs, tap, socket string) ([]Operation, error) {
	if kernel == "" || rootfs == "" {
		return nil, errors.New("resolved kernel and rootfs required")
	}
	if tap != "" && m.GuestNetwork == nil {
		return nil, errors.New("network provider requires guestNetwork")
	}
	ops := []Operation{
		{"/boot-source", map[string]any{"kernel_image_path": kernel, "boot_args": m.Kernel.Cmdline}},
		{"/machine-config", map[string]any{"vcpu_count": m.Machine.VCPUCount, "mem_size_mib": m.Machine.MemoryMiB}},
		{"/drives/" + m.Machine.RootfsDriveID, map[string]any{"drive_id": m.Machine.RootfsDriveID, "path_on_host": rootfs, "is_root_device": true, "is_read_only": false, "cache_type": "Writeback"}},
	}
	if tap != "" {
		ops = append(ops, Operation{"/network-interfaces/" + m.GuestNetwork.Iface, map[string]string{"iface_id": m.GuestNetwork.Iface, "guest_mac": m.GuestNetwork.MAC, "host_dev_name": tap}})
	}
	if m.GuestControl != nil {
		ops = append(ops, Operation{"/vsock", map[string]any{"guest_cid": 3, "uds_path": socket}})
	}
	ops = append(ops, Operation{"/entropy", map[string]any{}})
	if m.FreePageReporting != nil && *m.FreePageReporting {
		ops = append(ops, Operation{"/balloon", map[string]any{"amount_mib": 0, "deflate_on_oom": true, "free_page_reporting": true}})
	}
	return ops, nil
}

// ResolveKernel resolves the Builder default filename, versioned filenames, and explicit path overrides consistently.
func ResolveKernel(kernelDir, kernelVersion, override string) (string, error) {
	path := override
	if path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve kernel override %q: %w", path, err)
		}
		path = absolute
	}
	if path == "" {
		if kernelVersion == "" || filepath.Base(kernelVersion) != kernelVersion || kernelVersion == "." || kernelVersion == ".." || strings.ContainsAny(kernelVersion, "\x00\\") {
			return "", fmt.Errorf("invalid kernel.version %q", kernelVersion)
		}
		name := "vmlinux-" + kernelVersion
		// The standard Builder uses an unversioned default filename.
		if kernelVersion == "vmlinux.bin" {
			name = kernelVersion
		}
		path = filepath.Join(kernelDir, name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("kernel %s not found (kernel.version %q): %w", path, kernelVersion, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("kernel %s is not a regular file", path)
	}
	return path, nil
}

// PinKernel copies the boot input into a private inode before the VMM opens it.
// Hashing a mutable installation path after boot cannot identify the running kernel.
func PinKernel(source, target string) (sum string, resultErr error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("kernel must be a regular file")
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, output.Close())
		if resultErr != nil {
			_ = os.Remove(target)
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(output, hash), input); err != nil {
		return "", err
	}
	// Jailer may switch UID; the private parent directory controls access.
	if err := output.Chmod(0444); err != nil {
		return "", err
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
