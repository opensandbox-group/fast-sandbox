package firecracker

// agent_client.go is the driver's view of the node-level
// firecracker-runtime-agent (implementation plan §6): a JSON-over-HTTP
// client over the UDS socket. The driver stays in "local mode" (no agent,
// no remote pull) when no socket is configured, and falls back to the
// local cache check when the agent is unreachable — warmImages and cold
// boots must not depend on agent availability.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// Lease is the driver-side view of a runtime-agent device lease.
type Lease = agentprotocol.Lease

// errAgentUnreachable marks transport-level failures (socket missing,
// dial/read errors) of the agent client; the driver treats them as "agent
// absent" and falls back to local behavior.
var errAgentUnreachable = errors.New("runtime-agent unreachable")

// maxAgentResponseBytes bounds a decoded agent response body.
const maxAgentResponseBytes = 4 << 20

// AgentClient is the driver's view of the runtime-agent (testable via fake
// injection).
type AgentClient interface {
	// PinImage pulls and pins an image on the node, returning its manifest
	// digest. Replays of requestID are idempotent.
	PinImage(ctx context.Context, requestID, image string) (string, error)
	// UnpinImage drops one pin reference of an image.
	UnpinImage(ctx context.Context, requestID, image string) error
	// PublishImage uploads a node-local artifact set to the store under the
	// given index key and reports the manifest reference and digest.
	PublishImage(ctx context.Context, requestID, key, dir string) (PublishOutcome, error)
	// LeaseDevices creates a device lease for a Sandbox. The native stage
	// returns the shared cache file paths.
	LeaseDevices(ctx context.Context, requestID string, config *fastletapi.RuntimeSandboxConfig) (Lease, error)
	// ReleaseDevices drops a device lease owned by this pod.
	ReleaseDevices(ctx context.Context, requestID, leaseID string) error
	// ListLeases returns every lease on the node.
	ListLeases(ctx context.Context) ([]Lease, error)
	// Compatibility returns the node compatibility class.
	Compatibility(ctx context.Context) (string, error)
	// Health verifies the agent is serving.
	Health(ctx context.Context) error
}

// PublishOutcome reports one agent-side artifact publication.
type PublishOutcome struct {
	ManifestRef    string
	ArtifactDigest string
}

// agentHTTPClient implements AgentClient over the UDS socket.
type agentHTTPClient struct {
	client    *http.Client
	namespace string
	podUID    string
}

// NewAgentClient builds the UDS HTTP client of the runtime-agent. The
// caller identity (namespace + podUID) is attached to every request.
func NewAgentClient(socketPath, namespace, podUID string) (AgentClient, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("runtime-agent socket path is required")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		ForceAttemptHTTP2: false,
		IdleConnTimeout:   90 * time.Second,
	}
	return &agentHTTPClient{
		client:    &http.Client{Transport: transport},
		namespace: namespace,
		podUID:    podUID,
	}, nil
}

func (c *agentHTTPClient) PinImage(ctx context.Context, requestID, image string) (string, error) {
	var response agentprotocol.PinImageResponse
	if err := c.doJSON(ctx, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: c.identity(requestID), Image: image,
	}, &response); err != nil {
		return "", err
	}
	return response.ManifestDigest, nil
}

func (c *agentHTTPClient) UnpinImage(ctx context.Context, requestID, image string) error {
	return c.doJSON(ctx, agentprotocol.RouteUnpinImage, agentprotocol.UnpinImageRequest{
		Identity: c.identity(requestID), Image: image,
	}, nil)
}

func (c *agentHTTPClient) PublishImage(ctx context.Context, requestID, key, dir string) (PublishOutcome, error) {
	var response agentprotocol.PublishImageResponse
	if err := c.doJSON(ctx, agentprotocol.RoutePublishImage, agentprotocol.PublishImageRequest{
		Identity: c.identity(requestID), Key: key, Dir: dir,
	}, &response); err != nil {
		return PublishOutcome{}, err
	}
	return PublishOutcome{ManifestRef: response.ManifestRef, ArtifactDigest: response.ArtifactDigest}, nil
}

