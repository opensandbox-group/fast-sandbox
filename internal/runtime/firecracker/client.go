package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxResponseBytes bounds Firecracker API responses read by the driver.
const maxResponseBytes = 4 << 20

// MachineConfigRequest mirrors PUT /machine-config.
type MachineConfigRequest struct {
	VCPUs       int    `json:"vcpu_count"`
	MemSizeMiB  int    `json:"mem_size_mib"`
	HtEnabled   bool   `json:"ht_enabled,omitempty"`
	CPUTemplate string `json:"cpu_template,omitempty"`
}

// BootSourceRequest mirrors PUT /boot-source.
type BootSourceRequest struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// SnapshotMemBackend mirrors the mem_backend object of PUT /snapshot/load.
type SnapshotMemBackend struct {
	BackendType string `json:"backend_type"` // "File"
	BackendPath string `json:"backend_path"`
}

// SnapshotNetworkOverride replaces the host tap of one restored network
// interface. Only the tap name can be overridden: the guest MAC and the
// interface identity are baked into the snapshot state (v1.16).
type SnapshotNetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// SnapshotLoadRequest mirrors PUT /snapshot/load (v1.16): restore a Full
// snapshot from a vmstate file and a file-backed memory snapshot. Load
// must be the first configuration call: any machine/drive/network setup
// before it is rejected by Firecracker.
type SnapshotLoadRequest struct {
	SnapshotPath     string                    `json:"snapshot_path"`
	MemBackend       SnapshotMemBackend        `json:"mem_backend"`
	ResumeVM         bool                      `json:"resume_vm"`
	NetworkOverrides []SnapshotNetworkOverride `json:"network_overrides,omitempty"`
}

// SnapshotCreateRequest mirrors PUT /snapshot/create (v1.16). It is used by
// the golden snapshot preparation path (E2E self-bootstrap): cold-boot a
// preparation VM, pause it, and dump vmstate + memory.
type SnapshotCreateRequest struct {
	SnapshotType string `json:"snapshot_type"` // "Full"
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// DriveRequest mirrors PUT /drives/{drive_id}.
type DriveRequest struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only,omitempty"`
	CacheType    string `json:"cache_type,omitempty"`
}

// NetworkInterfaceRequest mirrors PUT /network-interfaces/{iface_id}.
type NetworkInterfaceRequest struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// VSockRequest configures PUT /vsock for a cold-boot VM. Keep the host socket private to the runtime; Guest CIDs 0, 1, and 2 are reserved.
type VSockRequest struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type actionRequest struct {
	ActionType string `json:"action_type"`
}

type versionResponse struct {
	Version string `json:"firecracker_version"`
}

type instanceInfoResponse struct {
	State string `json:"state"`
}

// Client is a thin Firecracker REST API client over the per-Sandbox Unix
// socket. It deliberately covers only the lifecycle and snapshot surface
// consumed by the runtime driver; metrics endpoints are out of scope.
type Client struct {
	httpClient *http.Client
}

// NewClient dials the Firecracker API at socketPath (host filesystem path).
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		ForceAttemptHTTP2: false,
		IdleConnTimeout:   30 * time.Second,
	}
	return &Client{httpClient: &http.Client{Transport: transport}}
}

// Close releases the connection pool.
func (c *Client) Close() {
	if c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
}

// Version reports the Firecracker version served on the socket.
func (c *Client) Version(ctx context.Context) (string, error) {
	var response versionResponse
	if err := c.do(ctx, http.MethodGet, "/version", nil, &response); err != nil {
		return "", err
	}
	if response.Version == "" {
		return "", fmt.Errorf("Firecracker /version response missing firecracker_version")
	}
	return response.Version, nil
}

// ConfigureMachine applies the vCPU/memory machine configuration.
func (c *Client) ConfigureMachine(ctx context.Context, request MachineConfigRequest) error {
	return c.do(ctx, http.MethodPut, "/machine-config", request, nil)
}

// ConfigureBootSource sets the kernel image and boot arguments.
func (c *Client) ConfigureBootSource(ctx context.Context, request BootSourceRequest) error {
	return c.do(ctx, http.MethodPut, "/boot-source", request, nil)
}

// AttachDrive registers a virtio-blk device.
func (c *Client) AttachDrive(ctx context.Context, request DriveRequest) error {
	return c.do(ctx, http.MethodPut, "/drives/"+pathEscape(request.DriveID), request, nil)
}

// AttachNetworkInterface registers a virtio-net device backed by host_dev_name.
func (c *Client) AttachNetworkInterface(ctx context.Context, request NetworkInterfaceRequest) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+pathEscape(request.IfaceID), request, nil)
}

// ConfigureVSock configures the Guest control channel before InstanceStart.
// Do not use it as an additional configuration step before LoadSnapshot.
func (c *Client) ConfigureVSock(ctx context.Context, request VSockRequest) error {
	if request.GuestCID < 3 || request.GuestCID == ^uint32(0) || request.UDSPath == "" {
		return fmt.Errorf("vsock requires a non-reserved guest CID and private socket path")
	}
	return c.do(ctx, http.MethodPut, "/vsock", request, nil)
}

// Start boots the microVM (InstanceStart action).
func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", actionRequest{ActionType: "InstanceStart"}, nil)
}

// Pause pauses the microVM (PATCH /vm). It is part of the snapshot
// preparation surface (golden snapshot self-bootstrap).
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": vmPaused}, nil)
}

// Resume resumes a paused microVM (PATCH /vm). After snapshot/load with
// resume_vm=false the restored VM is Paused; v1.16 rejects InstanceStart
// post-load, so Resume is the restore resume path.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"}, nil)
}

// LoadSnapshot restores the microVM from a Full snapshot (vmstate + memory
// file). resume_vm=false leaves the VM paused after load; the driver then
// resumes it via PATCH /vm (Resume).
func (c *Client) LoadSnapshot(ctx context.Context, request SnapshotLoadRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", request, nil)
}

// CreateSnapshot writes a Full snapshot (vmstate + memory) of the paused
// microVM. It is part of the snapshot preparation surface.
func (c *Client) CreateSnapshot(ctx context.Context, request SnapshotCreateRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", request, nil)
}

// VMState returns the current machine state (NotStarted, Running, Paused).
// Firecracker v1.x serves instance info at GET /; older API versions used
// GET /vm.
func (c *Client) VMState(ctx context.Context) (string, error) {
	var response instanceInfoResponse
	if err := c.do(ctx, http.MethodGet, "/", nil, &response); err != nil {
		return "", err
	}
	return response.State, nil
}

func (c *Client) do(ctx context.Context, method, path string, input, output any) error {
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
	limited := io.LimitReader(response.Body, maxResponseBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, readErr := io.ReadAll(limited)
		if readErr != nil {
			return fmt.Errorf("firecracker %s %s failed with %s: %w", method, path, response.Status, readErr)
		}
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

func pathEscape(value string) string {
	// Drive and interface IDs are DNS-safe identifiers generated by the
	// driver; Firecracker only accepts a restricted set, so escaping is a
	// guard rather than a lookup.
	var escaped bytes.Buffer
	for _, character := range value {
		if isSafeIDCharacter(character) {
			escaped.WriteRune(character)
			continue
		}
		fmt.Fprintf(&escaped, "%%%02X", character)
	}
	return escaped.String()
}
