// Package main: build-flow Firecracker cold boot and snapshot capture.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file encapsulates the build-flow Firecracker phase: inject an init
// script into the writable rootfs device, cold-boot a 1-shot microVM, wait
// for the guest workload to report readiness on the serial log, then pause
// and capture a Full snapshot (vmstate.bin + memfile).

const (
	// defaultBootArgs boots the first virtio-blk drive as / and hands control
	// to the injected init script. console=ttyS0 routes guest logs (including
	// the redis ready line) to the serial log file.
	defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw"

	// defaultInitPath is where the init script is written inside the rootfs.
	defaultInitPath = "/fsb-vm-init"

	// defaultReadyPattern is the redis-server readiness line on stdout.
	defaultReadyPattern = "Ready to accept connections"

	// buildVMID is the Firecracker --id of the one-shot build VM.
	buildVMID = "fsb-vm-build"
)

// defaultInitScript renders the init script baked into the rootfs for the
// redis sample: it mounts /proc, enters the image workdir and execs
// redis-server. --init-script-file replaces it for other images.
func defaultInitScript() string {
	return `#!/bin/sh
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mount -t proc proc /proc 2>/dev/null
cd /data 2>/dev/null
exec /usr/local/bin/redis-server --protected-mode no
`
}

// injectInitScript mounts the writable rootfs device, writes initPath (an
// absolute guest path) with mode 0755, syncs and unmounts. The script
// becomes part of the committed rootfs upper layer. The mount joins the
// rollback so the device is never detached while still mounted.
func injectInitScript(rootfsDevPath, mountPoint, initPath string, script []byte, undo *rollback) error {
	if err := mountReadWrite(rootfsDevPath, mountPoint); err != nil {
		return err
	}
	undo.push("mount "+mountPoint, func() error { return unmount(mountPoint) })
	if err := writeGuestFile(mountPoint, initPath, script, 0o755); err != nil {
		return err
	}
	syncFilesystems()
	if err := unmount(mountPoint); err != nil {
		return err
	}
	return nil
}

// validateInitPath requires a clean absolute guest path that can be passed
// as a single init= kernel argument.
func validateInitPath(initPath string) error {
	if !filepath.IsAbs(initPath) || filepath.Clean(initPath) != initPath || initPath == "/" ||
		strings.ContainsAny(initPath, " \t\r\n") {
		return fmt.Errorf("--init-path must be a clean absolute guest file path without whitespace, got %q", initPath)
	}
	return nil
}

// writeGuestFile writes payload to guestPath inside the mounted guest
// filesystem at rootDir. The image is untrusted: every lookup goes through
// os.Root, so a symlink in the image (e.g. /init -> /host/etc/passwd or an
// absolute link) can never redirect the write onto the host. Whatever the
// image left at the final component (a symlink, a device node or FIFO that
// would route the write to host I/O, a hard link shared with another guest
// file) is unlinked and a fresh regular file is created with O_EXCL, so the
// script is only ever written into a new inode of the guest filesystem.
func writeGuestFile(rootDir, guestPath string, payload []byte, mode os.FileMode) error {
	if err := validateInitPath(guestPath); err != nil {
		return err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return fmt.Errorf("open guest root %s: %w", rootDir, err)
	}
	defer root.Close()
	name := strings.TrimPrefix(guestPath, "/")
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create guest directory %s: %w", dir, err)
		}
	}
	if info, err := root.Lstat(name); err == nil {
		if info.IsDir() {
			return fmt.Errorf("guest path %s is a directory", guestPath)
		}
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("replace guest %s %s: %w", info.Mode().Type(), guestPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect guest file %s: %w", guestPath, err)
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create guest file %s: %w", guestPath, err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write guest file %s: %w", guestPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close guest file %s: %w", guestPath, err)
	}
	// The create mode is filtered by the umask.
	if err := root.Chmod(name, mode); err != nil {
		return fmt.Errorf("chmod guest file %s: %w", guestPath, err)
	}
	return nil
}

