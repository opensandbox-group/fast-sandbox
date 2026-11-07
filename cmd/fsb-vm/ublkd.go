package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"fast-sandbox/internal/ublkd"
)

// maxUblkdResponseBytes bounds the control-API responses read from ublkd.
const maxUblkdResponseBytes = 1 << 20

const ublkdConfigKey = "config"

// errAddOutcomeUnknown means the Add request may still be running in ublkd.
// The daemon does not expose in-flight Adds through List, so callers must keep
// the config and its backing files until a later cleanup can confirm the result.
var errAddOutcomeUnknown = errors.New("ublkd add outcome unknown")

type ublkdRequestError struct {
	err error
}

func (err *ublkdRequestError) Error() string { return err.err.Error() }
func (err *ublkdRequestError) Unwrap() error { return err.err }

// definitelyNotSent is true only when connecting to the unix socket failed.
// Every transport failure after a connection was established is ambiguous: the
// daemon may have accepted the request even though no response reached us.
func (err *ublkdRequestError) definitelyNotSent() bool {
	var opErr *net.OpError
	return errors.As(err.err, &opErr) && opErr.Op == "dial"
}

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
type ublkdDevice = ublkd.Device

type ublkdAddResponse struct {
	OK    bool   `json:"ok"`
	DevID int    `json:"dev_id"`
	Dev   string `json:"dev"`
	Error string `json:"error"`
}

// ublkdAcquireResponse is the /v1/acquire reply: an add reply plus the lease
// mode and reference count.
type ublkdAcquireResponse struct {
	OK       bool   `json:"ok"`
	DevID    int    `json:"dev_id"`
	Dev      string `json:"dev"`
	Mode     string `json:"mode"`
	RefCount int    `json:"refcount"`
	Error    string `json:"error"`
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
	request := map[string]string{ublkdConfigKey: configPath}
	var response ublkdAddResponse
	if err := c.do(ctx, http.MethodPost, "/v1/add", request, &response); err != nil {
		var requestErr *ublkdRequestError
		if errors.As(err, &requestErr) && !requestErr.definitelyNotSent() {
			return 0, "", fmt.Errorf("%w: %w", errAddOutcomeUnknown, err)
		}
		return 0, "", err
	}
	if !response.OK {
		return 0, "", fmt.Errorf("ublkd add %s: %s", configPath, response.Error)
	}
	return response.DevID, response.Dev, nil
}

// Acquire takes an exclusive lease; the warm pool serves it from a pre-created
// device, so callers must return it with Release rather than Del.
func (c *ublkdClient) Acquire(ctx context.Context, configPath string) (int, string, error) {
	request := map[string]string{ublkdConfigKey: configPath, "mode": "exclusive"}
	var response ublkdAcquireResponse
	if err := c.do(ctx, http.MethodPost, "/v1/acquire", request, &response); err != nil {
		var requestErr *ublkdRequestError
		if errors.As(err, &requestErr) && !requestErr.definitelyNotSent() {
			return 0, "", fmt.Errorf("%w: %w", errAddOutcomeUnknown, err)
		}
		return 0, "", err
	}
	if !response.OK {
		return 0, "", fmt.Errorf("ublkd acquire %s: %s", configPath, response.Error)
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

// Release drops one lease. A pooled device is parked back in the idle pool;
// any other exclusive device is torn down once its last lease returns, which
// makes Release safe for devices created by Add too.
func (c *ublkdClient) Release(ctx context.Context, devID int) error {
	var response ublkdOKResponse
	if err := c.do(ctx, http.MethodPost, "/v1/release", map[string]int{"dev_id": devID}, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("ublkd release %d: %s", devID, response.Error)
	}
	return nil
}

// List enumerates the devices currently served by the daemon.
func (c *ublkdClient) List(ctx context.Context) ([]ublkdDevice, error) {
	return ublkd.List(ctx, c.httpClient)
}

func (c *ublkdClient) do(ctx context.Context, method, path string, input, output any) error {
	_, err := c.doStatus(ctx, method, path, input, output)
	return err
}

func (c *ublkdClient) doStatus(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://ublkd"+path, body)
	if err != nil {
		return 0, err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, &ublkdRequestError{err: err}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxUblkdResponseBytes)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return response.StatusCode, fmt.Errorf("read ublkd response: %w", err)
	}
	// The daemon reports failures with a non-2xx status and a JSON body
	// carrying "error"; decode regardless so callers can surface it.
	if output != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, output); err != nil {
			return response.StatusCode, fmt.Errorf("decode ublkd response (%s): %w", bytes.TrimSpace(payload), err)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if output == nil {
			return response.StatusCode, fmt.Errorf("ublkd %s %s: %s: %s", method, path, response.Status, bytes.TrimSpace(payload))
		}
	}
	return response.StatusCode, nil
}

// FenceAdd disables future Adds of a retained pack config and waits for the
// daemon's per-config guard to become free. The pinned ublkd holds that guard
// through start(), including failures. Its 500 start-failure response to this
// invalid config therefore acknowledges that earlier Adds have finished.
// A timeout, disconnect, or unfamiliar response is never treated as success.
func (c *ublkdClient) FenceAdd(ctx context.Context, config string) error {
	if err := writeFileAtomic(config, []byte("fsb-vm: retired pack config\n"), 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var response ublkdAddResponse
		code, err := c.doStatus(ctx, http.MethodPost, "/v1/add", map[string]string{ublkdConfigKey: config}, &response)
		if err != nil {
			return fmt.Errorf("fence retained pack add: %w", err)
		}
		if code == http.StatusInternalServerError && !response.OK && response.Error == "device failed to start (see daemon log)" {
			return nil
		}
		if code != http.StatusConflict || response.Error != "an add of this image is already in progress" {
			return fmt.Errorf("ublkd did not acknowledge retired pack config: status %d: %s", code, response.Error)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
