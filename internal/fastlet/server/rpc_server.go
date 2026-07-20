package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/observability"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

// FastletServer handles HTTP requests from controller.
type FastletServer struct {
	addr           string
	sandboxManager *runtime.SandboxManager
}

// NewFastletServer creates a new fastlet HTTP server.
func NewFastletServer(addr string, sandboxManager *runtime.SandboxManager) *FastletServer {
	return &FastletServer{
		addr:           addr,
		sandboxManager: sandboxManager,
	}
}

// Start starts the HTTP server.
func (s *FastletServer) Start() error {
	return http.ListenAndServe(s.addr, s.Handler())
}

// Handler exposes the versioned Fastlet control protocol and legacy v1
// adapters. It is separated from Start so protocol behavior can be tested
// without opening a real listener.
func (s *FastletServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", s.handleReady)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/api/v1/fastlet/create", s.handleCreate)
	mux.HandleFunc("/api/v1/fastlet/delete", s.handleDelete)
	mux.HandleFunc("/api/v1/fastlet/status", s.handleStatus)
	mux.HandleFunc("/api/v1/fastlet/logs", s.handleLogs)
	mux.HandleFunc("/api/v2/fastlet/reservations", s.handleReserve)
	mux.HandleFunc("/api/v2/fastlet/reservations/cancel", s.handleCancelReservation)
	mux.HandleFunc("/api/v2/fastlet/ensure", s.handleEnsure)
	mux.HandleFunc("/api/v2/fastlet/inspect", s.handleInspect)
	mux.HandleFunc("/api/v2/fastlet/delete", s.handleDeleteV2)
	mux.HandleFunc("/api/v2/fastlet/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/api/v2/fastlet/runtime-diagnostics", s.handleRuntimeDiagnostics)
	mux.HandleFunc("/api/v2/fastlet/draining", s.handleSetDraining)

	klog.InfoS("Starting fastlet HTTP server", "addr", s.addr)
	return traceFastletAPI(mux)
}

func traceFastletAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/api/") {
			next.ServeHTTP(writer, request)
			return
		}
		request, span := observability.StartHTTPServer(request, "fastlet")
		defer observability.End(span, nil)
		next.ServeHTTP(writer, request)
	})
}

func (s *FastletServer) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.sandboxManager.Ready() {
		http.Error(w, "Fastlet recovery is incomplete", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *FastletServer) handleReserve(w http.ResponseWriter, r *http.Request) {
	var req api.ReserveSandboxRequest
	if !decodePost(w, r, &req) {
		return
	}
	r = r.WithContext(observability.WithIdentity(r.Context(), observability.Identity{
		RequestID: req.RequestID, Namespace: req.ClaimNamespace, SandboxName: req.ClaimName, FastletPodUID: req.FastletPodUID,
	}))
	response, err := s.sandboxManager.ReserveSandbox(&req)
	writeResponse(w, response, err)
}

func (s *FastletServer) handleCancelReservation(w http.ResponseWriter, r *http.Request) {
	var req api.CancelReservationRequest
	if !decodePost(w, r, &req) {
		return
	}
	r = r.WithContext(observability.WithIdentity(r.Context(), observability.Identity{RequestID: req.RequestID}))
	response, err := s.sandboxManager.CancelReservation(&req)
	writeResponse(w, response, err)
}

func (s *FastletServer) handleEnsure(w http.ResponseWriter, r *http.Request) {
	var req api.EnsureSandboxRequest
	if !decodePost(w, r, &req) {
		return
	}
	r = r.WithContext(withFastletRequestIdentity(r.Context(), req.Identity))
	r = r.WithContext(observability.WithIdentity(r.Context(), observability.Identity{
		Namespace: req.Sandbox.ClaimNamespace, SandboxName: req.Sandbox.ClaimName,
	}))
	response, err := s.sandboxManager.EnsureSandboxV2(r.Context(), &req)
	writeResponse(w, response, err)
}

func (s *FastletServer) handleInspect(w http.ResponseWriter, r *http.Request) {
	var req api.InspectSandboxRequest
	if !decodePost(w, r, &req) {
		return
	}
	r = r.WithContext(withFastletRequestIdentity(r.Context(), req.Identity))
	response, err := s.sandboxManager.InspectSandboxV2(&req)
	writeResponse(w, response, err)
}

func (s *FastletServer) handleDeleteV2(w http.ResponseWriter, r *http.Request) {
	var req api.DeleteSandboxV2Request
	if !decodePost(w, r, &req) {
		return
	}
	r = r.WithContext(withFastletRequestIdentity(r.Context(), req.Identity))
	response, err := s.sandboxManager.DeleteSandboxV2(&req)
	writeResponse(w, response, err)
}

func withFastletRequestIdentity(ctx context.Context, identity api.SandboxIdentity) context.Context {
	return observability.WithIdentity(ctx, observability.Identity{
		RequestID: identity.RequestID, SandboxUID: identity.SandboxUID, FastletPodUID: identity.FastletPodUID,
		InstanceGeneration: identity.InstanceGeneration, AssignmentAttempt: identity.AssignmentAttempt, RouteGeneration: identity.RouteGeneration,
	})
}

func (s *FastletServer) handleSetDraining(w http.ResponseWriter, r *http.Request) {
	var req api.SetDrainingRequest
	if !decodePost(w, r, &req) {
		return
	}
	s.sandboxManager.SetDraining(req.Draining, req.Reason)
	writeResponse(w, &api.SetDrainingResponse{Draining: req.Draining}, nil)
}

func (s *FastletServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cursor := api.CacheCursor{
		Epoch: r.URL.Query().Get("cacheEpoch"),
	}
	var err error
	if value := r.URL.Query().Get("cacheRevision"); value != "" {
		cursor.Revision, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			http.Error(w, "cacheRevision must be an unsigned integer", http.StatusBadRequest)
			return
		}
	}
	if value := r.URL.Query().Get("fullCache"); value != "" {
		cursor.ForceFull, err = strconv.ParseBool(value)
		if err != nil {
			http.Error(w, "fullCache must be a boolean", http.StatusBadRequest)
			return
		}
	}
	writeResponse(w, s.heartbeat(r, cursor), nil)
}

