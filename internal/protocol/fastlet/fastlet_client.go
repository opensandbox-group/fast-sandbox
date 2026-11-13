package fastlet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fast-sandbox/internal/fastletauth"
	"fast-sandbox/internal/observability"
)

// FastletAdmissionClient is the versioned control-plane protocol used by the
// multi-active Fast-Path and declarative Controller. It deliberately contains
// lifecycle/admission operations only; Exec/File are data-plane concerns.
type FastletAdmissionClient interface {
	CreateSandbox(ctx context.Context, fastletIP string, req *CreateSandboxRequest) (*CreateSandboxResponse, error)
	InspectSandbox(ctx context.Context, fastletIP string, req *InspectSandboxRequest) (*InspectSandboxResponse, error)
	DeleteSandbox(ctx context.Context, fastletIP string, req *DeleteSandboxRequest) (*DeleteSandboxResponse, error)
	ReconcileBindings(ctx context.Context, fastletIP string, req *ReconcileBindingsRequest) (*ReconcileBindingsResponse, error)
	InvokeHandler(ctx context.Context, fastletIP string, req *InvokeHandlerRequest) (*InvokeHandlerResponse, error)
	CreateSnapshot(ctx context.Context, fastletIP string, req *CreateSnapshotRequest) (*CreateSnapshotResponse, error)
	InspectSnapshot(ctx context.Context, fastletIP string, req *InspectSnapshotRequest) (*InspectSnapshotResponse, error)
	DeleteSnapshot(ctx context.Context, fastletIP string, req *DeleteSnapshotRequest) (*DeleteSnapshotResponse, error)
	Heartbeat(ctx context.Context, fastletIP string, req *HeartbeatRequest) (*HeartbeatResponse, error)
	RuntimeDiagnostics(ctx context.Context, fastletIP string) (*RuntimeDiagnostics, error)
	SandboxDiagnostics(ctx context.Context, fastletIP string, req *SandboxDiagnosticsRequest) (*SandboxDiagnosticsResponse, error)
	SetDraining(ctx context.Context, fastletIP string, req *SetDrainingRequest) (*SetDrainingResponse, error)
}

var _ FastletAdmissionClient = (*FastletClient)(nil)

const (
	// defaultFastletTimeout keeps control and observation calls responsive.
	defaultFastletTimeout = 5 * time.Second
	// Runtime creation may legitimately take longer for secure runtimes such as
	// Kata. Keep its transport deadline aligned with the runtime driver's
	// operation window instead of cancelling it at the generic control timeout.
	defaultFastletCreateTimeout = 2 * time.Minute
)

// FastletClient handles HTTP communication with fastlets.
type FastletClient struct {
	httpClient    *http.Client
	timeout       time.Duration
	createTimeout time.Duration
	fastletPort   int
	tokens        fastletauth.TokenSource
}

// NewFastletClient creates a new fastlet client.
func NewFastletClient(fastletPort int) *FastletClient {
	return &FastletClient{
		httpClient:    &http.Client{},
		timeout:       defaultFastletTimeout,
		createTimeout: defaultFastletCreateTimeout,
		fastletPort:   fastletPort,
	}
}

// SetTokenSource installs the Fastlet control authentication token source.
// When set, every request carries an Authorization header with a
// short-lived ServiceAccount token; when nil, requests are unauthenticated.
func (c *FastletClient) SetTokenSource(tokens fastletauth.TokenSource) {
	c.tokens = tokens
}

// SetTimeout sets the timeout for fastlet API calls.
func (c *FastletClient) SetTimeout(timeout time.Duration) {
	c.timeout = timeout
}

// SetCreateTimeout sets the timeout for the atomic Fastlet admission/create
// request without weakening the timeout used by heartbeats and observations.
func (c *FastletClient) SetCreateTimeout(timeout time.Duration) {
	c.createTimeout = timeout
}

func (c *FastletClient) CreateSandbox(ctx context.Context, fastletIP string, req *CreateSandboxRequest) (*CreateSandboxResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
		ctx = observability.WithIdentity(ctx, observability.Identity{RequestID: req.RequestID, Namespace: req.Identity.Namespace, SandboxName: req.Identity.Name})
	}
	response, err := postFastletJSONWithTimeout[CreateSandboxRequest, CreateSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/create", req, c.createTimeout)
	if err == nil || response == nil {
		return response, err
	}
	var failure *FastletError
	if errors.As(err, &failure) {
		return response, &CreateCallError{Disposition: createDispositionForFailure(response.Disposition, failure), Failure: failure}
	}
	return response, err
}

