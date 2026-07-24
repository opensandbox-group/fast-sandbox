package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
)

const maxRequestBytes = 4 << 20

type Backend interface {
	Capabilities(context.Context) boxliteprotocol.Capabilities
	Ensure(context.Context, boxliteprotocol.EnsureRequest) (boxliteprotocol.Box, error)
	Recover(context.Context, string) (boxliteprotocol.Box, error)
	Inspect(context.Context, string) (boxliteprotocol.Box, error)
	Delete(context.Context, string) error
	List(context.Context, string) ([]boxliteprotocol.Box, error)
	ListImages(context.Context) ([]string, error)
	PullImage(context.Context, string) error
}

type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Server struct {
	Backend Backend
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s.Backend == nil {
		writeError(writer, &Error{Code: boxliteprotocol.ErrorUnavailable, Message: "BoxLite backend is not configured"})
		return
	}
	switch {
	case request.URL.Path == "/v1/capabilities":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer)
			return
		}
		writeJSON(writer, http.StatusOK, s.Backend.Capabilities(request.Context()))
	case request.URL.Path == "/v1/boxes":
		s.handleBoxes(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/boxes/"):
		s.handleBox(writer, request)
	case request.URL.Path == "/v1/images":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer)
			return
		}
		images, err := s.Backend.ListImages(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, boxliteprotocol.ImagesResponse{Images: images})
	case request.URL.Path == "/v1/images/pull":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer)
			return
		}
		var pull boxliteprotocol.PullRequest
		if err := decodeJSON(request, &pull); err != nil {
			writeError(writer, &Error{Code: boxliteprotocol.ErrorInvalid, Message: err.Error(), Cause: err})
			return
		}
		if strings.TrimSpace(pull.Image) == "" {
			writeError(writer, &Error{Code: boxliteprotocol.ErrorInvalid, Message: "image reference is required"})
			return
		}
		if err := s.Backend.PullImage(request.Context(), pull.Image); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(writer, request)
	}
}

func (s *Server) handleBoxes(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}
	boxes, err := s.Backend.List(request.Context(), request.URL.Query().Get("namespace"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, boxliteprotocol.ListResponse{Boxes: boxes})
}

func (s *Server) handleBox(writer http.ResponseWriter, request *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(request.URL.EscapedPath(), "/v1/boxes/"))
	if err != nil || id == "" || strings.Contains(id, "/") {
		writeError(writer, &Error{Code: boxliteprotocol.ErrorInvalid, Message: "valid Box Sandbox UID is required"})
		return
	}
	switch request.Method {
	case http.MethodPut:
		var ensure boxliteprotocol.EnsureRequest
		if err := decodeJSON(request, &ensure); err != nil {
			writeError(writer, &Error{Code: boxliteprotocol.ErrorInvalid, Message: err.Error(), Cause: err})
			return
		}
		if ensure.Sandbox.SandboxID != id {
			writeError(writer, &Error{Code: boxliteprotocol.ErrorInvalid, Message: "path and Sandbox UID do not match"})
			return
		}
		box, err := s.Backend.Ensure(request.Context(), ensure)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, box)
	case http.MethodPost:
		box, err := s.Backend.Recover(request.Context(), id)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, box)
	case http.MethodGet:
		box, err := s.Backend.Inspect(request.Context(), id)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, box)
	case http.MethodDelete:
		if err := s.Backend.Delete(request.Context(), id); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(writer)
	}
}

func decodeJSON(request *http.Request, destination any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode request: multiple JSON values are not allowed")
		}
		return fmt.Errorf("decode request trailing data: %w", err)
	}
	return nil
}

func methodNotAllowed(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusMethodNotAllowed, boxliteprotocol.ErrorResponse{Code: boxliteprotocol.ErrorInvalid, Message: "method not allowed"})
}

func writeError(writer http.ResponseWriter, err error) {
	backendError := &Error{Code: boxliteprotocol.ErrorInternal, Message: err.Error(), Cause: err}
	var typed *Error
	if errors.As(err, &typed) {
		backendError = typed
	}
	status := http.StatusInternalServerError
	switch backendError.Code {
	case boxliteprotocol.ErrorInvalid:
		status = http.StatusBadRequest
	case boxliteprotocol.ErrorNotFound:
		status = http.StatusNotFound
	case boxliteprotocol.ErrorConflict, boxliteprotocol.ErrorImmutableSpecConflict:
		status = http.StatusConflict
	case boxliteprotocol.ErrorUnavailable:
		status = http.StatusServiceUnavailable
	}
	writeJSON(writer, status, boxliteprotocol.ErrorResponse{Code: backendError.Code, Message: backendError.Message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
