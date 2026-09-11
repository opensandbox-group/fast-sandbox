package server

// Package server exposes the firecracker-runtime-agent management API as a
// versioned JSON-over-HTTP service on a Unix socket (design docs §2.2).
// The server layer enforces the caller identity (empty PodUID -> 403), the
// idempotency key contract (requestId required on mutating RPCs), routes
// the seven stage-1 RPCs, and maps backend errors onto stable wire codes;
// idempotency and the journal live in the state package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"

	"k8s.io/klog/v2"
)

const (
	// socketMode is the Unix socket permission: group-fast-sandbox only.
	socketMode = 0o660
	// socketGroup is the owning group of the socket.
	socketGroup = "fast-sandbox"
	// maxRequestBytes bounds a decoded request body.
	maxRequestBytes = 4 << 20
)

// Server is the UDS HTTP service. A nil Backend answers 500 ErrorInternal.
type Server struct {
	Backend    Backend
	socketPath string
	http       *http.Server
}

// New builds the UDS service. The socket is created on Serve.
func New(backend Backend, socketPath string) *Server {
	return &Server{Backend: backend, socketPath: socketPath}
}

// SocketPath returns the configured socket path.
func (s *Server) SocketPath() string {
	return s.socketPath
}

// Serve listens on the Unix socket and blocks until the server stops or the
// context is cancelled (then it shuts down gracefully). A stale socket file
// is removed first. Socket permissions are tightened to the fast-sandbox
// group; the group lookup is best effort (the daemon runs privileged, but a
// misconfigured host must not prevent startup).
func (s *Server) Serve(ctx context.Context) error {
	if s.socketPath == "" {
		return errors.New("runtime-agent socket path is required")
	}
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale runtime-agent socket: %w", err)
	}
	if err := os.MkdirAll(parentDir(s.socketPath), 0o750); err != nil {
		return fmt.Errorf("prepare runtime-agent socket directory: %w", err)
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on runtime-agent socket %s: %w", s.socketPath, err)
	}
	defer listener.Close()
	if err := os.Chmod(s.socketPath, socketMode); err != nil {
		return fmt.Errorf("chmod runtime-agent socket: %w", err)
	}
	if group, err := user.LookupGroup(socketGroup); err == nil {
		if gid, convErr := strconv.Atoi(group.Gid); convErr == nil {
			if err := os.Chown(s.socketPath, -1, gid); err != nil {
				klog.Warningf("runtime-agent socket group %q: %v", socketGroup, err)
			}
		}
	} else {
		klog.Warningf("runtime-agent socket group %q not found: %v", socketGroup, err)
	}

	s.http = &http.Server{Handler: s}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(listener) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// ServeHTTP routes the versioned RPCs. Every request carries the caller
// identity in its body; an empty PodUID is rejected (403).
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s.Backend == nil {
		writeError(writer, &Error{Code: agentprotocol.ErrorInternal, Message: "runtime-agent backend is not configured"})
		return
	}
	switch request.URL.Path {
	case agentprotocol.RoutePinImage:
		s.handleMutating(writer, request, func(ctx context.Context, payload json.RawMessage) (any, error) {
			var req agentprotocol.PinImageRequest
			if err := decodeRequest(payload, &req); err != nil {
				return nil, err
			}
			if err := validateIdentity(req.Identity, true); err != nil {
				return nil, err
			}
			if strings.TrimSpace(req.Image) == "" {
				return nil, invalidRequest("image reference is required")
			}
			return s.Backend.PinImage(ctx, req)
		})
	case agentprotocol.RouteUnpinImage:
		s.handleMutating(writer, request, func(ctx context.Context, payload json.RawMessage) (any, error) {
			var req agentprotocol.UnpinImageRequest
			if err := decodeRequest(payload, &req); err != nil {
				return nil, err
			}
			if err := validateIdentity(req.Identity, true); err != nil {
				return nil, err
			}
			if strings.TrimSpace(req.Image) == "" {
				return nil, invalidRequest("image reference is required")
			}
			return nil, s.Backend.UnpinImage(ctx, req)
		})
	case agentprotocol.RouteLeaseDevices:
		s.handleMutating(writer, request, func(ctx context.Context, payload json.RawMessage) (any, error) {
			var req agentprotocol.LeaseDevicesRequest
			if err := decodeRequest(payload, &req); err != nil {
				return nil, err
			}
			if err := validateIdentity(req.Identity, true); err != nil {
				return nil, err
			}
			if strings.TrimSpace(req.SandboxID) == "" {
				return nil, invalidRequest("sandboxId is required")
			}
			if strings.TrimSpace(req.Image) == "" {
				return nil, invalidRequest("image reference is required")
			}
			return s.Backend.LeaseDevices(ctx, req)
		})
	case agentprotocol.RouteReleaseDevices:
		s.handleMutating(writer, request, func(ctx context.Context, payload json.RawMessage) (any, error) {
			var req agentprotocol.ReleaseDevicesRequest
			if err := decodeRequest(payload, &req); err != nil {
				return nil, err
			}
			if err := validateIdentity(req.Identity, true); err != nil {
				return nil, err
			}
			if strings.TrimSpace(req.LeaseID) == "" {
				return nil, invalidRequest("leaseId is required")
			}
			return nil, s.Backend.ReleaseDevices(ctx, req)
		})
	case agentprotocol.RouteListLeases:
		s.handleRead(writer, request, func(ctx context.Context, identity agentprotocol.Identity) (any, error) {
			leases, err := s.Backend.ListLeases(ctx, identity)
			if err != nil {
				return nil, err
			}
			return agentprotocol.ListLeasesResponse{Leases: leases}, nil
		})
	case agentprotocol.RouteCompatibility:
		s.handleRead(writer, request, func(ctx context.Context, identity agentprotocol.Identity) (any, error) {
			compatibility, err := s.Backend.Compatibility(ctx)
			if err != nil {
				return nil, err
			}
			return agentprotocol.CompatibilityResponse{CompatibilityClass: compatibility}, nil
		})
	case agentprotocol.RouteHealth:
		s.handleRead(writer, request, func(ctx context.Context, identity agentprotocol.Identity) (any, error) {
			return s.Backend.Health(ctx)
		})
	case agentprotocol.RoutePublishImage:
		s.handleMutating(writer, request, func(ctx context.Context, payload json.RawMessage) (any, error) {
			var req agentprotocol.PublishImageRequest
			if err := decodeRequest(payload, &req); err != nil {
				return nil, err
			}
			if err := validateIdentity(req.Identity, true); err != nil {
				return nil, err
			}
			if strings.TrimSpace(req.Key) == "" {
				return nil, invalidRequest("publish key is required")
			}
			if strings.TrimSpace(req.Dir) == "" {
				return nil, invalidRequest("staging directory is required")
			}
			return s.Backend.PublishImage(ctx, req)
		})
	default:
		http.NotFound(writer, request)
	}
}

