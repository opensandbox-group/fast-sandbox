package runtime

import (
	"context"
	"fmt"
	"io"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/runtimecatalog"
)

type SandboxMetadata struct {
	api.SandboxSpec
	ContainerID string
	PID         int
	Phase       string
	CreatedAt   int64
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

// Runtime is retained as a source-compatible alias while callers migrate to
// the explicit RuntimeDriver name.
type Runtime = RuntimeDriver

// RuntimeLogReader is a temporary internal diagnostic capability. It is not a
// Fast Sandbox public data-plane contract and will move behind Fastlet Proxy.
type RuntimeLogReader interface {
	GetSandboxLogs(ctx context.Context, sandboxID string, follow bool, stdout io.Writer) error
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

type AccessDescriptorProvider interface {
	GetAccessDescriptor(sandboxID string) (fastletnetwork.AccessDescriptor, error)
}

type RoutePublication struct {
	Namespace         string
	SandboxUID        string
	FastletPodUID     string
	AssignmentAttempt int64
	RouteGeneration   int64
	Access            fastletnetwork.AccessDescriptor
}

// RoutePublisher is the only lifecycle-facing data-plane boundary. The
// concrete implementation talks to the separate Fastlet Proxy over the shared
// Pod-local UDS; runtime drivers remain unaware of proxy protocols.
type RoutePublisher interface {
	ApplyRoute(context.Context, RoutePublication) error
	RemoveRoute(context.Context, RoutePublication) error
	ReconcileRoutes(context.Context, []RoutePublication) error
}

type RuntimeType = apiv1alpha1.RuntimeName

const (
	RuntimeTypeContainer RuntimeType = "container"
	RuntimeTypeGVisor    RuntimeType = "gvisor"
	RuntimeTypeKataQemu  RuntimeType = "kata-qemu"
	RuntimeTypeKataFc    RuntimeType = "kata-fc"
	RuntimeTypeKataClh   RuntimeType = "kata-clh"
	RuntimeTypeBoxLite   RuntimeType = "boxlite"
)

// RuntimeConfig defines the configuration for each runtime type.
type RuntimeConfig struct {
	Handler     string // containerd runtime handler name
	RuntimePath string // optional absolute path to shim binary
	ConfigPath  string // configuration file path (optional)
	NeedsTTY    bool   // whether TTY is required for this runtime
	OptionsType string // TypeUrl for runtime options (optional, e.g., gVisor)
}

var sharedRuntimeCatalog = runtimecatalog.Builtin()

// GetRuntimeHandler returns the containerd runtime handler for the given type.
func GetRuntimeHandler(rt RuntimeType) string {
	if cfg, err := ResolveRuntimeConfig(rt, ""); err == nil {
		return cfg.Handler
	}
	return ""
}

// GetRuntimeConfig returns the full RuntimeConfig for the given type.
func GetRuntimeConfig(rt RuntimeType) RuntimeConfig {
	cfg, err := ResolveRuntimeConfig(rt, "")
	if err != nil {
		cfg, _ = ResolveRuntimeConfig(RuntimeTypeContainer, "")
	}
	return cfg
}

func ResolveRuntimeConfig(rt RuntimeType, handlerOverride string) (RuntimeConfig, error) {
	profile, err := sharedRuntimeCatalog.Resolve(rt)
	if err != nil || profile.Driver != runtimecatalog.DriverKindContainerd || profile.Containerd == nil {
		return RuntimeConfig{}, ErrUnsupportedRuntime
	}
	cfg := RuntimeConfig{
		Handler:     profile.Containerd.Handler,
		RuntimePath: profile.Containerd.RuntimePath,
		ConfigPath:  profile.Containerd.ConfigPath,
		NeedsTTY:    profile.Containerd.NeedsTTY,
		OptionsType: profile.Containerd.OptionsType,
	}
	// Legacy-only compatibility hook. Production Fastlets no longer read a
	// handler override from Pod environment; the RuntimeCatalog is authoritative.
	if handlerOverride != "" {
		cfg.Handler = handlerOverride
	}
	return cfg, nil
}

func NewRuntime(ctx context.Context, runtimeType RuntimeType, socketPath string) (Runtime, error) {
	driver, _, err := NewDriverFactory(sharedRuntimeCatalog, NewHostCapabilityProber()).Create(ctx, runtimeType, socketPath)
	return driver, err
}

func NewRuntimeWithHandler(ctx context.Context, runtimeType RuntimeType, socketPath, handlerOverride string) (Runtime, error) {
	if handlerOverride == "" {
		return NewRuntime(ctx, runtimeType, socketPath)
	}
	cfg, err := ResolveRuntimeConfig(runtimeType, handlerOverride)
	if err != nil {
		return nil, err
	}

	var rt Runtime
	switch runtimeType {
	case RuntimeTypeContainer, RuntimeTypeGVisor,
		RuntimeTypeKataQemu, RuntimeTypeKataFc, RuntimeTypeKataClh:
		rt = newContainerdRuntimeWithConfig(runtimeType, cfg)
	default:
		return nil, ErrUnsupportedRuntime
	}

	if err := rt.Initialize(ctx, socketPath); err != nil {
		return nil, err
	}
	if report := rt.ProbeCapabilities(ctx); report.State != runtimecatalog.CapabilityReady {
		_ = rt.Close()
		return nil, fmt.Errorf("runtime driver did not become ready: %s: %s", report.Reason, report.Message)
	}
	return rt, nil
}
