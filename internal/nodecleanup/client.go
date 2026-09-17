package nodecleanup

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

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

type Client struct {
	socketPath string
	httpClient *http.Client
}

func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{Transport: transport, Timeout: 5 * time.Second},
	}
}

func (c *Client) EnsureRuntimeProcessesAbsent(
	ctx context.Context,
	kind runtimecatalog.ResidualProcessKind,
	sandboxID string,
) error {
	if c == nil || c.httpClient == nil {
		return errors.New("node cleanup client is not configured")
	}
	payload, err := json.Marshal(EnsureAbsentRequest{Kind: kind, SandboxID: sandboxID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node-janitor"+EnsureAbsentPath, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call NodeJanitor at %s: %w", c.socketPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	message, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		return fmt.Errorf("nodeJanitor returned %s and its response could not be read: %w", resp.Status, readErr)
	}
	return fmt.Errorf("nodeJanitor returned %s: %s", resp.Status, bytes.TrimSpace(message))
}
