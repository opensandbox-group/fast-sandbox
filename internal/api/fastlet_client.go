package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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

// FastletAdmissionClient is the versioned control-plane protocol used by the
// multi-active Fast-Path and declarative Controller. It deliberately contains
// lifecycle/admission operations only; Exec/File are data-plane concerns.
type FastletAdmissionClient interface {
	ReserveSandbox(ctx context.Context, fastletIP string, req *ReserveSandboxRequest) (*ReserveSandboxResponse, error)
	CancelReservation(ctx context.Context, fastletIP string, req *CancelReservationRequest) (*CancelReservationResponse, error)
	EnsureSandbox(ctx context.Context, fastletIP string, req *EnsureSandboxRequest) (*EnsureSandboxResponse, error)
	InspectSandbox(ctx context.Context, fastletIP string, req *InspectSandboxRequest) (*InspectSandboxResponse, error)
	DeleteSandboxV2(ctx context.Context, fastletIP string, req *DeleteSandboxV2Request) (*DeleteSandboxV2Response, error)
	Heartbeat(ctx context.Context, fastletIP string, req *HeartbeatRequest) (*HeartbeatResponse, error)
	RuntimeDiagnostics(ctx context.Context, fastletIP string) (*RuntimeDiagnostics, error)
	SetDraining(ctx context.Context, fastletIP string, req *SetDrainingRequest) (*SetDrainingResponse, error)
}

var _ FastletAdmissionClient = (*FastletClient)(nil)

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

func (c *FastletClient) ReserveSandbox(ctx context.Context, fastletIP string, req *ReserveSandboxRequest) (*ReserveSandboxResponse, error) {
	return postFastletJSON[ReserveSandboxRequest, ReserveSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/reservations", req)
}

func (c *FastletClient) CancelReservation(ctx context.Context, fastletIP string, req *CancelReservationRequest) (*CancelReservationResponse, error) {
	return postFastletJSON[CancelReservationRequest, CancelReservationResponse](c, ctx, fastletIP, "/api/v2/fastlet/reservations/cancel", req)
}

func (c *FastletClient) EnsureSandbox(ctx context.Context, fastletIP string, req *EnsureSandboxRequest) (*EnsureSandboxResponse, error) {
	return postFastletJSON[EnsureSandboxRequest, EnsureSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/ensure", req)
}

func (c *FastletClient) InspectSandbox(ctx context.Context, fastletIP string, req *InspectSandboxRequest) (*InspectSandboxResponse, error) {
	return postFastletJSON[InspectSandboxRequest, InspectSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/inspect", req)
}

func (c *FastletClient) DeleteSandboxV2(ctx context.Context, fastletIP string, req *DeleteSandboxV2Request) (*DeleteSandboxV2Response, error) {
	return postFastletJSON[DeleteSandboxV2Request, DeleteSandboxV2Response](c, ctx, fastletIP, "/api/v2/fastlet/delete", req)
}

func (c *FastletClient) Heartbeat(ctx context.Context, fastletIP string, req *HeartbeatRequest) (*HeartbeatResponse, error) {
	path := "/api/v2/fastlet/heartbeat"
	if req != nil {
		query := make(url.Values)
		query.Set("cacheEpoch", req.Cache.Epoch)
		query.Set("cacheRevision", strconv.FormatUint(req.Cache.Revision, 10))
		query.Set("fullCache", strconv.FormatBool(req.Cache.ForceFull))
		path += "?" + query.Encode()
	}
	return getFastletJSON[HeartbeatResponse](c, ctx, fastletIP, path)
}

func (c *FastletClient) RuntimeDiagnostics(ctx context.Context, fastletIP string) (*RuntimeDiagnostics, error) {
	return getFastletJSON[RuntimeDiagnostics](c, ctx, fastletIP, "/api/v2/fastlet/runtime-diagnostics")
}

func (c *FastletClient) SetDraining(ctx context.Context, fastletIP string, req *SetDrainingRequest) (*SetDrainingResponse, error) {
	return postFastletJSON[SetDrainingRequest, SetDrainingResponse](c, ctx, fastletIP, "/api/v2/fastlet/draining", req)
}

func postFastletJSON[Request any, Response any](c *FastletClient, ctx context.Context, fastletIP, path string, request *Request) (*Response, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	requestContext, cancel := c.requestContext(ctx)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.endpoint(fastletIP, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	return doFastletJSON[Response](c, httpRequest)
}

func getFastletJSON[Response any](c *FastletClient, ctx context.Context, fastletIP, path string) (*Response, error) {
	requestContext, cancel := c.requestContext(ctx)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodGet, c.endpoint(fastletIP, path), nil)
	if err != nil {
		return nil, err
	}
	return doFastletJSON[Response](c, httpRequest)
}

func doFastletJSON[Response any](c *FastletClient, request *http.Request) (*Response, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result Response
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	if failure := responseFastletError(any(&result)); failure != nil {
		return &result, failure
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &result, fmt.Errorf("Fastlet request failed with status: %d", response.StatusCode)
	}
	return &result, nil
}

func responseFastletError(response any) *FastletError {
	switch typed := response.(type) {
	case *ReserveSandboxResponse:
		return typed.Error
	case *CancelReservationResponse:
		return typed.Error
	case *EnsureSandboxResponse:
		return typed.Error
	case *InspectSandboxResponse:
		return typed.Error
	case *DeleteSandboxV2Response:
		return typed.Error
	default:
		return nil
	}
}

func (c *FastletClient) endpoint(fastletIP, path string) string {
	return fmt.Sprintf("http://%s:%d%s", fastletIP, c.fastletPort, path)
}

func (c *FastletClient) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline || c.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}
