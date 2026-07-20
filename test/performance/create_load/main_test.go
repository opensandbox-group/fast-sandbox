package main

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeFastPath struct {
	mu       sync.Mutex
	requests []*fastpathv1.CreateRequest
	deleted  []string
	failAt   map[string]bool
}

func (f *fakeFastPath) CreateSandbox(_ context.Context, request *fastpathv1.CreateRequest, _ ...grpc.CallOption) (*fastpathv1.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.failAt[request.RequestId] {
		return nil, status.Error(codes.ResourceExhausted, "full")
	}
	return &fastpathv1.CreateResponse{SandboxUid: "uid-" + request.RequestId, SandboxName: "sandbox-" + request.RequestId}, nil
}

func (f *fakeFastPath) DeleteSandbox(_ context.Context, request *fastpathv1.DeleteRequest, _ ...grpc.CallOption) (*fastpathv1.DeleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, request.SandboxName)
	return &fastpathv1.DeleteResponse{Success: true}, nil
}

func TestRunLoadReportsBoundedResultsAndCleanup(t *testing.T) {
	client := &fakeFastPath{failAt: map[string]bool{"load-2": true}}
	cfg := config{
		Endpoint: "fastpath:9090", Namespace: "load", Pool: "pool-a", Image: "alpine:latest",
		Command: "/bin/sh", Args: []string{"-c", "sleep 1"}, Requests: 5, Concurrency: 3,
		RequestTimeout: time.Second, RequestIDPrefix: "load", Cleanup: true, CleanupTimeout: time.Second,
		Runtime: "container", InfraProfile: "minimal", ImageState: "warm", ImageAffinity: "hit",
		NetworkSlotState: "clean", CreatePath: "fastpath", FastPathReplicas: 3, ControllerReplicas: 2, ProxyReplicas: 2,
	}
	report := runLoad(context.Background(), client, cfg)

	require.Equal(t, 4, report.Succeeded)
	require.Equal(t, 1, report.Failed)
	require.Equal(t, 5, report.Attempted)
	require.Zero(t, report.NotAttempted)
	require.Equal(t, 4, report.GRPCCodes[codes.OK.String()])
	require.Equal(t, 1, report.GRPCCodes[codes.ResourceExhausted.String()])
	require.Equal(t, 5, report.CreateRPCLatency.Samples)
	require.Equal(t, 4, report.SuccessfulCreateRPCLatency.Samples)
	require.Equal(t, 4, report.Identity.UniqueSandboxUIDs)
	require.Zero(t, report.Identity.DuplicateSandboxUIDs)
	require.Zero(t, report.Identity.MissingSandboxUIDs)
	require.NotNil(t, report.Cleanup)
	require.Equal(t, 4, report.Cleanup.Succeeded)
	require.Len(t, client.deleted, 4)

	seen := make(map[string]bool)
	for _, request := range client.requests {
		require.False(t, seen[request.RequestId])
		seen[request.RequestId] = true
		require.Equal(t, cfg.Namespace, request.Namespace)
		require.Equal(t, cfg.Pool, request.PoolRef)
	}
}

func TestParseConfigProvidesSafeExplicitDefaults(t *testing.T) {
	cfg, err := parseConfig(nil, &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, "fastpath", cfg.CreatePath)
	require.Equal(t, "unspecified", cfg.ImageState)
	require.Equal(t, []string{"-c", "sleep 3600"}, cfg.Args)
	require.NotEmpty(t, cfg.RequestIDPrefix)
}

func TestRunLoadSeparatesCanceledBeforeAttemptFromRPCLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := runLoad(ctx, &fakeFastPath{}, config{
		Requests: 3, Concurrency: 2, Rate: 1, RequestTimeout: time.Second,
		RequestIDPrefix: "cancelled", Namespace: "default", Pool: "pool-a", Image: "alpine:latest",
	})
	require.Zero(t, report.Attempted)
	require.Equal(t, 3, report.NotAttempted)
	require.Zero(t, report.CreateRPCLatency.Samples)
	require.Equal(t, 3, report.GRPCCodes[codes.Canceled.String()])
}

func TestSummarizeLatenciesUsesNearestRank(t *testing.T) {
	values := make([]time.Duration, 100)
	for index := range values {
		values[index] = time.Duration(index+1) * time.Millisecond
	}
	summary := summarizeLatencies(values)
	require.Equal(t, 50.0, summary.P50)
	require.Equal(t, 95.0, summary.P95)
	require.Equal(t, 99.0, summary.P99)
	require.Equal(t, 100.0, summary.Max)
}

func TestValidateConfigRejectsUnsafeOrAmbiguousLoad(t *testing.T) {
	base := config{
		Endpoint: "fastpath:9090", Namespace: "default", Pool: "pool-a", Image: "alpine:latest",
		Requests: 10, Concurrency: 2, RequestTimeout: time.Second, CleanupTimeout: time.Second, RequestIDPrefix: "load-a",
		CreatePath: "fastpath", ImageState: "unspecified", ImageAffinity: "unspecified", NetworkSlotState: "unspecified",
	}
	require.NoError(t, validateConfig(base))
	badConcurrency := base
	badConcurrency.Concurrency = 11
	require.ErrorContains(t, validateConfig(badConcurrency), "concurrency")
	badPrefix := base
	badPrefix.RequestIDPrefix = "unsafe prefix"
	require.ErrorContains(t, validateConfig(badPrefix), "whitespace")
}
