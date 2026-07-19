package testutil

import (
	"context"
	"sync"

	"fast-sandbox/internal/api"
)

var _ api.FastletAPIClient = (*FakeFastletClient)(nil)

// FakeFastletClient is an in-memory Fastlet/Heartbeat fake with optional hooks.
// Requests are copied into call history so concurrent state-machine tests can
// assert retries without sharing mutable request objects.
type FakeFastletClient struct {
	mu sync.Mutex

	CreateFunc    func(string, *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error)
	DeleteFunc    func(string, *api.DeleteSandboxRequest) (*api.DeleteSandboxResponse, error)
	HeartbeatFunc func(context.Context, string) (*api.FastletStatus, error)

	CreateCalls []api.CreateSandboxRequest
	DeleteCalls []api.DeleteSandboxRequest
}

func (f *FakeFastletClient) CreateSandbox(endpoint string, request *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
	f.mu.Lock()
	f.CreateCalls = append(f.CreateCalls, *request)
	f.mu.Unlock()
	if f.CreateFunc != nil {
		return f.CreateFunc(endpoint, request)
	}
	return &api.CreateSandboxResponse{Success: true, SandboxID: request.Sandbox.SandboxID}, nil
}

func (f *FakeFastletClient) DeleteSandbox(endpoint string, request *api.DeleteSandboxRequest) (*api.DeleteSandboxResponse, error) {
	f.mu.Lock()
	f.DeleteCalls = append(f.DeleteCalls, *request)
	f.mu.Unlock()
	if f.DeleteFunc != nil {
		return f.DeleteFunc(endpoint, request)
	}
	return &api.DeleteSandboxResponse{Success: true}, nil
}

func (f *FakeFastletClient) GetFastletStatus(ctx context.Context, endpoint string) (*api.FastletStatus, error) {
	if f.HeartbeatFunc != nil {
		return f.HeartbeatFunc(ctx, endpoint)
	}
	return &api.FastletStatus{}, nil
}
