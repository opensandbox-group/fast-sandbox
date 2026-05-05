package runtime

import (
	"context"
	"io"

	"fast-sandbox/internal/api"
)

type SandboxMetadata struct {
	api.SandboxSpec
	ContainerID string
	PID         int
	Phase       string
	CreatedAt   int64
}

type Runtime interface {
	Initialize(ctx context.Context, socketPath string) error

	SetNamespace(ns string)

	CreateSandbox(ctx context.Context, config *api.SandboxSpec) (*SandboxMetadata, error)

	DeleteSandbox(ctx context.Context, sandboxID string) error

	GetSandboxLogs(ctx context.Context, sandboxID string, follow bool, stdout io.Writer) error

	ListImages(ctx context.Context) ([]string, error)

	PullImage(ctx context.Context, image string) error

	GetSandboxStatus(ctx context.Context, sandboxID string) (string, error)

	Close() error
}

type RuntimeType string

const (
	RuntimeTypeContainer RuntimeType = "container"
	RuntimeTypeGVisor    RuntimeType = "gvisor"
	RuntimeTypeKataQemu  RuntimeType = "kata-qemu"
	RuntimeTypeKataFc    RuntimeType = "kata-fc"
	RuntimeTypeKataClh   RuntimeType = "kata-clh"
)

// RuntimeConfig defines the configuration for each runtime type.
type RuntimeConfig struct {
	Handler     string // containerd runtime handler name
	RuntimePath string // optional absolute path to shim binary
	ConfigPath  string // configuration file path (optional)
	NeedsTTY    bool   // whether TTY is required for this runtime
	OptionsType string // TypeUrl for runtime options (optional, e.g., gVisor)
}

// runtimeConfigs maps RuntimeType to its complete configuration.
// This is the single source of truth for runtime-specific settings.
var runtimeConfigs = map[RuntimeType]RuntimeConfig{
	RuntimeTypeContainer: {
		Handler: "io.containerd.runc.v2",
	},
	RuntimeTypeGVisor: {
		Handler:     "io.containerd.runsc.v1",
		ConfigPath:  "/etc/containerd/runsc.toml",
		NeedsTTY:    true, // see: https://github.com/google/gvisor/issues/12198
		OptionsType: "io.containerd.runsc.v1.options",
	},
	RuntimeTypeKataQemu: {
		Handler:     "io.containerd.kata.v2",
		RuntimePath: "/opt/kata/bin/containerd-shim-kata-v2",
		ConfigPath:  "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml",
	},
	RuntimeTypeKataFc: {
		Handler:     "io.containerd.kata.v2",
		RuntimePath: "/opt/kata/bin/containerd-shim-kata-v2",
		ConfigPath:  "/opt/kata/share/defaults/kata-containers/configuration-fc.toml",
	},
	RuntimeTypeKataClh: {
		Handler:     "io.containerd.kata.v2",
		RuntimePath: "/opt/kata/bin/containerd-shim-kata-v2",
		ConfigPath:  "/opt/kata/share/defaults/kata-containers/configuration-clh.toml",
	},
}

// GetRuntimeHandler returns the containerd runtime handler for the given type.
func GetRuntimeHandler(rt RuntimeType) string {
	if cfg, ok := runtimeConfigs[rt]; ok {
		return cfg.Handler
	}
	return runtimeConfigs[RuntimeTypeContainer].Handler
}

// GetRuntimeConfig returns the full RuntimeConfig for the given type.
func GetRuntimeConfig(rt RuntimeType) RuntimeConfig {
	cfg, err := ResolveRuntimeConfig(rt, "")
	if err != nil {
		return runtimeConfigs[RuntimeTypeContainer]
	}
	return cfg
}

func ResolveRuntimeConfig(rt RuntimeType, handlerOverride string) (RuntimeConfig, error) {
	cfg, ok := runtimeConfigs[rt]
	if !ok {
		return RuntimeConfig{}, ErrUnsupportedRuntime
	}
	if handlerOverride != "" {
		cfg.Handler = handlerOverride
	}
	return cfg, nil
}

func NewRuntime(ctx context.Context, runtimeType RuntimeType, socketPath string) (Runtime, error) {
	return NewRuntimeWithHandler(ctx, runtimeType, socketPath, "")
}

func NewRuntimeWithHandler(ctx context.Context, runtimeType RuntimeType, socketPath, handlerOverride string) (Runtime, error) {
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
	return rt, nil
}
