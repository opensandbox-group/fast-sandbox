package fastlet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/observability"
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
	identity := SandboxIdentity{SandboxUID: "sandbox-a", Namespace: "default", Name: "sandbox-a", RuntimeInstanceID: "runtime-a", FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/fastlet/create":
			var request CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "request-a", request.RequestID)
			require.NotEmpty(t, r.Header.Get("traceparent"))
			require.NoError(t, json.NewEncoder(w).Encode(CreateSandboxResponse{Disposition: CreateDispositionCreated}))
		case "/api/v2/fastlet/inspect":
			require.NoError(t, json.NewEncoder(w).Encode(InspectSandboxResponse{Sandbox: &SandboxStatus{
				Runtime: RuntimeObservation{State: RuntimeStateReady}, DataPlane: DataPlaneObservation{State: DataPlaneStateReady},
			}}))
		case "/api/v2/fastlet/delete":
			require.NoError(t, json.NewEncoder(w).Encode(DeleteSandboxResponse{}))
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
	_, err := client.CreateSandbox(ctx, endpoint, &CreateSandboxRequest{RequestID: "request-a", Identity: identity})
	require.NoError(t, err)
	inspected, err := client.InspectSandbox(ctx, endpoint, &InspectSandboxRequest{Identity: identity})
	require.NoError(t, err)
	require.Equal(t, RuntimeStateReady, inspected.Sandbox.Runtime.State)
	_, err = client.DeleteSandbox(ctx, endpoint, &DeleteSandboxRequest{Identity: identity})
	require.NoError(t, err)
	diagnostics, err := client.SandboxDiagnostics(ctx, endpoint, &SandboxDiagnosticsRequest{Identity: identity, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, "ready", diagnostics.Events[0].Message)
	draining, err := client.SetDraining(ctx, endpoint, &SetDrainingRequest{Draining: true})
	require.NoError(t, err)
	require.True(t, draining.Draining)

	for _, want := range []string{
		"/api/v2/fastlet/create",
		"/api/v2/fastlet/inspect", "/api/v2/fastlet/delete", "/api/v2/fastlet/diagnostics/sandbox",
		"/api/v2/fastlet/draining",
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
		require.NoError(t, json.NewEncoder(w).Encode(CreateSandboxResponse{Disposition: CreateDispositionRejectedBeforeSideEffects, Error: &FastletError{
			Code: ErrorCapacityRejected, Message: "full", Retryable: true,
		}}))
	})
	client, endpoint := testFastletClient(t, handler)
	client.SetTimeout(2 * time.Second)
	client.SetCreateTimeout(3 * time.Second)
	require.Equal(t, 2*time.Second, client.timeout)
	require.Equal(t, 3*time.Second, client.createTimeout)
	response, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{})
	require.Error(t, err)
	require.NotNil(t, response)
	var failure *FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, ErrorCapacityRejected, failure.Code)
	require.True(t, failure.Retryable)
	require.Equal(t, CreateDispositionRejectedBeforeSideEffects, CreateDispositionFromError(err))
}

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) { return string(s), nil }

type failingTokenSource struct{}

func (failingTokenSource) Token(context.Context) (string, error) {
	return "", errors.New("token mint failed")
}

func TestFastletClientAttachesControlAuthToken(t *testing.T) {
	var observed []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/fastlet/create":
			require.NoError(t, json.NewEncoder(w).Encode(CreateSandboxResponse{Disposition: CreateDispositionCreated}))
		case "/api/v2/fastlet/heartbeat":
			require.NoError(t, json.NewEncoder(w).Encode(HeartbeatResponse{FastletStatus: FastletStatus{RuntimeReady: true}}))
		default:
			http.NotFound(w, r)
		}
	})
	client, endpoint := testFastletClient(t, handler)
	client.SetTokenSource(staticTokenSource("token-a"))
	_, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{RequestID: "request-a"})
	require.NoError(t, err)
	_, err = client.Heartbeat(context.Background(), endpoint, &HeartbeatRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer token-a", "Bearer token-a"}, observed,
		"every call, heartbeat included, carries the control auth token")
}

func TestFastletClientFailsClosedWhenTokenMintFails(t *testing.T) {
	requests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(CreateSandboxResponse{Disposition: CreateDispositionCreated}))
	})
	client, endpoint := testFastletClient(t, handler)
	client.SetTokenSource(failingTokenSource{})
	_, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{RequestID: "request-a"})
	require.Error(t, err, "a token acquisition failure must fail the call closed")
	require.Equal(t, 0, requests, "no request may leave without a token")
}

func TestFastletClientPreservesStatusOnPlainTextRejection(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("fastlet control authentication is required"))
	})
	client, endpoint := testFastletClient(t, handler)
	_, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{RequestID: "request-a"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "status 401", "a non-JSON rejection must surface the HTTP status, not a decode error")
	require.Contains(t, err.Error(), "fastlet control authentication is required")
}

func TestFastletClientClassifiesAuthRejectionDisposition(t *testing.T) {
	authEnvelope := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "rejected", "retryable": status == 503}})
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/fastlet/create":
			authEnvelope(w, http.StatusForbidden, "AuthNotAllowed")
		case "/api/v2/fastlet/snapshots/create":
			authEnvelope(w, http.StatusServiceUnavailable, "AuthUnavailable")
		default:
			http.NotFound(w, r)
		}
	})
	client, endpoint := testFastletClient(t, handler)

	_, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{RequestID: "request-a"})
	require.Error(t, err)
	var callError *CreateCallError
	require.ErrorAs(t, err, &callError)
	var failure *FastletError
	require.ErrorAs(t, err, &failure)
	require.True(t, IsAuthFailure(failure), "an allow-list rejection must be classified as an authentication outcome")
	require.Equal(t, CreateDispositionRejectedBeforeSideEffects, callError.Disposition,
		"an auth rejection provably never reached admission, so the disposition must be retry-safe")

	_, err = client.CreateSnapshot(context.Background(), endpoint, &CreateSnapshotRequest{})
	require.ErrorAs(t, err, &callError)
	require.True(t, IsAuthFailure(callError.Failure))
	require.Equal(t, CreateDispositionRejectedBeforeSideEffects, callError.Disposition)
}

func TestFastletClientKeepsRuntimeUnknownOutcomeDistinct(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(CreateSandboxResponse{
			Disposition: CreateDispositionUnknown,
			Error:       &FastletError{Code: ErrorUnknownOutcome, Message: "runtime state uncertain", Retryable: true},
		})
	})
	client, endpoint := testFastletClient(t, handler)
	response, err := client.CreateSandbox(context.Background(), endpoint, &CreateSandboxRequest{RequestID: "request-a"})
	require.Error(t, err)
	var callError *CreateCallError
	require.ErrorAs(t, err, &callError)
	require.False(t, IsAuthFailure(callError.Failure), "a runtime UnknownOutcome must not be misclassified as authentication")
	require.Equal(t, CreateDispositionUnknown, callError.Disposition, "the Fastlet-reported disposition must be preserved")
	require.NotNil(t, response)
}

func TestFastletClientReportsOversizedResponseBodies(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxControlResponseBodyBytes+64))
	})
	client, endpoint := testFastletClient(t, handler)
	_, err := client.InspectSandbox(context.Background(), endpoint, &InspectSandboxRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds", "an oversized body must be reported as a size cap, not a decode error")
}