// coldBootAndSnapshot launches a 1-shot Firecracker on rootfsDevPath, waits
// for the ready pattern in the serial log, pauses the VM and captures a Full
// snapshot into snapDir. The drive is attached through the relative
// rootfs.dev link inside the Firecracker working directory, so the vmstate
// records no host device path. The Firecracker process is always stopped
// before returning; it also joins the rollback, so a VMM that could not be
// stopped keeps the rootfs device it holds attached. The produced file
// paths are returned.
func coldBootAndSnapshot(ctx context.Context, options *buildOptions, workDir, rootfsDevPath string, undo *rollback) (vmstatePath, memfilePath string, err error) {
	runtimeDir := filepath.Join(workDir, "fc")
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create firecracker runtime dir: %w", err)
	}
	apiSocket := filepath.Join(runtimeDir, apiSocketName)
	serialLog := filepath.Join(runtimeDir, serialLogName)
	if _, err := linkRootfsDrive(runtimeDir, rootfsDevPath); err != nil {
		return "", "", err
	}

	bootArgs := options.bootArgs
	if strings.TrimSpace(bootArgs) == "" {
		bootArgs = defaultBootArgs + " init=" + options.initPath
	}

	process, err := launchFirecracker(global.firecrackerBin, runtimeDir, apiSocket, buildVMID, serialLog)
	if err != nil {
		return "", "", err
	}
	// The build VM never outlives this function, success or failure.
	undo.push(fmt.Sprintf("build VM (pid %d)", process.PID), func() error {
		return terminateProcess(process, firecrackerStopGrace)
	})
	defer func() {
		if stopErr := terminateProcess(process, firecrackerStopGrace); stopErr != nil && err == nil {
			err = fmt.Errorf("stop build VM: %w", stopErr)
		}
	}()

	client := newFirecrackerClient(apiSocket)
	defer client.close()
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.waitReady(readyCtx); err != nil {
		return "", "", err
	}
	if err := client.setBootSource(ctx, bootSource{KernelImagePath: options.kernel, BootArgs: bootArgs}); err != nil {
		return "", "", fmt.Errorf("configure boot source: %w", err)
	}
	if err := client.addDrive(ctx, drive{
		DriveID:      options.rootfsDriveID,
		PathOnHost:   rootfsDriveLink,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		return "", "", fmt.Errorf("attach rootfs drive: %w", err)
	}
	if err := client.setMachineConfig(ctx, machineConfig{VCPUCount: options.vcpu, MemSizeMiB: options.memoryMB}); err != nil {
		return "", "", fmt.Errorf("configure machine: %w", err)
	}
	if err := client.startInstance(ctx); err != nil {
		return "", "", fmt.Errorf("start instance: %w", err)
	}

	if err := waitSerialFor(serialLog, options.readyPattern, options.readyTimeout); err != nil {
		return "", "", err
	}

	snapDir := filepath.Join(workDir, "snapshot")
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create snapshot dir: %w", err)
	}
	vmstatePath = filepath.Join(snapDir, inDiskVMState)
	memfilePath = filepath.Join(snapDir, inDiskMemfile)
	if err := client.pause(ctx); err != nil {
		return "", "", fmt.Errorf("pause VM: %w", err)
	}
	if err := client.createSnapshot(ctx, snapshotCreateRequest{
		SnapshotType: "Full",
		SnapshotPath: vmstatePath,
		MemFilePath:  memfilePath,
	}); err != nil {
		return "", "", fmt.Errorf("create full snapshot: %w", err)
	}
	return vmstatePath, memfilePath, nil
}

// waitSerialFor polls the serial log until pattern appears or timeout
// expires; on timeout the log tail is included in the error for diagnosis.
func waitSerialFor(logPath, pattern string, timeout time.Duration) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("ready pattern must not be empty")
	}
	deadline := time.Now().Add(timeout)
	for {
		payload, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(payload), pattern) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest did not report %q within %s; serial log tail:\n%s",
				pattern, timeout, logTail(payload, 4096))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// logTail returns the last limit bytes of payload for error messages.
func logTail(payload []byte, limit int) string {
	if len(payload) <= limit {
		return string(payload)
	}
	return "...\n" + string(payload[len(payload)-limit:])
}
