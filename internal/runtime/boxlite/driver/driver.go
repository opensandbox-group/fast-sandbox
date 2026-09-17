package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
)

const boxLiteMaxResponseBytes = 4 << 20

var requiredBoxLiteSidecarCapabilities = boxliteprotocol.RequiredCapabilities

// Driver is intentionally a pure-Go client. Native BoxLite code lives
// in a dedicated Pod sidecar and is reached only through a versioned UDS API.
type Driver struct {
	mu               sync.RWMutex
	profile          runtimecatalog.RuntimeProfile
	config           runtimecatalog.BoxLiteConfig
	namespace        string
	client           *http.Client
	transport        *http.Transport
	infraMgr         *fastletinfra.Manager
	accessByID       map[string]dataplane.AccessDescriptor
	registryProvider registryconfig.Provider
}

func (d *Driver) SetRegistryProvider(provider registryconfig.Provider) {
	d.mu.Lock()
	d.registryProvider = provider
	d.mu.Unlock()
}

type boxLiteCapabilities = boxliteprotocol.Capabilities
type boxLiteArtifact = boxliteprotocol.Artifact
type boxLiteEnsureRequest = boxliteprotocol.EnsureRequest
type boxLiteBox = boxliteprotocol.Box
type boxLiteListResponse = boxliteprotocol.ListResponse
type boxLiteImagesResponse = boxliteprotocol.ImagesResponse
type boxLitePullRequest = boxliteprotocol.PullRequest
type boxLiteErrorResponse = boxliteprotocol.ErrorResponse

func New(profile runtimecatalog.RuntimeProfile) (*Driver, error) {
	if profile.BoxLite == nil {
		return nil, fmt.Errorf("boxLite runtime profile %q has no private configuration", profile.Name)
	}
	return &Driver{
		profile: profile, config: *profile.BoxLite,
		accessByID: make(map[string]dataplane.AccessDescriptor),
	}, nil
}

func (d *Driver) Initialize(_ context.Context, _ string) error {
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

func (d *Driver) SetNamespace(namespace string) {
	d.mu.Lock()
	d.namespace = namespace
	d.mu.Unlock()
}

func (d *Driver) SetInfraManager(manager *fastletinfra.Manager) {
	d.mu.Lock()
	d.infraMgr = manager
	d.mu.Unlock()
}

func (d *Driver) ProbeCapabilities(ctx context.Context) CapabilityReport {
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

func (d *Driver) EnsureSandbox(ctx context.Context, input *fastletapi.EnsureSandboxInput) (*SandboxMetadata, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: BoxLite Sandbox input is required", ErrInvalidConfig)
	}
	config := &input.Sandbox
	identity := config.Identity
	if identity.SandboxUID == "" || identity.FastletPodUID == "" || identity.InstanceGeneration <= 0 || identity.RuntimeInstanceID == "" || identity.AssignmentAttempt <= 0 {
		return nil, fmt.Errorf("%w: complete BoxLite Sandbox identity is required", ErrInvalidConfig)
	}
	d.mu.RLock()
	namespace := d.namespace
	infraManager := d.infraMgr
	d.mu.RUnlock()
	request := boxLiteEnsureRequest{FastletNamespace: namespace, Input: *input, TunnelGuestPort: d.config.TunnelGuestPort}
	if infraManager != nil {
		instance, err := infraManager.PrepareInstance(ctx, config)
		if err != nil {
			return nil, fmt.Errorf("%w: prepare BoxLite Infra Component instance: %v", ErrInfraUnavailable, err)
		}
		for _, mount := range instance.Mounts {
			source := mount.GuestSource
			if source == "" {
				source = mount.Source
			}
			request.Artifacts = append(request.Artifacts, boxLiteArtifact{
				Source: source, Destination: mount.Destination, Options: append([]string(nil), mount.Options...),
			})
		}
	}
	var box boxLiteBox
	if err := d.doJSON(ctx, http.MethodPut, "/v1/boxes/"+url.PathEscape(identity.SandboxUID), request, &box); err != nil {
		return nil, err
	}
	metadata, err := d.metadataFromBox(box)
	if err != nil {
		return nil, err
	}
	if err := validateExistingRuntimeProfile(metadata, config); err != nil {
		return nil, err
	}
	d.rememberAccess(metadata.Config.Identity.SandboxUID, box.Access)
	return metadata, nil
}

func (d *Driver) InspectSandbox(ctx context.Context, sandboxID string) (*SandboxMetadata, error) {
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
	d.rememberAccess(metadata.Config.Identity.SandboxUID, box.Access)
	return metadata, nil
}

func (d *Driver) DeleteSandbox(ctx context.Context, sandboxID string) error {
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

func (d *Driver) ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error) {
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
		d.rememberAccess(metadata.Config.Identity.SandboxUID, box.Access)
		managed = append(managed, metadata)
	}
	return managed, nil
}

