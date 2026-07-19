package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"fast-sandbox/internal/api"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/runtimecatalog"
)

const boxLiteMaxResponseBytes = 4 << 20

var requiredBoxLiteSidecarCapabilities = []string{
	"owner-fence-v1",
	"guest-copy-v1",
	"local-forward-v1",
	"resource-limits-v1",
	"recovery-v1",
	"image-cache-v1",
}

// BoxLiteDriver is intentionally a pure-Go client. Native BoxLite code lives
// in a dedicated Pod sidecar and is reached only through a versioned UDS API.
type BoxLiteDriver struct {
	mu         sync.RWMutex
	profile    runtimecatalog.RuntimeProfile
	config     runtimecatalog.BoxLiteConfig
	namespace  string
	client     *http.Client
	transport  *http.Transport
	infraMgr   *fastletinfra.Manager
	accessByID map[string]fastletnetwork.AccessDescriptor
}

type boxLiteCapabilities struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Ready           bool            `json:"ready"`
	Reason          string          `json:"reason,omitempty"`
	Message         string          `json:"message,omitempty"`
	Capabilities    map[string]bool `json:"capabilities"`
}

type boxLiteArtifact struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Options     []string `json:"options,omitempty"`
}

type boxLiteEnsureRequest struct {
	Namespace       string            `json:"namespace"`
	Sandbox         api.SandboxSpec   `json:"sandbox"`
	TunnelGuestPort uint32            `json:"tunnelGuestPort"`
	Artifacts       []boxLiteArtifact `json:"artifacts,omitempty"`
}

type boxLiteBox struct {
	Sandbox                    api.SandboxSpec                    `json:"sandbox"`
	BoxID                      string                             `json:"boxId"`
	PID                        int                                `json:"pid,omitempty"`
	Phase                      string                             `json:"phase"`
	CreatedAt                  int64                              `json:"createdAt"`
	Access                     fastletnetwork.AccessDescriptor    `json:"access"`
	InfraServices              []fastletinfra.ServiceEndpoint     `json:"infraServices,omitempty"`
	InfraUpstreamHeadersByPort map[uint32]map[string]string       `json:"infraUpstreamHeadersByPort,omitempty"`
	InfraDiagnostics           []fastletinfra.ComponentDiagnostic `json:"infraDiagnostics,omitempty"`
}

type boxLiteListResponse struct {
	Boxes []boxLiteBox `json:"boxes"`
}

type boxLiteImagesResponse struct {
	Images []string `json:"images"`
}

type boxLitePullRequest struct {
	Image string `json:"image"`
}

type boxLiteErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func newBoxLiteDriver(profile runtimecatalog.RuntimeProfile) *BoxLiteDriver {
	return &BoxLiteDriver{
		profile: profile, config: *profile.BoxLite,
		accessByID: make(map[string]fastletnetwork.AccessDescriptor),
	}
}

func (d *BoxLiteDriver) Initialize(_ context.Context, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return nil
	}
	if strings.TrimSpace(d.config.ControlSocket) == "" || strings.TrimSpace(d.config.ProtocolVersion) == "" || d.config.TunnelGuestPort == 0 {
		return fmt.Errorf("%w: BoxLite control socket, protocol version, and tunnel guest port are required", ErrInvalidConfig)
	}
	endpoint := d.config.ControlSocket
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		},
		ForceAttemptHTTP2: false,
		IdleConnTimeout:   90 * time.Second,
	}
	d.transport = transport
	d.client = &http.Client{Transport: transport}
	return nil
}

func (d *BoxLiteDriver) SetNamespace(namespace string) {
	d.mu.Lock()
	d.namespace = namespace
	d.mu.Unlock()
}

func (d *BoxLiteDriver) SetInfraManager(manager *fastletinfra.Manager) {
	d.mu.Lock()
	d.infraMgr = manager
	d.mu.Unlock()
}

