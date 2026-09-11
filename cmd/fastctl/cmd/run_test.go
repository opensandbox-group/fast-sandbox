package cmd

import (
	"context"
	"os"
	"testing"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/viper"
	"google.golang.org/grpc"
)

type MockClient struct {
	fastpathv2.UnimplementedFastPathServiceServer
	CreateFunc      func(ctx context.Context, req *fastpathv2.CreateSandboxRequest) (*fastpathv2.CreateSandboxResponse, error)
	DiagnosticsFunc func(ctx context.Context, req *fastpathv2.SandboxDiagnosticsRequest) (*fastpathv2.SandboxDiagnosticsResponse, error)
}

func (m *MockClient) CreateSandbox(ctx context.Context, in *fastpathv2.CreateSandboxRequest, opts ...grpc.CallOption) (*fastpathv2.CreateSandboxResponse, error) {
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, in)
	}
	return &fastpathv2.CreateSandboxResponse{Sandbox: &fastpathv2.SandboxInfo{}}, nil
}

func (m *MockClient) DeleteSandbox(ctx context.Context, in *fastpathv2.DeleteRequest, opts ...grpc.CallOption) (*fastpathv2.DeleteResponse, error) {
	return &fastpathv2.DeleteResponse{}, nil
}
func (m *MockClient) ListSandboxes(ctx context.Context, in *fastpathv2.ListSandboxesRequest, opts ...grpc.CallOption) (*fastpathv2.ListSandboxesResponse, error) {
	return &fastpathv2.ListSandboxesResponse{}, nil
}
func (m *MockClient) GetSandbox(ctx context.Context, in *fastpathv2.GetSandboxRequest, opts ...grpc.CallOption) (*fastpathv2.GetSandboxResponse, error) {
	return &fastpathv2.GetSandboxResponse{Sandbox: &fastpathv2.SandboxInfo{
		Runtime: &fastpathv2.RuntimeInfo{State: fastpathv2.RuntimeState_RUNTIME_STATE_READY},
		Ready:   true,
	}}, nil
}
func (m *MockClient) GetSandboxDiagnostics(ctx context.Context, in *fastpathv2.SandboxDiagnosticsRequest, opts ...grpc.CallOption) (*fastpathv2.SandboxDiagnosticsResponse, error) {
	if m.DiagnosticsFunc != nil {
		return m.DiagnosticsFunc(ctx, in)
	}
	return &fastpathv2.SandboxDiagnosticsResponse{}, nil
}
func (m *MockClient) UpdateSandbox(ctx context.Context, in *fastpathv2.UpdateSandboxRequest, opts ...grpc.CallOption) (*fastpathv2.UpdateSandboxResponse, error) {
	return &fastpathv2.UpdateSandboxResponse{}, nil
}
func (m *MockClient) ResolveEndpoint(ctx context.Context, in *fastpathv2.ResolveEndpointRequest, opts ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
	return &fastpathv2.ResolveEndpointResponse{}, nil
}
func (m *MockClient) GetPool(ctx context.Context, in *fastpathv2.GetPoolRequest, opts ...grpc.CallOption) (*fastpathv2.PoolInfo, error) {
	return &fastpathv2.PoolInfo{}, nil
}
func (m *MockClient) ListPools(ctx context.Context, in *fastpathv2.ListPoolsRequest, opts ...grpc.CallOption) (*fastpathv2.ListPoolsResponse, error) {
	return &fastpathv2.ListPoolsResponse{}, nil
}
func (m *MockClient) CreateSandboxSnapshot(ctx context.Context, in *fastpathv2.CreateSandboxSnapshotRequest, opts ...grpc.CallOption) (*fastpathv2.CreateSandboxSnapshotResponse, error) {
	return &fastpathv2.CreateSandboxSnapshotResponse{Snapshot: &fastpathv2.SandboxSnapshotInfo{}}, nil
}
func (m *MockClient) GetSandboxSnapshot(ctx context.Context, in *fastpathv2.GetSandboxSnapshotRequest, opts ...grpc.CallOption) (*fastpathv2.GetSandboxSnapshotResponse, error) {
	return &fastpathv2.GetSandboxSnapshotResponse{Snapshot: &fastpathv2.SandboxSnapshotInfo{}}, nil
}
func (m *MockClient) DeleteSandboxSnapshot(ctx context.Context, in *fastpathv2.DeleteSandboxSnapshotRequest, opts ...grpc.CallOption) (*fastpathv2.DeleteSandboxSnapshotResponse, error) {
	return &fastpathv2.DeleteSandboxSnapshotResponse{}, nil
}

func TestRunCommand(t *testing.T) {
	mockClient := &MockClient{}
	clientFactory = func() (fastpathv2.FastPathServiceClient, *grpc.ClientConn, error) {
		return mockClient, nil, nil
	}

	var capturedReq *fastpathv2.CreateSandboxRequest
	mockClient.CreateFunc = func(ctx context.Context, req *fastpathv2.CreateSandboxRequest) (*fastpathv2.CreateSandboxResponse, error) {
		capturedReq = req
		return &fastpathv2.CreateSandboxResponse{Sandbox: &fastpathv2.SandboxInfo{
			Identity: &fastpathv2.SandboxIdentity{Uid: "test-sb-id", Name: "my-sandbox"}, Ready: true,
		}}, nil
	}

	viper.Reset()
	viper.Set("namespace", "test-ns")

	pool = ""
	image = ""
	requestID = ""

	rootCmd.SetArgs([]string{"run", "my-sandbox", "--image=alpine", "--pool=test-pool"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if capturedReq == nil {
		t.Fatal("CreateSandbox was not called")
	}
	if capturedReq.Image != "alpine" {
		t.Errorf("expected image 'alpine', got '%s'", capturedReq.Image)
	}
	if capturedReq.RequestId != "my-sandbox" {
		t.Errorf("expected request_id to equal Sandbox name, got %q", capturedReq.RequestId)
	}
	// ... other assert
}

func TestRunCommandWithFile(t *testing.T) {
	mockClient := &MockClient{}
	clientFactory = func() (fastpathv2.FastPathServiceClient, *grpc.ClientConn, error) {
		return mockClient, nil, nil // nil conn
	}
	var capturedReq *fastpathv2.CreateSandboxRequest
	mockClient.CreateFunc = func(ctx context.Context, req *fastpathv2.CreateSandboxRequest) (*fastpathv2.CreateSandboxResponse, error) {
		capturedReq = req
		return &fastpathv2.CreateSandboxResponse{Sandbox: &fastpathv2.SandboxInfo{}}, nil
	}

	tmpFile, _ := os.CreateTemp("", "config.yaml")
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString(`
image: nginx
pool_ref: file-pool
working_dir: /workspace
`)
	tmpFile.Close()

	pool = ""
	image = ""
	requestID = ""

	// exec: run my-sandbox -f config.yaml --pool=override-pool
	rootCmd.SetArgs([]string{"run", "my-sandbox", "-f", tmpFile.Name(), "--pool=override-pool"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if capturedReq.Image != "nginx" {
		t.Errorf("expected image 'nginx' (from file), got '%s'", capturedReq.Image)
	}
	if capturedReq.PoolRef != "override-pool" {
		t.Errorf("expected pool 'override-pool' (from flag), got '%s'", capturedReq.PoolRef)
	}
	if capturedReq.WorkingDir != "/workspace" {
		t.Errorf("expected working dir '/workspace', got '%s'", capturedReq.WorkingDir)
	}
}