func (c *FastletClient) InspectSandbox(ctx context.Context, fastletIP string, req *InspectSandboxRequest) (*InspectSandboxResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
	return postFastletJSON[InspectSandboxRequest, InspectSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/inspect", req)
}

func (c *FastletClient) DeleteSandbox(ctx context.Context, fastletIP string, req *DeleteSandboxRequest) (*DeleteSandboxResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
	return postFastletJSON[DeleteSandboxRequest, DeleteSandboxResponse](c, ctx, fastletIP, "/api/v2/fastlet/delete", req)
}

func (c *FastletClient) ReconcileBindings(ctx context.Context, fastletIP string, req *ReconcileBindingsRequest) (*ReconcileBindingsResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
	return postFastletJSON[ReconcileBindingsRequest, ReconcileBindingsResponse](c, ctx, fastletIP, "/api/v2/fastlet/bindings/reconcile", req)
}

func (c *FastletClient) InvokeHandler(ctx context.Context, fastletIP string, req *InvokeHandlerRequest) (*InvokeHandlerResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity)
	}
	return postFastletJSON[InvokeHandlerRequest, InvokeHandlerResponse](c, ctx, fastletIP, "/api/v2/fastlet/handlers/invoke", req)
}

func (c *FastletClient) CreateSnapshot(ctx context.Context, fastletIP string, req *CreateSnapshotRequest) (*CreateSnapshotResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity.Sandbox)
	}
	response, err := postFastletJSON[CreateSnapshotRequest, CreateSnapshotResponse](c, ctx, fastletIP, "/api/v2/fastlet/snapshots/create", req)
	if err == nil || response == nil {
		return response, err
	}
	var failure *FastletError
	if errors.As(err, &failure) {
		return response, &CreateCallError{Disposition: createDispositionForFailure(response.Disposition, failure), Failure: failure}
	}
	return response, err
}

func (c *FastletClient) InspectSnapshot(ctx context.Context, fastletIP string, req *InspectSnapshotRequest) (*InspectSnapshotResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity.Sandbox)
	}
	return postFastletJSON[InspectSnapshotRequest, InspectSnapshotResponse](c, ctx, fastletIP, "/api/v2/fastlet/snapshots/inspect", req)
}

func (c *FastletClient) DeleteSnapshot(ctx context.Context, fastletIP string, req *DeleteSnapshotRequest) (*DeleteSnapshotResponse, error) {
	if req != nil {
		ctx = withFastletIdentity(ctx, req.Identity.Sandbox)
	}
	return postFastletJSON[DeleteSnapshotRequest, DeleteSnapshotResponse](c, ctx, fastletIP, "/api/v2/fastlet/snapshots/delete", req)
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
	return postFastletJSONWithTimeout[Request, Response](c, ctx, fastletIP, path, request, c.timeout)
}

