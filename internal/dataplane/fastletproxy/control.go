package fastletproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultControlSocket = "/run/fast-sandbox/proxy/control.sock"

type ControlServer struct {
	Store      *Store
	SocketPath string
}

func (s *ControlServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/routes/{sandboxUID}", s.applyRoute)
	mux.HandleFunc("DELETE /v1/routes/{sandboxUID}", s.deleteRoute)
	mux.HandleFunc("POST /v1/routes/{sandboxUID}/draining", s.markDraining)
	mux.HandleFunc("GET /v1/routes", s.snapshotRoutes)
	mux.HandleFunc("GET /v1/routes/watch", s.watchRoutes)
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	return mux
}

func (s *ControlServer) Serve(ctx context.Context) error {
	if s.Store == nil {
		return errors.New("Fastlet Proxy RouteStore is required")
	}
	socketPath := s.SocketPath
	if socketPath == "" {
		socketPath = DefaultControlSocket
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("create proxy control directory: %w", err)
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on proxy control socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	if err := os.Chmod(socketPath, 0o660); err != nil {
		return fmt.Errorf("set proxy control socket permissions: %w", err)
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket proxy control path %s", path)
	}
	return os.Remove(path)
}

func (s *ControlServer) applyRoute(writer http.ResponseWriter, request *http.Request) {
	var route Route
	if err := decodeJSON(request, &route); err != nil {
		writeControlError(writer, err)
		return
	}
	if pathUID := request.PathValue("sandboxUID"); route.SandboxUID == "" {
		route.SandboxUID = pathUID
	} else if route.SandboxUID != pathUID {
		writeControlError(writer, ErrRouteConflict)
		return
	}
	revision, err := s.Store.Apply(route)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]uint64{"revision": revision})
}

func (s *ControlServer) deleteRoute(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	revision, err := s.Store.Delete(request.PathValue("sandboxUID"), generation)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]uint64{"revision": revision})
}

func (s *ControlServer) markDraining(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	revision, err := s.Store.MarkDraining(request.PathValue("sandboxUID"), generation)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]uint64{"revision": revision})
}

func (s *ControlServer) snapshotRoutes(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, s.Store.Snapshot())
}

func (s *ControlServer) watchRoutes(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/x-ndjson")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	events, cancel := s.Store.Subscribe()
	defer cancel()
	encoder := json.NewEncoder(writer)
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			if err := encoder.Encode(event); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseGeneration(request *http.Request) (int64, error) {
	value, err := strconv.ParseInt(request.URL.Query().Get("routeGeneration"), 10, 64)
	if err != nil || value <= 0 {
		return 0, ErrRouteConflict
	}
	return value, nil
}

func decodeJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeControlError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrRouteNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrRouteStale), errors.Is(err, ErrRouteConflict):
		status = http.StatusConflict
	}
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

type ControlClient struct {
	httpClient *http.Client
}

type ControlError struct {
	StatusCode int
	Message    string
}

func (e *ControlError) Error() string {
	return fmt.Sprintf("Fastlet Proxy control returned HTTP %d: %s", e.StatusCode, e.Message)
}

func NewControlClient(socketPath string) *ControlClient {
	if socketPath == "" {
		socketPath = DefaultControlSocket
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &ControlClient{httpClient: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
}

func (c *ControlClient) Apply(ctx context.Context, route Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/v1/routes/"+url.PathEscape(route.SandboxUID), body, nil)
}

func (c *ControlClient) Delete(ctx context.Context, sandboxUID string, generation int64) error {
	path := "/v1/routes/" + url.PathEscape(sandboxUID) + "?routeGeneration=" + strconv.FormatInt(generation, 10)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *ControlClient) MarkDraining(ctx context.Context, sandboxUID string, generation int64) error {
	path := "/v1/routes/" + url.PathEscape(sandboxUID) + "/draining?routeGeneration=" + strconv.FormatInt(generation, 10)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

func (c *ControlClient) Snapshot(ctx context.Context) (Snapshot, error) {
	var snapshot Snapshot
	err := c.do(ctx, http.MethodGet, "/v1/routes", nil, &snapshot)
	return snapshot, err
}

func (c *ControlClient) Watch(ctx context.Context, consume func(Event) error) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://fastlet-proxy/v1/routes/watch", nil)
	if err != nil {
		return err
	}
	watchClient := *c.httpClient
	watchClient.Timeout = 0
	response, err := watchClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Fastlet Proxy control returned %s", response.Status)
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if err := consume(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (c *ControlClient) do(ctx context.Context, method, path string, body []byte, result any) error {
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(string(body))
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://fastlet-proxy"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure map[string]string
		_ = json.NewDecoder(response.Body).Decode(&failure)
		return &ControlError{StatusCode: response.StatusCode, Message: failure["error"]}
	}
	if result != nil {
		return json.NewDecoder(response.Body).Decode(result)
	}
	return nil
}
