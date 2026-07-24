package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
	boxliteserver "fast-sandbox/internal/runtime/boxlite/server"
)

const probeResponseLimit = 1 << 20

func main() {
	socketPath := flag.String("socket", "/run/fast-sandbox/boxlite/runtime.sock", "Pod-local BoxLite runtime control socket")
	stateRoot := flag.String("state-root", "/var/lib/fast-sandbox/boxlite", "BoxLite state root")
	probeSocket := flag.String("probe-socket", "", "Probe a running BoxLite runtime control socket and exit")
	flag.Parse()
	if *probeSocket != "" {
		probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := probeCapabilities(probeCtx, *probeSocket); err != nil {
			fmt.Fprintf(os.Stderr, "boxlite-runtime: probe: %v\n", err)
			os.Exit(1)
		}
		return
	}
	backend, err := newBackend(*stateRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxlite-runtime: initialize backend: %v\n", err)
		os.Exit(1)
	}
	if closer, ok := backend.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	listener, err := listenUnix(*socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxlite-runtime: listen: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           &boxliteserver.Server{Backend: backend},
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "boxlite-runtime: serve: %v\n", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}

func probeCapabilities(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		return errors.New("control socket path is required")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		ForceAttemptHTTP2: false,
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://boxlite/v1/capabilities", nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("capability endpoint returned %s: %s", response.Status, string(body))
	}
	var capabilities boxliteprotocol.Capabilities
	decoder := json.NewDecoder(io.LimitReader(response.Body, probeResponseLimit))
	if err := decoder.Decode(&capabilities); err != nil {
		return fmt.Errorf("decode capabilities: %w", err)
	}
	if capabilities.ProtocolVersion != boxliteprotocol.ProtocolVersionV1 {
		return fmt.Errorf("protocol version %q is not supported", capabilities.ProtocolVersion)
	}
	for _, capability := range boxliteprotocol.RequiredCapabilities {
		if !capabilities.Capabilities[capability] {
			return fmt.Errorf("required capability %q is unavailable", capability)
		}
	}
	if !capabilities.Ready {
		reason := capabilities.Reason
		if reason == "" {
			reason = "BoxLiteRuntimeNotReady"
		}
		return fmt.Errorf("%s: %s", reason, capabilities.Message)
	}
	return nil
}

func listenUnix(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("control socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
