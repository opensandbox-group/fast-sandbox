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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/fastlet/reservations":
			var request ReserveSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "request-a", request.RequestID)
			require.NotEmpty(t, r.Header.Get("traceparent"))
			require.NoError(t, json.NewEncoder(w).Encode(ReserveSandboxResponse{ReservationToken: "token-a"}))
		case "/api/v2/fastlet/reservations/cancel":
			require.NoError(t, json.NewEncoder(w).Encode(CancelReservationResponse{Canceled: true}))
		case "/api/v2/fastlet/ensure":
			require.NoError(t, json.NewEncoder(w).Encode(EnsureSandboxResponse{Accepted: true}))
		case "/api/v2/fastlet/inspect":
			require.NoError(t, json.NewEncoder(w).Encode(InspectSandboxResponse{Sandbox: &SandboxStatus{Phase: "running"}}))
		case "/api/v2/fastlet/delete":
			require.NoError(t, json.NewEncoder(w).Encode(DeleteSandboxV2Response{Accepted: true}))
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
	identity := SandboxIdentity{RequestID: "request-a", SandboxUID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1}

	reserved, err := client.ReserveSandbox(ctx, endpoint, &ReserveSandboxRequest{RequestID: "request-a"})
	require.NoError(t, err)
	require.Equal(t, "token-a", reserved.ReservationToken)
	_, err = client.CancelReservation(ctx, endpoint, &CancelReservationRequest{RequestID: "request-a"})
	require.NoError(t, err)
	_, err = client.EnsureSandbox(ctx, endpoint, &EnsureSandboxRequest{Identity: identity})
	require.NoError(t, err)
	inspected, err := client.InspectSandbox(ctx, endpoint, &InspectSandboxRequest{Identity: identity})
	require.NoError(t, err)
	require.Equal(t, "running", inspected.Sandbox.Phase)
	_, err = client.DeleteSandboxV2(ctx, endpoint, &DeleteSandboxV2Request{Identity: identity})
	require.NoError(t, err)
	draining, err := client.SetDraining(ctx, endpoint, &SetDrainingRequest{Draining: true})
	require.NoError(t, err)
	require.True(t, draining.Draining)

	for _, want := range []string{
		"/api/v2/fastlet/reservations", "/api/v2/fastlet/reservations/cancel", "/api/v2/fastlet/ensure",
		"/api/v2/fastlet/inspect", "/api/v2/fastlet/delete", "/api/v2/fastlet/draining",
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
		require.NoError(t, json.NewEncoder(w).Encode(EnsureSandboxResponse{Error: &FastletError{
			Code: ErrorCapacityRejected, Message: "full", Retryable: true,
		}}))
	})
	client, endpoint := testFastletClient(t, handler)
	client.SetTimeout(2 * time.Second)
	require.Equal(t, 2*time.Second, client.timeout)
	response, err := client.EnsureSandbox(context.Background(), endpoint, &EnsureSandboxRequest{})
	require.Error(t, err)
	require.NotNil(t, response)
	var failure *FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, ErrorCapacityRejected, failure.Code)
	require.True(t, failure.Retryable)
}
