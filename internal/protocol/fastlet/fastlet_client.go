package fastlet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"fast-sandbox/internal/observability"
)

// FastletAdmissionClient is the versioned control-plane protocol used by the
// multi-active Fast-Path and declarative Controller. It deliberately contains
// lifecycle/admission operations only; Exec/File are data-plane concerns.
type FastletAdmissionClient interface {
	CreateSandbox(ctx context.Context, fastletIP string, req *CreateSandboxRequest) (*CreateSandboxResponse, error)
	InspectSandbox(ctx context.Context, fastletIP string, req *InspectSandboxRequest) (*InspectSandboxResponse, error)
	DeleteSandboxV2(ctx context.Context, fastletIP string, req *DeleteSandboxV2Request) (*DeleteSandboxV2Response, error)
	Heartbeat(ctx context.Context, fastletIP string, req *HeartbeatRequest) (*HeartbeatResponse, error)
	RuntimeDiagnostics(ctx context.Context, fastletIP string) (*RuntimeDiagnostics, error)
	SandboxDiagnostics(ctx context.Context, fastletIP string, req *SandboxDiagnosticsRequest) (*SandboxDiagnosticsResponse, error)
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

func (c *FastletClient) CreateSandbox(ctx context.Context, fastletIP string, req *CreateSandboxRequest) (*CreateSandboxResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
		ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: req.Sandbox.ClaimNamespace, SandboxName: req.Sandbox.ClaimName})
	}
	return postFastletJSON[CreateSandboxRequest, CreateSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/create", req)
}

func (c *FastletClient) InspectSandbox(ctx context.Context, fastletIP string, req *InspectSandboxRequest) (*InspectSandboxResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
	return postFastletJSON[InspectSandboxRequest, InspectSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/inspect", req)
}

func (c *FastletClient) DeleteSandboxV2(ctx context.Context, fastletIP string, req *DeleteSandboxV2Request) (*DeleteSandboxV2Response, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
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

func (c *FastletClient) SandboxDiagnostics(ctx context.Context, fastletIP string, req *SandboxDiagnosticsRequest) (*SandboxDiagnosticsResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
	return postFastletJSON[SandboxDiagnosticsRequest, SandboxDiagnosticsResponse](c, ctx, fastletIP, "/api/v2/fastlet/diagnostics/sandbox", req)
}

func (c *FastletClient) SetDraining(ctx context.Context, fastletIP string, req *SetDrainingRequest) (*SetDrainingResponse, error) {
	return postFastletJSON[SetDrainingRequest, SetDrainingResponse](c, ctx, fastletIP, "/api/v2/fastlet/draining", req)
}

func postFastletJSON[Request any, Response any](c *FastletClient, ctx context.Context, fastletIP, path string, request *Request) (_ *Response, resultErr error) {
	ctx, span := observability.StartClient(ctx, "fastlet.client "+path)
	defer func() { observability.End(span, resultErr) }()
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
	observability.InjectHTTP(requestContext, httpRequest.Header)
	return doFastletJSON[Response](c, httpRequest)
}

func getFastletJSON[Response any](c *FastletClient, ctx context.Context, fastletIP, path string) (_ *Response, resultErr error) {
	ctx, span := observability.StartClient(ctx, "fastlet.client "+path)
	defer func() { observability.End(span, resultErr) }()
	requestContext, cancel := c.requestContext(ctx)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodGet, c.endpoint(fastletIP, path), nil)
	if err != nil {
		return nil, err
	}
	observability.InjectHTTP(requestContext, httpRequest.Header)
	return doFastletJSON[Response](c, httpRequest)
}

func withFastletIdentity(ctx context.Context, identity SandboxIdentity) context.Context {
	return observability.WithIdentity(ctx, observability.Identity{
		RequestID: identity.RequestID, SandboxUID: identity.SandboxUID, FastletPodUID: identity.FastletPodUID,
		InstanceGeneration: identity.InstanceGeneration, AssignmentAttempt: identity.AssignmentAttempt, RouteGeneration: identity.RouteGeneration,
	})
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
	case *CreateSandboxResponse:
		return typed.Error
	case *InspectSandboxResponse:
		return typed.Error
	case *DeleteSandboxV2Response:
		return typed.Error
	case *SandboxDiagnosticsResponse:
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
