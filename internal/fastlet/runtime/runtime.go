package runtime

import (
	"context"
	"time"

	"fast-sandbox/internal/api"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

type SandboxMetadata struct {
	api.SandboxSpec
	ContainerID                string
	PID                        int
	Phase                      string
	CreatedAt                  int64
	UserProcessStartedAt       time.Time
	UserProcessStartSource     api.UserProcessStartSource
	InfraServices              []fastletinfra.ServiceEndpoint
	InfraUpstreamHeadersByPort map[uint32]map[string]string
	InfraDiagnostics           []fastletinfra.ComponentDiagnostic
}

// RuntimeDriver is the runtime-neutral lifecycle boundary used by Fastlet.
// User data-plane protocols such as Exec/File/PTY are deliberately excluded.
type RuntimeDriver interface {
	Initialize(ctx context.Context, socketPath string) error
	SetNamespace(ns string)
	ProbeCapabilities(ctx context.Context) CapabilityReport
	EnsureSandbox(ctx context.Context, config *api.SandboxSpec) (*SandboxMetadata, error)
	InspectSandbox(ctx context.Context, sandboxID string) (*SandboxMetadata, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error)
	Close() error
}

// RuntimeArtifactCache is an optional cache capability implemented by drivers
// that use pullable image/artifact stores.
type RuntimeArtifactCache interface {
	ListImages(ctx context.Context) ([]string, error)
	PullImage(ctx context.Context, image string) error
}

// RuntimeResourceRecoverer reconciles runtime-owned or Fastlet-owned durable
// resources before admission readiness is enabled.
type RuntimeResourceRecoverer interface {
	RecoverRuntimeResources(ctx context.Context, managed []*SandboxMetadata) error
}

// RuntimeResourceAdmission reports whether a new runtime resource can be
// acquired now. Fixed Pool capacity remains enforced by SandboxManager.
type RuntimeResourceAdmission interface {
	RuntimeResourceAvailable() bool
}

// NetworkConfigurable is implemented by drivers using Fastlet-owned Linux
// network slots. Runtime-owned networking (for example BoxLite) does not
// implement this interface.
type NetworkConfigurable interface {
	SetNetworkManager(manager *fastletnetwork.Manager)
}

// InfraConfigurable is implemented by drivers that compile a prepared
// Runtime Augmentation plan into their native runtime spec.
type InfraConfigurable interface {
	SetInfraManager(manager *fastletinfra.Manager)
}

type AccessDescriptorProvider interface {
	GetAccessDescriptor(sandboxID string) (fastletnetwork.AccessDescriptor, error)
}

type RoutePublication struct {
	Namespace             string
	SandboxUID            string
	FastletPodUID         string
	AssignmentAttempt     int64
	RouteGeneration       int64
	Access                fastletnetwork.AccessDescriptor
	UpstreamHeadersByPort map[uint32]map[string]string
}

// RoutePublisher is the only lifecycle-facing data-plane boundary. The
// concrete implementation talks to the separate Fastlet Proxy over the shared
// Pod-local UDS; runtime drivers remain unaware of proxy protocols.
type RoutePublisher interface {
	ApplyRoute(context.Context, RoutePublication) error
	RemoveRoute(context.Context, RoutePublication) error
	ReconcileRoutes(context.Context, []RoutePublication) error
}

// RuntimeConfig defines the configuration for each runtime type.
type RuntimeConfig struct {
	Handler     string // containerd runtime handler name
	RuntimePath string // optional absolute path to shim binary
	ConfigPath  string // configuration file path (optional)
	NeedsTTY    bool   // whether TTY is required for this runtime
	OptionsType string // TypeUrl for runtime options (optional, e.g., gVisor)
}