func postFastletJSONWithTimeout[Request any, Response any](c *FastletClient, ctx context.Context, fastletIP, path string, request *Request, timeout time.Duration) (_ *Response, resultErr error) {
	ctx, span := observability.StartClient(ctx, "fastlet.client "+path)
	defer func() { observability.End(span, resultErr) }()
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	requestContext, cancel := c.requestContext(ctx, timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.endpoint(fastletIP, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if err := c.authorize(requestContext, httpRequest); err != nil {
		return nil, err
	}
	observability.InjectHTTP(requestContext, httpRequest.Header)
	return doFastletJSON[Response](c, httpRequest)
}

func getFastletJSON[Response any](c *FastletClient, ctx context.Context, fastletIP, path string) (_ *Response, resultErr error) {
	ctx, span := observability.StartClient(ctx, "fastlet.client "+path)
	defer func() { observability.End(span, resultErr) }()
	requestContext, cancel := c.requestContext(ctx, c.timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodGet, c.endpoint(fastletIP, path), nil)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(requestContext, httpRequest); err != nil {
		return nil, err
	}
	observability.InjectHTTP(requestContext, httpRequest.Header)
	return doFastletJSON[Response](c, httpRequest)
}

// authorize attaches the control auth token. A mint failure fails the call
// closed — no unauthenticated request is ever sent.
func (c *FastletClient) authorize(ctx context.Context, request *http.Request) error {
	if c.tokens == nil {
		return nil
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		// Typed and provably pre-send, so callers classify it as
		// REJECTED_BEFORE_SIDE_EFFECTS rather than UnknownOutcome.
		return &FastletError{Code: ErrorAuthUnavailable, Message: fmt.Sprintf("acquire control auth token: %v", err), Retryable: true}
	}
	request.Header.Set("Authorization", fastletauth.BearerPrefix+token)
	return nil
}

// IsAuthFailure reports whether the failure came from the authentication
// middleware rather than admission.
func IsAuthFailure(failure *FastletError) bool {
	if failure == nil {
		return false
	}
	switch failure.Code {
	case ErrorAuthRejected, ErrorAuthNotAllowed, ErrorAuthUnavailable:
		return true
	default:
		return false
	}
}

// createDispositionForFailure classifies authentication failures as
// REJECTED_BEFORE_SIDE_EFFECTS: the middleware rejected the call before
// admission, so a retry is always safe. Other failures keep the
// Fastlet-reported disposition.
func createDispositionForFailure(reported CreateDisposition, failure *FastletError) CreateDisposition {
	if IsAuthFailure(failure) {
		return CreateDispositionRejectedBeforeSideEffects
	}
	return reported
}

func withFastletIdentity(ctx context.Context, identity SandboxIdentity) context.Context {
	return observability.WithIdentity(ctx, observability.Identity{
		Namespace: identity.Namespace, SandboxName: identity.Name, SandboxUID: identity.SandboxUID, FastletPodUID: identity.FastletPodUID,
		InstanceGeneration: identity.InstanceGeneration, AssignmentAttempt: identity.AssignmentAttempt, RouteGeneration: identity.RouteGeneration,
	})
}

func doFastletJSON[Response any](c *FastletClient, request *http.Request) (*Response, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	// Read the body before classifying the outcome: non-JSON rejections must
	// surface the HTTP status, not a decode error.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxControlResponseBodyBytes {
		return nil, fmt.Errorf("fastlet response exceeds %d bytes (status %d)", maxControlResponseBodyBytes, response.StatusCode)
	}
	var result Response
	if decodeErr := json.Unmarshal(body, &result); decodeErr != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("fastlet request failed with status %d: %s", response.StatusCode, summarizeControlBody(body))
		}
		return nil, decodeErr
	}
	if failure := responseFastletError(any(&result)); failure != nil {
		return &result, failure
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &result, fmt.Errorf("fastlet request failed with status: %d", response.StatusCode)
	}
	return &result, nil
}

// maxControlResponseBodyBytes bounds a control response body; Fastlet
// responses are small JSON documents.
const maxControlResponseBodyBytes = 1 << 20

// summarizeControlBody keeps a bounded snippet of an error body.
func summarizeControlBody(body []byte) string {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 256 {
		snippet = snippet[:256] + "...(truncated)"
	}
	return snippet
}

func responseFastletError(response any) *FastletError {
	switch typed := response.(type) {
	case *CreateSandboxResponse:
		return typed.Error
	case *InspectSandboxResponse:
		return typed.Error
	case *DeleteSandboxResponse:
		return typed.Error
	case *ReconcileBindingsResponse:
		return typed.Error
	case *InvokeHandlerResponse:
		return typed.Error
	case *SandboxDiagnosticsResponse:
		return typed.Error
	case *CreateSnapshotResponse:
		return typed.Error
	case *InspectSnapshotResponse:
		return typed.Error
	case *DeleteSnapshotResponse:
		return typed.Error
	case *SetDrainingResponse:
		return typed.Error
	default:
		return nil
	}
}

func (c *FastletClient) endpoint(fastletIP, path string) string {
	return "http://" + net.JoinHostPort(fastletIP, strconv.Itoa(c.fastletPort)) + path
}

func (c *FastletClient) requestContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline || timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
