package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"fast-sandbox/internal/api"
	fastletruntime "fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

type serverRuntime struct {
	mu        sync.Mutex
	sandboxes map[string]*fastletruntime.SandboxMetadata
	images    []string
}

func newServerRuntime() *serverRuntime {
	return &serverRuntime{sandboxes: make(map[string]*fastletruntime.SandboxMetadata), images: []string{"docker.io/library/alpine:latest"}}
}

func (*serverRuntime) Initialize(context.Context, string) error { return nil }
func (*serverRuntime) SetNamespace(string)                      {}
func (*serverRuntime) Close() error                             { return nil }
func (*serverRuntime) ProbeCapabilities(context.Context) fastletruntime.CapabilityReport {
	return fastletruntime.CapabilityReport{State: runtimecatalog.CapabilityReady, Reason: "TestRuntimeReady"}
}
func (r *serverRuntime) EnsureSandbox(_ context.Context, spec *api.SandboxSpec) (*fastletruntime.SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	metadata := &fastletruntime.SandboxMetadata{SandboxSpec: *spec, Phase: "running"}
	r.sandboxes[spec.SandboxID] = metadata
	copy := *metadata
	return &copy, nil
}
func (r *serverRuntime) InspectSandbox(_ context.Context, id string) (*fastletruntime.SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	metadata := r.sandboxes[id]
	if metadata == nil {
		return nil, fastletruntime.ErrSandboxNotFound
	}
	copy := *metadata
	return &copy, nil
}
func (r *serverRuntime) DeleteSandbox(_ context.Context, id string) error {
	r.mu.Lock()
	delete(r.sandboxes, id)
	r.mu.Unlock()
	return nil
}
func (r *serverRuntime) ListManagedSandboxes(context.Context) ([]*fastletruntime.SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*fastletruntime.SandboxMetadata, 0, len(r.sandboxes))
	for _, metadata := range r.sandboxes {
		copy := *metadata
		result = append(result, &copy)
	}
	return result, nil
}
func (r *serverRuntime) ListImages(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.images...), nil
}
func (r *serverRuntime) PullImage(_ context.Context, image string) error {
	r.mu.Lock()
	r.images = append(r.images, image)
	r.mu.Unlock()
	return nil
}

func newServerManager(t *testing.T, driver fastletruntime.RuntimeDriver, recoverOnStart bool) *fastletruntime.SandboxManager {
	t.Helper()
	manager, err := fastletruntime.NewSandboxManagerWithConfig(driver, fastletruntime.SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RecoverOnStart: recoverOnStart,
		TokenGenerator: func() (string, error) { return "reservation-a", nil },
	})
	require.NoError(t, err)
	return manager
}

func postJSON(t *testing.T, handler http.Handler, path string, request, response any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	if response != nil {
		require.NoError(t, json.NewDecoder(recorder.Body).Decode(response))
	}
	return recorder
}

func TestV2AdmissionProtocolAndHeartbeat(t *testing.T) {
	manager := newServerManager(t, newServerRuntime(), false)
	handler := NewFastletServer(":0", manager).Handler()

	var reserved api.ReserveSandboxResponse
	recorder := postJSON(t, handler, "/api/v2/fastlet/reservations", api.ReserveSandboxRequest{
		RequestID: "request-a", CreateSpecHash: "spec-a", ClaimNamespace: "default", ClaimName: "sandbox-a", FastletPodUID: "pod-uid-a",
	}, &reserved)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "reservation-a", reserved.ReservationToken)

	var ensured api.EnsureSandboxResponse
	recorder = postJSON(t, handler, "/api/v2/fastlet/ensure", api.EnsureSandboxRequest{
		Identity:         api.SandboxIdentity{RequestID: "request-a", SandboxUID: "sandbox-a", InstanceGeneration: 1, AssignmentAttempt: 1, FastletPodUID: "pod-uid-a"},
		ReservationToken: reserved.ReservationToken,
		CreateSpecHash:   "spec-a",
		Sandbox: api.SandboxSpec{
			ClaimUID: "claim-a", ClaimNamespace: "default", ClaimName: "sandbox-a", Image: "alpine:latest",
		},
	}, &ensured)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, ensured.Created)

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/fastlet/heartbeat", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var heartbeat api.HeartbeatResponse
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&heartbeat))
	require.Equal(t, 1, heartbeat.Admission.Running)
	require.True(t, heartbeat.RuntimeReady)
	require.Equal(t, "pod-uid-a", heartbeat.FastletPodUID)

	var rejected api.ReserveSandboxResponse
	recorder = postJSON(t, handler, "/api/v2/fastlet/reservations", api.ReserveSandboxRequest{
		RequestID: "request-b", CreateSpecHash: "spec-b", ClaimNamespace: "default", ClaimName: "sandbox-b", FastletPodUID: "pod-uid-a",
	}, &rejected)
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, api.ErrorCapacityRejected, rejected.Error.Code)
}

func TestReadinessIsFalseUntilRecoveryCompletes(t *testing.T) {
	manager := newServerManager(t, newServerRuntime(), true)
	handler := NewFastletServer(":0", manager).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NoError(t, manager.Recover(context.Background()))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestMetricsEndpoint(t *testing.T) {
	manager := newServerManager(t, newServerRuntime(), false)
	handler := NewFastletServer(":0", manager).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "go_goroutines")
}

func TestSetDrainingRejectsNewReservations(t *testing.T) {
	manager := newServerManager(t, newServerRuntime(), false)
	handler := NewFastletServer(":0", manager).Handler()
	var draining api.SetDrainingResponse
	recorder := postJSON(t, handler, "/api/v2/fastlet/draining", api.SetDrainingRequest{Draining: true, Reason: "upgrade"}, &draining)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, draining.Draining)
	var rejected api.ReserveSandboxResponse
	recorder = postJSON(t, handler, "/api/v2/fastlet/reservations", api.ReserveSandboxRequest{
		RequestID: "request-a", CreateSpecHash: "spec-a", ClaimNamespace: "default", ClaimName: "sandbox-a", FastletPodUID: "pod-uid-a",
	}, &rejected)
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Equal(t, api.ErrorDraining, rejected.Error.Code)
}

func TestHeartbeatUsesCacheCursor(t *testing.T) {
	manager := newServerManager(t, newServerRuntime(), false)
	handler := NewFastletServer(":0", manager).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/fastlet/heartbeat", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var first api.HeartbeatResponse
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&first))
	require.True(t, first.Cache.Full)
	require.Equal(t, []string{"alpine:latest"}, first.Cache.Images)

	recorder = httptest.NewRecorder()
	path := "/api/v2/fastlet/heartbeat?cacheEpoch=" + first.Cache.Epoch + "&cacheRevision=1"
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var unchanged api.HeartbeatResponse
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&unchanged))
	require.False(t, unchanged.Cache.Full)
	require.Empty(t, unchanged.Cache.Images)
	require.Greater(t, unchanged.Sequence, first.Sequence)

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/fastlet/heartbeat?cacheRevision=invalid", nil))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
