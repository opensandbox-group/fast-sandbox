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

// defaultRuntimeHandlers maps RuntimeType to containerd runtime handler.
var defaultRuntimeHandlers = map[RuntimeType]string{
	RuntimeTypeContainer: "io.containerd.runc.v2",
	RuntimeTypeGVisor:    "io.containerd.runsc.v1",
	RuntimeTypeKataQemu:  "io.containerd.kata-qemu.v2",
	RuntimeTypeKataFc:    "io.containerd.kata-fc.v2",
	RuntimeTypeKataClh:   "io.containerd.kata-clh.v2",
}

// GetRuntimeHandler returns the containerd runtime handler for the given type.
func GetRuntimeHandler(rt RuntimeType) string {
	if handler, ok := defaultRuntimeHandlers[rt]; ok {
		return handler
	}
	return defaultRuntimeHandlers[RuntimeTypeContainer]
}

func NewRuntime(ctx context.Context, runtimeType RuntimeType, socketPath string) (Runtime, error) {
	handler := GetRuntimeHandler(runtimeType)

	var rt Runtime
	switch runtimeType {
	case RuntimeTypeContainer, RuntimeTypeGVisor,
		RuntimeTypeKataQemu, RuntimeTypeKataFc, RuntimeTypeKataClh:
		rt = newContainerdRuntime(handler)
	default:
		return nil, ErrUnsupportedRuntime
	}

	if err := rt.Initialize(ctx, socketPath); err != nil {
		return nil, err
	}
	return rt, nil
}
