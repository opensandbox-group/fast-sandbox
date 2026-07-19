package boxlitewire

import (
	"time"

	"fast-sandbox/internal/api"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

const ProtocolVersionV1 = "v1"

const (
	CapabilityOwnerFence     = "owner-fence-v1"
	CapabilityArtifactVolume = "artifact-volume-v1"
	CapabilityLocalForward   = "local-forward-v1"
	CapabilityResourceLimit  = "resource-limits-v1"
	CapabilityRecovery       = "recovery-v1"
	CapabilityImageCache     = "image-cache-v1"
)

var RequiredCapabilities = []string{
	CapabilityOwnerFence,
	CapabilityArtifactVolume,
	CapabilityLocalForward,
	CapabilityResourceLimit,
	CapabilityRecovery,
	CapabilityImageCache,
}

const (
	ErrorInvalid               = "Invalid"
	ErrorNotFound              = "NotFound"
	ErrorConflict              = "Conflict"
	ErrorImmutableSpecConflict = "ImmutableSpecConflict"
	ErrorUnavailable           = "Unavailable"
	ErrorInternal              = "Internal"
)

type Capabilities struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Ready           bool            `json:"ready"`
	Reason          string          `json:"reason,omitempty"`
	Message         string          `json:"message,omitempty"`
	Capabilities    map[string]bool `json:"capabilities"`
}

type Artifact struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Options     []string `json:"options,omitempty"`
}

type EnsureRequest struct {
	Namespace       string          `json:"namespace"`
	Sandbox         api.SandboxSpec `json:"sandbox"`
	TunnelGuestPort uint32          `json:"tunnelGuestPort"`
	Artifacts       []Artifact      `json:"artifacts,omitempty"`
}

type Box struct {
	Sandbox                    api.SandboxSpec                    `json:"sandbox"`
	BoxID                      string                             `json:"boxId"`
	PID                        int                                `json:"pid,omitempty"`
	Phase                      string                             `json:"phase"`
	CreatedAt                  int64                              `json:"createdAt"`
	UserProcessStartedAt       time.Time                          `json:"userProcessStartedAt,omitempty"`
	UserProcessStartSource     api.UserProcessStartSource         `json:"userProcessStartSource,omitempty"`
	Access                     fastletnetwork.AccessDescriptor    `json:"access"`
	InfraServices              []fastletinfra.ServiceEndpoint     `json:"infraServices,omitempty"`
	InfraUpstreamHeadersByPort map[uint32]map[string]string       `json:"infraUpstreamHeadersByPort,omitempty"`
	InfraDiagnostics           []fastletinfra.ComponentDiagnostic `json:"infraDiagnostics,omitempty"`
}

type ListResponse struct {
	Boxes []Box `json:"boxes"`
}

type ImagesResponse struct {
	Images []string `json:"images"`
}

type PullRequest struct {
	Image string `json:"image"`
}

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