func (c *agentHTTPClient) LeaseDevices(ctx context.Context, requestID string, config *fastletapi.RuntimeSandboxConfig) (Lease, error) {
	if config == nil {
		return Lease{}, fmt.Errorf("%w: sandbox spec is required", runtimecontract.ErrInvalidConfig)
	}
	identity := config.Identity
	spec := config.Spec
	var response agentprotocol.LeaseDevicesResponse
	if err := c.doJSON(ctx, agentprotocol.RouteLeaseDevices, agentprotocol.LeaseDevicesRequest{
		Identity: c.identity(requestID), SandboxID: identity.SandboxUID, Image: spec.Image,
		MemSizeMiB: defaultMemoryMiB(spec.Memory), RootfsWritable: false,
	}, &response); err != nil {
		return Lease{}, err
	}
	return Lease{
		LeaseID: response.LeaseID, SandboxID: identity.SandboxUID, Image: spec.Image,
		PodUID: c.podUID, Namespace: c.namespace,
		RootfsDev: response.RootfsDev, MemDev: response.MemDev, CreatedAt: time.Now(),
	}, nil
}

func (c *agentHTTPClient) ReleaseDevices(ctx context.Context, requestID, leaseID string) error {
	return c.doJSON(ctx, agentprotocol.RouteReleaseDevices, agentprotocol.ReleaseDevicesRequest{
		Identity: c.identity(requestID), LeaseID: leaseID,
	}, nil)
}

func (c *agentHTTPClient) ListLeases(ctx context.Context) ([]Lease, error) {
	var response agentprotocol.ListLeasesResponse
	if err := c.doJSON(ctx, agentprotocol.RouteListLeases, c.identity(""), &response); err != nil {
		return nil, err
	}
	return append([]Lease(nil), response.Leases...), nil
}

func (c *agentHTTPClient) Compatibility(ctx context.Context) (string, error) {
	var response agentprotocol.CompatibilityResponse
	if err := c.doJSON(ctx, agentprotocol.RouteCompatibility, c.identity(""), &response); err != nil {
		return "", err
	}
	return response.CompatibilityClass, nil
}

func (c *agentHTTPClient) Health(ctx context.Context) error {
	var response agentprotocol.HealthResponse
	if err := c.doJSON(ctx, agentprotocol.RouteHealth, c.identity(""), &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("runtime-agent reports unhealthy: leases=%d images=%d", response.LeaseCount, response.ImageCount)
	}
	return nil
}

func (c *agentHTTPClient) identity(requestID string) agentprotocol.Identity {
	return agentprotocol.Identity{RequestID: requestID, Namespace: c.namespace, PodUID: c.podUID}
}

// doJSON performs one RPC and maps the wire errors onto runtime contract
// errors: NotFound -> ErrImageNotReady, Unauthorized/Conflict -> invalid
// config, transport failures -> errAgentUnreachable.
func (c *agentHTTPClient) doJSON(ctx context.Context, path string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://firecracker-agent"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", errAgentUnreachable, err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxAgentResponseBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var wireError agentprotocol.ErrorResponse
		_ = json.NewDecoder(limited).Decode(&wireError)
		if wireError.Message == "" {
			wireError.Message = response.Status
		}
		switch {
		case response.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", runtimecontract.ErrImageNotReady, wireError.Message)
		case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w: runtime-agent rejected the caller: %s", runtimecontract.ErrInvalidConfig, wireError.Message)
		case response.StatusCode == http.StatusConflict:
			return fmt.Errorf("%w: runtime-agent conflict: %s", runtimecontract.ErrInvalidConfig, wireError.Message)
		default:
			return fmt.Errorf("runtime-agent %s failed: %s: %s", path, wireError.Code, wireError.Message)
		}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(output); err != nil {
		return fmt.Errorf("decode runtime-agent response: %w", err)
	}
	return nil
}
