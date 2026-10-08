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

// drivePatch mirrors PATCH /drives/{drive_id}: redirect a restored drive to a
// new host backing path (the freshly attached /dev/ublkbN).
type drivePatch struct {
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host"`
}

type versionResponse struct {
	Version string `json:"version"`
}

type instanceInfoResponse struct {
	State string `json:"state"`
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
// leaves the VM paused for post-load drive redirection.
func (c *firecrackerClient) loadSnapshot(ctx context.Context, request snapshotLoadRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", request, nil)
}

// updateDrive redirects a restored drive's backing path (PATCH /drives/{id}).
func (c *firecrackerClient) updateDrive(ctx context.Context, driveID, pathOnHost string) error {
	return c.do(ctx, http.MethodPatch, "/drives/"+driveID, drivePatch{DriveID: driveID, PathOnHost: pathOnHost}, nil)
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

// state returns the current machine state.
func (c *firecrackerClient) state(ctx context.Context) (string, error) {
	var response instanceInfoResponse
	if err := c.do(ctx, http.MethodGet, "/", nil, &response); err != nil {
		return "", err
	}
	return response.State, nil
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

// launchFirecracker starts the firecracker binary detached from the CLI
// process, writing stdout/stderr to logPath. The VM deliberately outlives the
// command context; only an explicit kill (via delete) terminates it.
func launchFirecracker(bin, apiSocket, sandboxID, logPath string) (int, error) {
	// A stale socket from a crashed run blocks the new bind.
	_ = os.Remove(apiSocket)
	id := sandboxID
	if len(id) > firecrackerIDLimit {
		id = id[:firecrackerIDLimit]
	}
	command := exec.Command(bin, "--id", id, "--api-sock", apiSocket)
	if logPath != "" {
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return 0, fmt.Errorf("open firecracker log %s: %w", logPath, err)
		}
		defer logFile.Close()
		command.Stdout = logFile
		command.Stderr = logFile
	}
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start firecracker: %w", err)
	}
	// Reap the child asynchronously so it does not linger as a zombie after
	// the short-lived CLI exits; the PID is persisted for later signalling.
	pid := command.Process.Pid
	go func() { _ = command.Wait() }()
	return pid, nil
}
