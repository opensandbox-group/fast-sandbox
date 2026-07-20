package cmd

import (
	"context"
	"os"
	"testing"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/spf13/viper"
	"google.golang.org/grpc"
)

type MockClient struct {
	fastpathv1.UnimplementedFastPathServiceServer
	CreateFunc func(ctx context.Context, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error)
}

func (m *MockClient) CreateSandbox(ctx context.Context, in *fastpathv1.CreateRequest, opts ...grpc.CallOption) (*fastpathv1.CreateResponse, error) {
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, in)
	}
	return &fastpathv1.CreateResponse{}, nil
}

func (m *MockClient) DeleteSandbox(ctx context.Context, in *fastpathv1.DeleteRequest, opts ...grpc.CallOption) (*fastpathv1.DeleteResponse, error) {
	return &fastpathv1.DeleteResponse{Success: true}, nil
}
func (m *MockClient) ListSandboxes(ctx context.Context, in *fastpathv1.ListRequest, opts ...grpc.CallOption) (*fastpathv1.ListResponse, error) {
	return &fastpathv1.ListResponse{}, nil
}
func (m *MockClient) GetSandbox(ctx context.Context, in *fastpathv1.GetRequest, opts ...grpc.CallOption) (*fastpathv1.SandboxInfo, error) {
	return &fastpathv1.SandboxInfo{}, nil
}
func (m *MockClient) UpdateSandbox(ctx context.Context, in *fastpathv1.UpdateRequest, opts ...grpc.CallOption) (*fastpathv1.UpdateResponse, error) {
	return &fastpathv1.UpdateResponse{}, nil
}
func (m *MockClient) ResolveEndpoint(ctx context.Context, in *fastpathv1.ResolveEndpointRequest, opts ...grpc.CallOption) (*fastpathv1.ResolveEndpointResponse, error) {
	return &fastpathv1.ResolveEndpointResponse{}, nil
}
func (m *MockClient) IssueRouteCredential(ctx context.Context, in *fastpathv1.IssueRouteCredentialRequest, opts ...grpc.CallOption) (*fastpathv1.IssueRouteCredentialResponse, error) {
	return &fastpathv1.IssueRouteCredentialResponse{}, nil
}

func TestRunCommand(t *testing.T) {
	mockClient := &MockClient{}
	clientFactory = func() (fastpathv1.FastPathServiceClient, *grpc.ClientConn, error) {
		return mockClient, nil, nil
	}

	var capturedReq *fastpathv1.CreateRequest
	mockClient.CreateFunc = func(ctx context.Context, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
		capturedReq = req
		return &fastpathv1.CreateResponse{
			SandboxId:  "test-sb-id",
			FastletPod: "test-fastlet",
		}, nil
	}

	viper.Reset()
	viper.Set("namespace", "test-ns")

	pool = ""
	mode = ""
	image = ""
	ports = nil

	rootCmd.SetArgs([]string{"run", "my-sandbox", "--image=alpine", "--pool=test-pool", "--mode=strong"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if capturedReq == nil {
		t.Fatal("CreateSandbox was not called")
	}
	if capturedReq.Name != "my-sandbox" {
		t.Errorf("expected name 'my-sandbox', got '%s'", capturedReq.Name)
	}
	if capturedReq.Image != "alpine" {
		t.Errorf("expected image 'alpine', got '%s'", capturedReq.Image)
	}
	if capturedReq.RequestId == "" {
		t.Error("expected an automatically generated request_id")
	}
	if capturedReq.ConsistencyMode != fastpathv1.ConsistencyMode_FAST || len(capturedReq.ExposedPorts) != 0 {
		t.Errorf("deprecated create fields must not be sent: mode=%s ports=%v", capturedReq.ConsistencyMode, capturedReq.ExposedPorts)
	}
	// ... other assert
}

func TestRunCommandWithFile(t *testing.T) {
	mockClient := &MockClient{}
	clientFactory = func() (fastpathv1.FastPathServiceClient, *grpc.ClientConn, error) {
		return mockClient, nil, nil // nil conn
	}
	var capturedReq *fastpathv1.CreateRequest
	mockClient.CreateFunc = func(ctx context.Context, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
		capturedReq = req
		return &fastpathv1.CreateResponse{}, nil
	}

	tmpFile, _ := os.CreateTemp("", "config.yaml")
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString(`
image: nginx
pool_ref: file-pool
consistency_mode: fast
`)
	tmpFile.Close()

	pool = ""
	image = ""

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
	if capturedReq.ConsistencyMode != fastpathv1.ConsistencyMode_FAST || len(capturedReq.ExposedPorts) != 0 {
		t.Errorf("legacy config fields must be ignored: mode=%s ports=%v", capturedReq.ConsistencyMode, capturedReq.ExposedPorts)
	}
}