func (d *BoxLiteDriver) ProbeCapabilities(ctx context.Context) CapabilityReport {
	report := CapabilityReport{Runtime: d.profile.Name, ProfileHash: d.profile.ProfileHash, State: runtimecatalog.CapabilityDegraded}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var capabilities boxLiteCapabilities
	if err := d.doJSON(probeCtx, http.MethodGet, "/v1/capabilities", nil, &capabilities); err != nil {
		report.Reason = "BoxLiteSidecarUnavailable"
		report.Message = err.Error()
		return report
	}
	if capabilities.ProtocolVersion != d.config.ProtocolVersion {
		report.Reason = "BoxLiteProtocolMismatch"
		report.Message = fmt.Sprintf("BoxLite sidecar protocol %q does not match required %q", capabilities.ProtocolVersion, d.config.ProtocolVersion)
		return report
	}
	for _, capability := range requiredBoxLiteSidecarCapabilities {
		if !capabilities.Capabilities[capability] {
			report.Missing = append(report.Missing, capability)
		}
	}
	if len(report.Missing) > 0 {
		report.Reason = "BoxLiteSidecarCapabilityMissing"
		report.Message = fmt.Sprintf("BoxLite sidecar is missing required capabilities: %v", report.Missing)
		return report
	}
	if !capabilities.Ready {
		report.Reason = capabilities.Reason
		if report.Reason == "" {
			report.Reason = "BoxLiteSidecarNotReady"
		}
		report.Message = capabilities.Message
		return report
	}
	report.State = runtimecatalog.CapabilityReady
	report.Reason = "RuntimeDriverReady"
	report.Message = "BoxLite runtime sidecar and tunnel capabilities are ready"
	return report
}

func (d *BoxLiteDriver) EnsureSandbox(ctx context.Context, config *api.SandboxSpec) (*SandboxMetadata, error) {
	if config == nil || config.SandboxID == "" || config.FastletPodUID == "" || config.InstanceGeneration <= 0 || config.AssignmentAttempt <= 0 {
		return nil, fmt.Errorf("%w: complete BoxLite Sandbox identity is required", ErrInvalidConfig)
	}
	d.mu.RLock()
	namespace := d.namespace
	infraManager := d.infraMgr
	d.mu.RUnlock()
	request := boxLiteEnsureRequest{Namespace: namespace, Sandbox: *config, TunnelGuestPort: d.config.TunnelGuestPort}
	if infraManager != nil {
		instance, err := infraManager.PrepareInstance(ctx, config)
		if err != nil {
			return nil, fmt.Errorf("%w: prepare BoxLite InfraProfile instance: %v", ErrInfraUnavailable, err)
		}
		for _, mount := range instance.Mounts {
			request.Artifacts = append(request.Artifacts, boxLiteArtifact{
				Source: mount.Source, Destination: mount.Destination, Options: append([]string(nil), mount.Options...),
			})
		}
	}
	var box boxLiteBox
	if err := d.doJSON(ctx, http.MethodPut, "/v1/boxes/"+url.PathEscape(config.SandboxID), request, &box); err != nil {
		return nil, err
	}
	metadata, err := d.metadataFromBox(box)
	if err != nil {
		return nil, err
	}
	if err := validateExistingRuntimeProfile(metadata, config); err != nil {
		return nil, err
	}
	d.rememberAccess(metadata.SandboxID, box.Access)
	return metadata, nil
}

func (d *BoxLiteDriver) InspectSandbox(ctx context.Context, sandboxID string) (*SandboxMetadata, error) {
	if sandboxID == "" {
		return nil, ErrSandboxNotFound
	}
	var box boxLiteBox
	if err := d.doJSON(ctx, http.MethodGet, "/v1/boxes/"+url.PathEscape(sandboxID), nil, &box); err != nil {
		return nil, err
	}
	metadata, err := d.metadataFromBox(box)
	if err != nil {
		return nil, err
	}
	d.rememberAccess(metadata.SandboxID, box.Access)
	return metadata, nil
}

func (d *BoxLiteDriver) DeleteSandbox(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return nil
	}
	err := d.doJSON(ctx, http.MethodDelete, "/v1/boxes/"+url.PathEscape(sandboxID), nil, nil)
	if errors.Is(err, ErrSandboxNotFound) {
		err = nil
	}
	if err == nil {
		d.mu.Lock()
		delete(d.accessByID, sandboxID)
		d.mu.Unlock()
	}
	return err
}

func (d *BoxLiteDriver) ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error) {
	d.mu.RLock()
	namespace := d.namespace
	d.mu.RUnlock()
	var response boxLiteListResponse
	if err := d.doJSON(ctx, http.MethodGet, "/v1/boxes?namespace="+url.QueryEscape(namespace), nil, &response); err != nil {
		return nil, err
	}
	managed := make([]*SandboxMetadata, 0, len(response.Boxes))
	for _, box := range response.Boxes {
		metadata, err := d.metadataFromBox(box)
		if err != nil {
			return nil, err
		}
		d.rememberAccess(metadata.SandboxID, box.Access)
		managed = append(managed, metadata)
	}
	return managed, nil
}

