package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fast-sandbox/internal/observability"

	"github.com/stretchr/testify/require"
)

func testFastletClient(t *testing.T, handler http.Handler) (*FastletClient, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	address := server.Listener.Addr().(*net.TCPAddr)
	return NewFastletClient(address.Port), address.IP.String()
}

func TestFastletClientAdmissionEndpoints(t *testing.T) {
	paths := make(chan string, 8)
	identity := SandboxIdentity{RequestID: "request-a", SandboxUID: "sandbox-a", RuntimeInstanceID: "runtime-a", FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/fastlet/create":
			var request CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "request-a", request.Identity.RequestID)
			require.NotEmpty(t, r.Header.Get("traceparent"))
			require.NoError(t, json.NewEncoder(w).Encode(CreateSandboxResponse{Accepted: true}))
		case "/api/v2/fastlet/inspect":
			require.NoError(t, json.NewEncoder(w).Encode(InspectSandboxResponse{Sandbox: &SandboxStatus{Phase: "running"}}))
		case "/api/v2/fastlet/delete":
			require.NoError(t, json.NewEncoder(w).Encode(DeleteSandboxV2Response{Accepted: true}))
		case "/api/v2/fastlet/diagnostics/sandbox":
			var request SandboxDiagnosticsRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, identity.RuntimeInstanceID, request.Identity.RuntimeInstanceID)
			require.NoError(t, json.NewEncoder(w).Encode(SandboxDiagnosticsResponse{Events: []SandboxDiagnosticEvent{{Source: "runtime", Message: "ready"}}}))
		case "/api/v2/fastlet/draining":
			require.NoError(t, json.NewEncoder(w).Encode(SetDrainingResponse{Draining: true}))
		default:
			http.NotFound(w, r)
		}
	})
	client, endpoint := testFastletClient(t, handler)
	traceHeaders := make(http.Header)
	traceHeaders.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := observability.ExtractHTTP(context.Background(), traceHeaders)
	_, err := client.CreateSandbox(ctx, endpoint, &CreateSandboxRequest{Identity: identity})
	require.NoError(t, err)
	inspected, err := client.InspectSandbox(ctx, endpoint, &InspectSandboxRequest{Identity: identity})
	require.NoError(t, err)
	require.Equal(t, "running", inspected.Sandbox.Phase)
	_, err = client.DeleteSandboxV2(ctx, endpoint, &DeleteSandboxV2Request{Identity: identity})
	require.NoError(t, err)
	diagnostics, err := client.SandboxDiagnostics(ctx, endpoint, &SandboxDiagnosticsRequest{Identity: identity, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, "ready", diagnostics.Events[0].Message)
	draining, err := client.SetDraining(ctx, endpoint, &SetDrainingRequest{Draining: true})
	require.NoError(t, err)
	require.True(t, draining.Draining)

	for _, want := range []string{
		"/api/v2/fastlet/create",
		"/api/v2/fastlet/inspect", "/api/v2/fastlet/delete", "/api/v2/fastlet/diagnostics/sandbox", "/api/v2/fastlet/draining",
	} {
		require.Equal(t, want, <-paths)
	}
}

func TestFastletClientHeartbeatAndDiagnostics(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/fastlet/heartbeat":
			require.Equal(t, "boot-a", r.URL.Query().Get("cacheEpoch"))
			require.Equal(t, "3", r.URL.Query().Get("cacheRevision"))
			require.Equal(t, "true", r.URL.Query().Get("fullCache"))
			require.NoError(t, json.NewEncoder(w).Encode(HeartbeatResponse{FastletStatus: FastletStatus{FastletPodUID: "pod-a", RuntimeReady: true}}))
		case "/api/v2/fastlet/runtime-diagnostics":
			require.NoError(t, json.NewEncoder(w).Encode(RuntimeDiagnostics{State: "Ready"}))
		default:
			http.NotFound(w, r)
		}
	})
	client, endpoint := testFastletClient(t, handler)
	heartbeat, err := client.Heartbeat(context.Background(), endpoint, &HeartbeatRequest{Cache: CacheCursor{Epoch: "boot-a", Revision: 3, ForceFull: true}})
	require.NoError(t, err)
	require.True(t, heartbeat.RuntimeReady)
	diagnostics, err := client.RuntimeDiagnostics(context.Background(), endpoint)
	require.NoError(t, err)
	require.Equal(t, "Ready", diagnostics.State)
}

func TestFastletClientPreservesStructuredErrorsAndTimeout(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		require.NoError(t, json.NewEncoder(w).Encode(CreateSandboxResponse{Error: &FastletError{
			Code: ErrorCapacityRejected, Message: "full", Retryable: true,
		}}))
	})
	client, endpoint := testFastletClient(t, handler)
	client.SetTimeout(2 * time.Second)
	require.Equal(t, 2*time.Second, client.timeout)
	response, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{})
	require.Error(t, err)
	require.NotNil(t, response)
	var failure *FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, ErrorCapacityRejected, failure.Code)
	require.True(t, failure.Retryable)
}
