package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

// FastletAPIClient defines the interface for communicating with sandbox fastlets.
// This allows both the real HTTP client and mocks to be used interchangeably.
type FastletAPIClient interface {
	CreateSandbox(fastletIP string, req *CreateSandboxRequest) (*CreateSandboxResponse, error)
	DeleteSandbox(fastletIP string, req *DeleteSandboxRequest) (*DeleteSandboxResponse, error)
	GetFastletStatus(ctx context.Context, fastletIP string) (*FastletStatus, error)
}

const (
	// defaultFastletTimeout is the default timeout for fastlet API calls
	defaultFastletTimeout = 5 * time.Second
)

// FastletClient handles HTTP communication with fastlets.
type FastletClient struct {
	httpClient  *http.Client
	timeout     time.Duration
	fastletPort int
}

// NewFastletClient creates a new fastlet client.
func NewFastletClient(fastletPort int) *FastletClient {
	return &FastletClient{
		httpClient: &http.Client{
			Timeout: defaultFastletTimeout,
		},
		timeout:     defaultFastletTimeout,
		fastletPort: fastletPort,
	}
}

// SetTimeout sets the timeout for fastlet API calls.
func (c *FastletClient) SetTimeout(timeout time.Duration) {
	c.timeout = timeout
	c.httpClient.Timeout = timeout
}

// CreateSandbox sends a create sandbox request to the fastlet.
func (c *FastletClient) CreateSandbox(fastletIP string, req *CreateSandboxRequest) (*CreateSandboxResponse, error) {
	start := time.Now()
	defer func() {
		duration := time.Since(start)
		klog.InfoS("Fastlet CreateSandbox RPC",
			"endpoint", fastletIP,
			"sandboxID", req.Sandbox.SandboxID,
			"duration_ms", duration.Milliseconds())
	}()

	if req.Sandbox.SandboxID == "" {
		return nil, errors.New("sandboxID is required")
	}

	url := fmt.Sprintf("http://%s:%d/api/v1/fastlet/create", fastletIP, c.fastletPort)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var createResp CreateSandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return &createResp, fmt.Errorf("create failed with status: %d, message: %s", resp.StatusCode, createResp.Message)
	}

	return &createResp, nil
}

// DeleteSandbox sends a delete sandbox request to the fastlet.
func (c *FastletClient) DeleteSandbox(fastletIP string, req *DeleteSandboxRequest) (*DeleteSandboxResponse, error) {
	start := time.Now()
	defer func() {
		duration := time.Since(start)
		klog.InfoS("Fastlet DeleteSandbox RPC",
			"endpoint", fastletIP,
			"sandboxID", req.SandboxID,
			"duration_ms", duration.Milliseconds())
	}()

	url := fmt.Sprintf("http://%s:%d/api/v1/fastlet/delete", fastletIP, c.fastletPort)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var deleteResp DeleteSandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&deleteResp); err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return &deleteResp, fmt.Errorf("delete failed with status: %d, message: %s", resp.StatusCode, deleteResp.Message)
	}

	return &deleteResp, nil
}

// GetFastletStatus fetches the current status of a fastlet with context support.
func (c *FastletClient) GetFastletStatus(ctx context.Context, fastletIP string) (*FastletStatus, error) {
	// Apply timeout if not already set in context
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	url := fmt.Sprintf("http://%s:%d/api/v1/fastlet/status", fastletIP, c.fastletPort)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get status failed with status: %d", resp.StatusCode)
	}

	var status FastletStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}

	return &status, nil
}