// RecoverRuntimeResources reattaches the Sidecar to each durable Box and
// restores the guest LocalForward tunnel before Fastlet routes are published.
func (d *Driver) RecoverRuntimeResources(ctx context.Context, managed []*SandboxMetadata) error {
	for _, metadata := range managed {
		if metadata == nil || metadata.Config.Identity.SandboxUID == "" {
			continue
		}
		sandboxUID := metadata.Config.Identity.SandboxUID
		var box boxLiteBox
		if err := d.doJSON(ctx, http.MethodPost, "/v1/boxes/"+url.PathEscape(sandboxUID), nil, &box); err != nil {
			return fmt.Errorf("recover BoxLite Sandbox %s: %w", sandboxUID, err)
		}
		if box.Config.Identity.SandboxUID != sandboxUID {
			return fmt.Errorf("%w: recovered BoxLite Sandbox identity mismatch", ErrSandboxProfileMismatch)
		}
		d.rememberAccess(sandboxUID, box.Access)
	}
	return nil
}

func (d *Driver) GetAccessDescriptor(sandboxID string) (dataplane.AccessDescriptor, error) {
	d.mu.RLock()
	access, ok := d.accessByID[sandboxID]
	d.mu.RUnlock()
	if !ok {
		return dataplane.AccessDescriptor{}, fmt.Errorf("%w: BoxLite access descriptor for %q is not recovered", ErrNetworkUnavailable, sandboxID)
	}
	if access.Kind != dataplane.AccessKindLocalForward {
		return dataplane.AccessDescriptor{}, fmt.Errorf("%w: invalid BoxLite LocalForward descriptor", ErrNetworkUnavailable)
	}
	if err := access.Validate(); err != nil {
		return dataplane.AccessDescriptor{}, fmt.Errorf("%w: %v", ErrNetworkUnavailable, err)
	}
	return access, nil
}

func (d *Driver) ListImages(ctx context.Context) ([]string, error) {
	var response boxLiteImagesResponse
	if err := d.doJSON(ctx, http.MethodGet, "/v1/images", nil, &response); err != nil {
		return nil, err
	}
	return append([]string(nil), response.Images...), nil
}

func (d *Driver) PullImage(ctx context.Context, image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("%w: image reference is required", ErrInvalidConfig)
	}
	return d.doJSON(ctx, http.MethodPost, "/v1/images/pull", boxLitePullRequest{Image: image}, nil)
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.transport != nil {
		d.transport.CloseIdleConnections()
	}
	d.client = nil
	d.transport = nil
	d.accessByID = make(map[string]dataplane.AccessDescriptor)
	return nil
}

func (d *Driver) metadataFromBox(box boxLiteBox) (*SandboxMetadata, error) {
	if box.Config.Identity.SandboxUID == "" || box.BoxID == "" {
		return nil, errors.New("BoxLite sidecar returned incomplete Box identity")
	}
	if box.Access.Kind != dataplane.AccessKindLocalForward {
		return nil, fmt.Errorf("%w: BoxLite sidecar did not return a LocalForward endpoint", ErrNetworkUnavailable)
	}
	if err := box.Access.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid BoxLite LocalForward endpoint: %v", ErrNetworkUnavailable, err)
	}
	return &SandboxMetadata{
		Config: box.Config, Allocation: box.Allocation, ContainerID: box.BoxID, PID: box.PID, Phase: box.Phase, CreatedAt: box.CreatedAt,
		UserProcessStartedAt:   box.UserProcessStartedAt,
		UserProcessStartSource: box.UserProcessStartSource,
		InfraServices:          append([]fastletinfra.ServiceEndpoint(nil), box.InfraServices...),
		InfraDiagnostics:       append([]fastletinfra.ComponentDiagnostic(nil), box.InfraDiagnostics...),
	}, nil
}

func (d *Driver) rememberAccess(sandboxID string, access dataplane.AccessDescriptor) {
	d.mu.Lock()
	d.accessByID[sandboxID] = access
	d.mu.Unlock()
}

func (d *Driver) doJSON(ctx context.Context, method, path string, input, output any) error {
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
		case response.StatusCode == http.StatusConflict && wireError.Code == boxliteprotocol.ErrorImmutableSpecConflict:
			return fmt.Errorf("%w: %s", ErrSandboxProfileMismatch, wireError.Message)
		case response.StatusCode == http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrSandboxAlreadyExists, wireError.Message)
		default:
			return fmt.Errorf("boxLite sidecar %s %s failed: %s: %s", method, path, wireError.Code, wireError.Message)
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
	_ RuntimeDriver            = (*Driver)(nil)
	_ RuntimeArtifactCache     = (*Driver)(nil)
	_ AccessDescriptorProvider = (*Driver)(nil)
)
