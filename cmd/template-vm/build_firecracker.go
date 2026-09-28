// Package main: build-flow Firecracker cold boot and snapshot capture.
package main

import (
	"context"
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
	defaultInitPath = "/template-vm-init"

	// defaultReadyPattern is the redis-server readiness line on stdout.
	defaultReadyPattern = "Ready to accept connections"
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
// becomes part of the committed rootfs upper layer.
func injectInitScript(rootfsDevPath, mountPoint, initPath string, script []byte) error {
	if err := mountReadWrite(rootfsDevPath, mountPoint); err != nil {
		return err
	}
	defer func() { _ = unmount(mountPoint) }()
	target := filepath.Join(mountPoint, initPath)
	if err := os.WriteFile(target, script, 0o755); err != nil {
		return fmt.Errorf("write init script %s: %w", target, err)
	}
	syncFilesystems()
	if err := unmount(mountPoint); err != nil {
		return err
	}
	return nil
}

// coldBootAndSnapshot launches a 1-shot Firecracker on rootfsDevPath, waits
// for the ready pattern in the serial log, pauses the VM and captures a Full
// snapshot into snapDir. The Firecracker process is always killed before
// returning; the produced file paths are returned.
func coldBootAndSnapshot(ctx context.Context, options *buildOptions, workDir, rootfsDevPath string) (vmstatePath, memfilePath string, err error) {
	runtimeDir := filepath.Join(workDir, "fc")
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create firecracker runtime dir: %w", err)
	}
	apiSocket := filepath.Join(runtimeDir, apiSocketName)
	serialLog := filepath.Join(runtimeDir, serialLogName)

	bootArgs := options.bootArgs
	if strings.TrimSpace(bootArgs) == "" {
		bootArgs = defaultBootArgs + " init=" + options.initPath
	}

	pid, err := launchFirecracker(global.firecrackerBin, apiSocket, "tvm-build", serialLog)
	if err != nil {
		return "", "", err
	}
	// The build VM never outlives this function, success or failure.
	defer func() { _ = killProcess(pid) }()

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
		PathOnHost:   rootfsDevPath,
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