func (d *BoxLiteDriver) GetAccessDescriptor(sandboxID string) (fastletnetwork.AccessDescriptor, error) {
	d.mu.RLock()
	access, ok := d.accessByID[sandboxID]
	d.mu.RUnlock()
	if !ok {
		return fastletnetwork.AccessDescriptor{}, fmt.Errorf("%w: BoxLite access descriptor for %q is not recovered", ErrNetworkUnavailable, sandboxID)
	}
	if access.Kind != fastletnetwork.AccessKindLocalForward || access.Address == "" {
		return fastletnetwork.AccessDescriptor{}, fmt.Errorf("%w: invalid BoxLite LocalForward descriptor", ErrNetworkUnavailable)
	}
	return access, nil
}

func (d *BoxLiteDriver) ListImages(ctx context.Context) ([]string, error) {
	var response boxLiteImagesResponse
	if err := d.doJSON(ctx, http.MethodGet, "/v1/images", nil, &response); err != nil {
		return nil, err
	}
	return append([]string(nil), response.Images...), nil
}

func (d *BoxLiteDriver) PullImage(ctx context.Context, image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("%w: image reference is required", ErrInvalidConfig)
	}
	return d.doJSON(ctx, http.MethodPost, "/v1/images/pull", boxLitePullRequest{Image: image}, nil)
}

func (d *BoxLiteDriver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.transport != nil {
		d.transport.CloseIdleConnections()
	}
	d.client = nil
	d.transport = nil
	d.accessByID = make(map[string]fastletnetwork.AccessDescriptor)
	return nil
}

func (d *BoxLiteDriver) metadataFromBox(box boxLiteBox) (*SandboxMetadata, error) {
	if box.Sandbox.SandboxID == "" || box.BoxID == "" {
		return nil, errors.New("BoxLite sidecar returned incomplete Box identity")
	}
	if box.Access.Kind != fastletnetwork.AccessKindLocalForward || box.Access.Address == "" {
		return nil, fmt.Errorf("%w: BoxLite sidecar did not return a LocalForward endpoint", ErrNetworkUnavailable)
	}
	return &SandboxMetadata{
		SandboxSpec: box.Sandbox, ContainerID: box.BoxID, PID: box.PID, Phase: box.Phase, CreatedAt: box.CreatedAt,
		InfraServices:              append([]fastletinfra.ServiceEndpoint(nil), box.InfraServices...),
		InfraUpstreamHeadersByPort: cloneHeadersByPort(box.InfraUpstreamHeadersByPort),
		InfraDiagnostics:           append([]fastletinfra.ComponentDiagnostic(nil), box.InfraDiagnostics...),
	}, nil
}

func (d *BoxLiteDriver) rememberAccess(sandboxID string, access fastletnetwork.AccessDescriptor) {
	d.mu.Lock()
	d.accessByID[sandboxID] = access
	d.mu.Unlock()
}

func (d *BoxLiteDriver) doJSON(ctx context.Context, method, path string, input, output any) error {
	d.mu.RLock()
	client := d.client
	d.mu.RUnlock()
	if client == nil {
		return ErrRuntimeNotInitialized
	}
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://boxlite-runtime"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, boxLiteMaxResponseBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var wireError boxLiteErrorResponse
		_ = json.NewDecoder(limited).Decode(&wireError)
		if wireError.Message == "" {
			wireError.Message = response.Status
		}
		switch {
		case response.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrSandboxNotFound, wireError.Message)
		case response.StatusCode == http.StatusConflict && wireError.Code == "ImmutableSpecConflict":
			return fmt.Errorf("%w: %s", ErrSandboxProfileMismatch, wireError.Message)
		case response.StatusCode == http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrSandboxAlreadyExists, wireError.Message)
		default:
			return fmt.Errorf("BoxLite sidecar %s %s failed: %s: %s", method, path, wireError.Code, wireError.Message)
		}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(output); err != nil {
		return fmt.Errorf("decode BoxLite sidecar response: %w", err)
	}
	return nil
}

var (
	_ RuntimeDriver            = (*BoxLiteDriver)(nil)
	_ RuntimeArtifactCache     = (*BoxLiteDriver)(nil)
	_ InfraConfigurable        = (*BoxLiteDriver)(nil)
	_ AccessDescriptorProvider = (*BoxLiteDriver)(nil)
)