// handleMutating runs a mutating RPC: POST only, bounded body, and a
// non-empty idempotency key (validated per route on the decoded identity).
func (s *Server) handleMutating(writer http.ResponseWriter, request *http.Request, run func(ctx context.Context, payload json.RawMessage) (any, error)) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}
	payload, err := readBody(request)
	if err != nil {
		writeError(writer, invalidRequest("%v", err))
		return
	}
	result, err := run(request.Context(), payload)
	if err != nil {
		writeError(writer, err)
		return
	}
	if result == nil {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

// handleRead runs a read-only RPC: POST only, bounded body, caller identity
// required, no idempotency key.
func (s *Server) handleRead(writer http.ResponseWriter, request *http.Request, run func(ctx context.Context, identity agentprotocol.Identity) (any, error)) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}
	payload, err := readBody(request)
	if err != nil {
		writeError(writer, invalidRequest("%v", err))
		return
	}
	var identity agentprotocol.Identity
	if err := decodeIdentity(payload, &identity); err != nil {
		writeError(writer, err)
		return
	}
	if err := validateIdentity(identity); err != nil {
		writeError(writer, err)
		return
	}
	result, err := run(request.Context(), identity)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

// decodeRequest decodes a full RPC request, rejecting unknown fields.
func decodeRequest(payload json.RawMessage, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return invalidRequest("decode request: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return invalidRequest("multiple JSON values are not allowed")
		}
		return invalidRequest("decode request trailing data: %v", err)
	}
	return nil
}

// decodeIdentity parses the caller identity out of a request body.
func decodeIdentity(payload json.RawMessage, identity *agentprotocol.Identity) error {
	var envelope struct {
		RequestID string `json:"requestId"`
		Namespace string `json:"namespace,omitempty"`
		PodUID    string `json:"podUid"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return invalidRequest("decode identity: %v", err)
	}
	identity.RequestID = envelope.RequestID
	identity.Namespace = envelope.Namespace
	identity.PodUID = envelope.PodUID
	return nil
}

// validateIdentity enforces the caller identity contract: an empty PodUID
// is rejected (403). Mutating RPCs additionally require the idempotency
// key; the callers pass requireRequestID to demand it.
func validateIdentity(identity agentprotocol.Identity, requireRequestID ...bool) error {
	if strings.TrimSpace(identity.PodUID) == "" {
		return unauthorized("caller podUid is required")
	}
	if len(requireRequestID) > 0 && requireRequestID[0] && strings.TrimSpace(identity.RequestID) == "" {
		return invalidRequest("requestId is required for mutating RPCs")
	}
	return nil
}

// readBody reads and bounds the request body.
func readBody(request *http.Request) (json.RawMessage, error) {
	defer request.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxRequestBytes {
		return nil, fmt.Errorf("request body exceeds the %d-byte limit", maxRequestBytes)
	}
	return payload, nil
}

// writeError maps a backend error onto a stable wire code and status.
func writeError(writer http.ResponseWriter, err error) {
	classified := &Error{Code: agentprotocol.ErrorInternal, Message: err.Error(), Cause: err}
	var typed *Error
	if errors.As(err, &typed) {
		classified = typed
	} else if errors.Is(err, agentstate.ErrConflict) {
		classified = &Error{Code: agentprotocol.ErrorConflict, Message: err.Error(), Cause: err}
	} else if errors.Is(err, runtimecontract.ErrImageNotReady) {
		classified = &Error{Code: agentprotocol.ErrorNotFound, Message: err.Error(), Cause: err}
	}
	status := http.StatusInternalServerError
	switch classified.Code {
	case agentprotocol.ErrorInvalidRequest:
		status = http.StatusBadRequest
	case agentprotocol.ErrorUnauthorized:
		status = http.StatusForbidden
	case agentprotocol.ErrorConflict:
		status = http.StatusConflict
	case agentprotocol.ErrorNotFound:
		status = http.StatusNotFound
	case agentprotocol.ErrorForbidden:
		status = http.StatusForbidden
	}
	writeJSON(writer, status, agentprotocol.ErrorResponse{Code: classified.Code, Message: classified.Message})
}

func methodNotAllowed(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusMethodNotAllowed, agentprotocol.ErrorResponse{Code: agentprotocol.ErrorInvalidRequest, Message: "method not allowed"})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func parentDir(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index <= 0 {
		return "."
	}
	return path[:index]
}
