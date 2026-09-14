package cmd

import (
	"context"
	"testing"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/spf13/viper"
	"google.golang.org/grpc"
)

func TestPauseCommandTargetsSandbox(t *testing.T) {
	mockClient := &MockClient{}
	clientFactory = func() (fastpathv2.FastPathServiceClient, *grpc.ClientConn, error) {
		return mockClient, nil, nil
	}
	var capturedReq *fastpathv2.PauseSandboxRequest
	mockClient.PauseFunc = func(_ context.Context, req *fastpathv2.PauseSandboxRequest) (*fastpathv2.PauseSandboxResponse, error) {
		capturedReq = req
		return &fastpathv2.PauseSandboxResponse{
			Sandbox: &fastpathv2.SandboxInfo{State: fastpathv2.SandboxState_SANDBOX_STATE_PAUSED}, Generation: 2,
		}, nil
	}

	viper.Reset()
	viper.Set("namespace", "test-ns")
	rootCmd.SetArgs([]string{"pause", "my-sandbox"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if capturedReq == nil || capturedReq.Sandbox.GetNamespacedName().GetName() != "my-sandbox" {
		t.Fatalf("PauseSandbox was not called for my-sandbox: %+v", capturedReq)
	}
	if capturedReq.Sandbox.GetNamespacedName().GetNamespace() != "test-ns" {
		t.Errorf("expected namespace test-ns, got %q", capturedReq.Sandbox.GetNamespacedName().GetNamespace())
	}
}

func TestResumeCommandCarriesCheckpointFence(t *testing.T) {
	mockClient := &MockClient{}
	clientFactory = func() (fastpathv2.FastPathServiceClient, *grpc.ClientConn, error) {
		return mockClient, nil, nil
	}
	var capturedReq *fastpathv2.ResumeSandboxRequest
	mockClient.ResumeFunc = func(_ context.Context, req *fastpathv2.ResumeSandboxRequest) (*fastpathv2.ResumeSandboxResponse, error) {
		capturedReq = req
		return &fastpathv2.ResumeSandboxResponse{
			Sandbox: &fastpathv2.SandboxInfo{State: fastpathv2.SandboxState_SANDBOX_STATE_RUNNING}, Generation: 3,
		}, nil
	}

	viper.Reset()
	viper.Set("namespace", "test-ns")
	resumeCheckpointID = ""
	rootCmd.SetArgs([]string{"resume", "my-sandbox", "--checkpoint-id", "ckpt-1"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if capturedReq == nil || capturedReq.Sandbox.GetNamespacedName().GetName() != "my-sandbox" {
		t.Fatalf("ResumeSandbox was not called for my-sandbox: %+v", capturedReq)
	}
	if capturedReq.ExpectedCheckpointId != "ckpt-1" {
		t.Errorf("expected checkpoint fence ckpt-1, got %q", capturedReq.ExpectedCheckpointId)
	}
}
