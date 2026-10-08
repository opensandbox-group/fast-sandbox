package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxUblkdResponseBytes bounds the control-API responses read from ublkd.
const maxUblkdResponseBytes = 1 << 20

// ublkdClient talks to overlaybd-ublkd's HTTP-over-unix-socket control API
// (ADR-002): POST /v1/add, POST /v1/del, GET /v1/list, GET /v1/ping.
type ublkdClient struct {
	httpClient *http.Client
	socket     string
}

// newUblkdClient dials the ublkd control socket. add blocks until the device
// is ready, so the timeout is generous.
func newUblkdClient(socket string) *ublkdClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &ublkdClient{
		httpClient: &http.Client{Transport: transport, Timeout: 5 * time.Minute},
		socket:     socket,
	}
}

// ublkdDevice is one device entry returned by /v1/list (and the shape shared
// by /v1/add success replies).
type ublkdDevice struct {
	DevID    int    `json:"dev_id"`
	Dev      string `json:"dev"`
	Config   string `json:"config"`
	Writable bool   `json:"writable"`
	State    string `json:"state"`
	Mode     string `json:"mode"`
	RefCount int    `json:"refcount"`
}

type ublkdAddResponse struct {
	OK    bool   `json:"ok"`
	DevID int    `json:"dev_id"`
	Dev   string `json:"dev"`
	Error string `json:"error"`
}

type ublkdListResponse struct {
	OK      bool          `json:"ok"`
	Devices []ublkdDevice `json:"devices"`
	Error   string        `json:"error"`
}

type ublkdOKResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Ping verifies the daemon is reachable (GET /v1/ping); it is the startup
// reachability check of ADR-002 (the CLI never launches the daemon itself).
func (c *ublkdClient) Ping(ctx context.Context) error {
	var response ublkdOKResponse
	if err := c.do(ctx, http.MethodGet, "/v1/ping", nil, &response); err != nil {
		return fmt.Errorf("overlaybd-ublkd not reachable on %s: %w", c.socket, err)
	}
	return nil
}

// Add attaches the device described by configPath and returns its dev_id and
// /dev/ublkbN path. The call blocks until the device is usable.
func (c *ublkdClient) Add(ctx context.Context, configPath string) (int, string, error) {
	request := map[string]string{"config": configPath}
	var response ublkdAddResponse
	if err := c.do(ctx, http.MethodPost, "/v1/add", request, &response); err != nil {
		return 0, "", err
	}
	if !response.OK {
		return 0, "", fmt.Errorf("ublkd add %s: %s", configPath, response.Error)
	}
	return response.DevID, response.Dev, nil
}

// Del detaches a device by dev_id. It is idempotent from the caller's
// perspective: an already-gone device is reported by the daemon.
func (c *ublkdClient) Del(ctx context.Context, devID int) error {
	request := map[string]int{"dev_id": devID}
	var response ublkdOKResponse
	if err := c.do(ctx, http.MethodPost, "/v1/del", request, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("ublkd del %d: %s", devID, response.Error)
	}
	return nil
}

// List enumerates the devices currently served by the daemon.
func (c *ublkdClient) List(ctx context.Context) ([]ublkdDevice, error) {
	var response ublkdListResponse
	if err := c.do(ctx, http.MethodGet, "/v1/list", nil, &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, fmt.Errorf("ublkd list: %s", response.Error)
	}
	return response.Devices, nil
}

func (c *ublkdClient) do(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://ublkd"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxUblkdResponseBytes)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read ublkd response: %w", err)
	}
	// The daemon reports failures with a non-2xx status and a JSON body
	// carrying "error"; decode regardless so callers can surface it.
	if output != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, output); err != nil {
			return fmt.Errorf("decode ublkd response (%s): %w", bytes.TrimSpace(payload), err)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if output == nil {
			return fmt.Errorf("ublkd %s %s: %s: %s", method, path, response.Status, bytes.TrimSpace(payload))
		}
	}
	return nil
}