func (s *FastletServer) handleRuntimeDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeResponse(w, s.sandboxManager.RuntimeDiagnostics(r.Context()), nil)
}

func decodePost(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeResponse(w http.ResponseWriter, response any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(statusForFastletError(err))
	}
	if encodeErr := json.NewEncoder(w).Encode(response); encodeErr != nil {
		klog.ErrorS(encodeErr, "Encode Fastlet response")
	}
}

func statusForFastletError(err error) int {
	var failure *api.FastletError
	if !errors.As(err, &failure) {
		return http.StatusInternalServerError
	}
	switch failure.Code {
	case api.ErrorCapacityRejected:
		return http.StatusTooManyRequests
	case api.ErrorInProgress:
		return http.StatusAccepted
	case api.ErrorRuntimeUnavailable, api.ErrorNetworkUnavailable, api.ErrorInfraUnavailable, api.ErrorUnknownOutcome:
		return http.StatusServiceUnavailable
	case api.ErrorNotFound:
		return http.StatusNotFound
	default:
		return http.StatusConflict
	}
}

// handleLogs streams sandbox logs.
func (s *FastletServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sandboxID := r.URL.Query().Get("sandboxId")
	if sandboxID == "" {
		http.Error(w, "sandboxId is required", http.StatusBadRequest)
		return
	}
	follow := r.URL.Query().Get("follow") == "true"

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Transfer-Encoding", "chunked")

	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.f = f
	}

	if err := s.sandboxManager.GetLogs(r.Context(), sandboxID, follow, fw); err != nil {
		klog.ErrorS(err, "GetLogs failed", "sandbox", sandboxID)
		return
	}
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (n int, err error) {
	n, err = fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return
}

// handleCreate handles create sandbox requests.
func (s *FastletServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req api.CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := s.sandboxManager.CreateSandbox(r.Context(), &req.Sandbox)
	if err != nil {
		klog.ErrorS(err, "Create sandbox failed", "sandbox", req.Sandbox.SandboxID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDelete handles delete sandbox requests.
func (s *FastletServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req api.DeleteSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := s.sandboxManager.DeleteSandbox(req.SandboxID)
	if err != nil {
		klog.ErrorS(err, "Delete sandbox failed", "sandbox", req.SandboxID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleStatus handles status queries.
func (s *FastletServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	heartbeat := s.heartbeat(r, api.CacheCursor{ForceFull: true})
	heartbeat.Images = append([]string(nil), heartbeat.Cache.Images...)
	writeResponse(w, &heartbeat.FastletStatus, nil)
}

func (s *FastletServer) heartbeat(r *http.Request, cursor api.CacheCursor) api.HeartbeatResponse {
	cacheSnapshot, err := s.sandboxManager.CacheSnapshot(r.Context(), cursor)
	if err != nil {
		klog.ErrorS(err, "Warning: failed to refresh cache inventory")
	}
	sbStatuses := s.sandboxManager.GetSandboxStatuses(r.Context())
	nodeName := os.Getenv("NODE_NAME")
	admission, recovering, draining := s.sandboxManager.State()
	infraProfile, infraProfileHash, infraReady, preparedArtifacts, _ := s.sandboxManager.InfraStatus()
	status := api.FastletStatus{
		FastletID:           os.Getenv("POD_NAME"), // Use Pod Name as Fastlet ID
		NodeName:            nodeName,
		Capacity:            s.sandboxManager.GetCapacity(),
		Allocated:           len(sbStatuses),
		SandboxStatuses:     sbStatuses,
		Admission:           admission,
		RuntimeReady:        s.sandboxManager.RuntimeReady(),
		Recovering:          recovering,
		Draining:            draining,
		FastletPodUID:       s.sandboxManager.FastletPodUID(),
		ResourceProfileHash: s.sandboxManager.ResourceProfileHash(),
		InfraProfile:        infraProfile, InfraProfileHash: infraProfileHash,
		InfraReady: infraReady, PreparedArtifacts: preparedArtifacts,
	}
	return api.HeartbeatResponse{
		FastletStatus: status,
		Sequence:      s.sandboxManager.NextHeartbeatSequence(), ObservedAt: time.Now().UTC(),
		Cache: cacheSnapshot, Diagnostics: s.sandboxManager.RuntimeDiagnostics(r.Context()),
	}
}
