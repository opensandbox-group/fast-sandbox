package testutil

import (
	"context"
	"sync"

	"fast-sandbox/internal/api"
	fastletruntime "fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/runtimecatalog"
)

var _ fastletruntime.RuntimeDriver = (*FakeRuntime)(nil)

// FakeRuntime provides deterministic hooks for Fastlet lifecycle tests. It is
// intentionally kept behind the current Runtime interface and will evolve with
// RuntimeDriver rather than leaking runtime-specific behavior into tests.
type FakeRuntime struct {
	mu sync.Mutex

	EnsureFunc func(context.Context, *api.SandboxSpec) (*fastletruntime.SandboxMetadata, error)
	DeleteFunc func(context.Context, string) error
	StatusFunc func(context.Context, string) (string, error)
	ImagesFunc func(context.Context) ([]string, error)

	Namespace   string
	EnsureCalls []api.SandboxSpec
	DeleteCalls []string
}

func (f *FakeRuntime) Initialize(context.Context, string) error { return nil }

func (f *FakeRuntime) ProbeCapabilities(context.Context) fastletruntime.CapabilityReport {
	return fastletruntime.CapabilityReport{State: runtimecatalog.CapabilityReady}
}

func (f *FakeRuntime) SetNamespace(namespace string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Namespace = namespace
}

func (f *FakeRuntime) EnsureSandbox(ctx context.Context, spec *api.SandboxSpec) (*fastletruntime.SandboxMetadata, error) {
	f.mu.Lock()
	f.EnsureCalls = append(f.EnsureCalls, *spec)
	f.mu.Unlock()
	if f.EnsureFunc != nil {
		return f.EnsureFunc(ctx, spec)
	}
	return &fastletruntime.SandboxMetadata{SandboxSpec: *spec, ContainerID: spec.SandboxID, Phase: "running"}, nil
}

func (f *FakeRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	f.mu.Lock()
	f.DeleteCalls = append(f.DeleteCalls, sandboxID)
	f.mu.Unlock()
	if f.DeleteFunc != nil {
		return f.DeleteFunc(ctx, sandboxID)
	}
	return nil
}

func (f *FakeRuntime) ListImages(ctx context.Context) ([]string, error) {
	if f.ImagesFunc != nil {
		return f.ImagesFunc(ctx)
	}
	return nil, nil
}

func (f *FakeRuntime) PullImage(context.Context, string) error { return nil }

func (f *FakeRuntime) GetSandboxStatus(ctx context.Context, sandboxID string) (string, error) {
	if f.StatusFunc != nil {
		return f.StatusFunc(ctx, sandboxID)
	}
	return "running", nil
}

func (f *FakeRuntime) InspectSandbox(ctx context.Context, sandboxID string) (*fastletruntime.SandboxMetadata, error) {
	status, err := f.GetSandboxStatus(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	return &fastletruntime.SandboxMetadata{SandboxSpec: api.SandboxSpec{SandboxID: sandboxID}, Phase: status}, nil
}

func (f *FakeRuntime) ListManagedSandboxes(context.Context) ([]*fastletruntime.SandboxMetadata, error) {
	return nil, nil
}

func (f *FakeRuntime) Close() error { return nil }
