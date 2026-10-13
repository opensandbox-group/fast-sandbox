package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// maxFirecrackerResponseBytes bounds Firecracker API responses.
const maxFirecrackerResponseBytes = 1 << 20

// firecrackerClient is a thin Firecracker REST client over the per-sandbox
// unix socket, covering only the snapshot-restore surface this tool needs
// (ADR-005). It mirrors internal/runtime/firecracker but is kept standalone
// so the CLI carries no control-plane dependencies (ADR-001).
type firecrackerClient struct {
	httpClient *http.Client
}

func newFirecrackerClient(socketPath string) *firecrackerClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &firecrackerClient{httpClient: &http.Client{Transport: transport, Timeout: 2 * time.Minute}}
}

func (c *firecrackerClient) close() {
	c.httpClient.CloseIdleConnections()
}

// snapshotMemBackend is the mem_backend object of PUT /snapshot/load.
type snapshotMemBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// snapshotLoadRequest mirrors PUT /snapshot/load: restore a Full snapshot
// from a vmstate file and a file-backed memory snapshot. It must be the first
// configuration call after launch.
type snapshotLoadRequest struct {
	SnapshotPath string             `json:"snapshot_path"`
	MemBackend   snapshotMemBackend `json:"mem_backend"`
	ResumeVM     bool               `json:"resume_vm"`
}

type versionResponse struct {
	Version string `json:"version"`
}

// version reports the Firecracker version served on the socket; it doubles as
// the socket-readiness probe.
func (c *firecrackerClient) version(ctx context.Context) (string, error) {
	var response versionResponse
	if err := c.do(ctx, http.MethodGet, "/version", nil, &response); err != nil {
		return "", err
	}
	return response.Version, nil
}

// loadSnapshot restores the microVM from a Full snapshot; resume_vm=false
// leaves the VM paused until the network is acquired. Firecracker opens every
// disk recorded in the vmstate during this call, so the rootfs drive path
// must already resolve (see rootfsDriveLink).
func (c *firecrackerClient) loadSnapshot(ctx context.Context, request snapshotLoadRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", request, nil)
}

// resume moves the restored (paused) VM to Running (PATCH /vm).
func (c *firecrackerClient) resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"}, nil)
}

// ---- cold-boot / snapshot-create surface (build flow) ----

// bootSource mirrors PUT /boot-source (build flow cold boot).
type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

// drive mirrors PUT /drives/{drive_id} (build flow cold boot).
type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// machineConfig mirrors PUT /machine-config (build flow cold boot).
type machineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

// snapshotCreateRequest mirrors PUT /snapshot/create: capture a Full
// snapshot (vmstate + full memory file) of a paused VM.
type snapshotCreateRequest struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// setBootSource installs the guest kernel and boot arguments.
func (c *firecrackerClient) setBootSource(ctx context.Context, source bootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", source, nil)
}

// addDrive attaches a host block device to the guest.
func (c *firecrackerClient) addDrive(ctx context.Context, drive drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+drive.DriveID, drive, nil)
}

// setMachineConfig sizes the microVM.
func (c *firecrackerClient) setMachineConfig(ctx context.Context, config machineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", config, nil)
}

// startInstance boots the configured microVM (PUT /actions InstanceStart).
func (c *firecrackerClient) startInstance(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"}, nil)
}

// pause moves the running VM to Paused (PATCH /vm); a Full snapshot create
// requires the VM to be paused first.
func (c *firecrackerClient) pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"}, nil)
}

// createSnapshot captures a Full snapshot of the paused VM.
func (c *firecrackerClient) createSnapshot(ctx context.Context, request snapshotCreateRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", request, nil)
}

// waitReady polls the API socket until Firecracker answers GET /version or the
// context expires.
func (c *firecrackerClient) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := c.version(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker API did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *firecrackerClient) do(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxFirecrackerResponseBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(limited)
		return fmt.Errorf("firecracker %s %s failed with %s: %s", method, path, response.Status, bytes.TrimSpace(payload))
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(output); err != nil {
		return fmt.Errorf("decode firecracker response: %w", err)
	}
	return nil
}

// firecrackerIDLimit matches Firecracker's --id length ceiling.
const firecrackerIDLimit = 32

// defaultRootfsDriveID is the drive_id the rootfs device is registered under
// when the template is built; it is baked into the snapshot vmstate.
const defaultRootfsDriveID = "rootfs"

// rootfsDriveLink is the relative path_on_host of the rootfs drive. The build
// attaches the drive through this name, so the vmstate records it instead of
// the build host's /dev/ublkbN. Firecracker resolves it against its working
// directory, where a symlink of this name points at the device attached on
// the current host; the same snapshot therefore restores whatever dev_id
// ublkd hands out (the convention of internal/runtime/firecracker restore).
const rootfsDriveLink = "rootfs.dev"

// linkRootfsDrive (re)creates <dir>/rootfs.dev pointing at devPath.
func linkRootfsDrive(dir, devPath string) (string, error) {
	link := filepath.Join(dir, rootfsDriveLink)
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale rootfs drive link %s: %w", link, err)
	}
	if err := os.Symlink(devPath, link); err != nil {
		return "", fmt.Errorf("link rootfs drive %s -> %s: %w", link, devPath, err)
	}
	return link, nil
}

// launchFirecracker starts the firecracker binary detached from the CLI
// process with workDir as its working directory (relative drive paths resolve
// there), writing stdout/stderr to logPath. The VM deliberately outlives the
// command context; only an explicit terminate (via delete) stops it. The
// returned record identifies the process for later signalling.
func launchFirecracker(bin, workDir, apiSocket, vmID, logPath string) (processRecord, error) {
	// A stale socket from a crashed run blocks the new bind.
	_ = os.Remove(apiSocket)
	id := vmID
	if len(id) > firecrackerIDLimit {
		id = id[:firecrackerIDLimit]
	}
	command := exec.Command(bin, "--id", id, "--api-sock", apiSocket)
	command.Dir = workDir
	if logPath != "" {
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return processRecord{}, fmt.Errorf("open firecracker log %s: %w", logPath, err)
		}
		defer logFile.Close()
		command.Stdout = logFile
		command.Stderr = logFile
	}
	if err := command.Start(); err != nil {
		return processRecord{}, fmt.Errorf("start firecracker: %w", err)
	}
	// Capture the identity while the child cannot have been reaped yet: Wait
	// has not run, so the PID still names this process (alive or zombie).
	record, err := recordProcess(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return processRecord{}, err
	}
	// Reap the child asynchronously so it does not linger as a zombie after
	// the short-lived CLI exits.
	go func() { _ = command.Wait() }()
	return record, nil
}
