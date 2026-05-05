package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/fastlet/runtime"

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
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fastlet/create", s.handleCreate)
	mux.HandleFunc("/api/v1/fastlet/delete", s.handleDelete)
	mux.HandleFunc("/api/v1/fastlet/status", s.handleStatus)
	mux.HandleFunc("/api/v1/fastlet/logs", s.handleLogs)

	klog.InfoS("Starting fastlet HTTP server", "addr", s.addr)
	return http.ListenAndServe(s.addr, mux)
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

	images, err := s.sandboxManager.ListImages(r.Context())
	if err != nil {
		klog.ErrorS(err, "Warning: failed to list images")
		images = []string{}
	}
	sbStatuses := s.sandboxManager.GetSandboxStatuses(r.Context())
	nodeName := os.Getenv("NODE_NAME")
	status := api.FastletStatus{
		FastletID:       os.Getenv("POD_NAME"), // Use Pod Name as Fastlet ID
		NodeName:        nodeName,
		Capacity:        s.sandboxManager.GetCapacity(),
		Allocated:       len(sbStatuses),
		Images:          images,
		SandboxStatuses: sbStatuses,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}
