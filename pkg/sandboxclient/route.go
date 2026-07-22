// Package sandboxclient contains protocol adapters that resolve a Sandbox data
// plane route through FastPath and then talk directly to an injected Infra
// Component. FastPath remains a lifecycle and route-discovery API; it does not
// implement Exec or File semantics.
package sandboxclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"google.golang.org/grpc"
)

// EndpointControl is the small FastPath surface needed by data-plane
// adapters. The generated FastPathServiceClient satisfies this interface.
type EndpointControl interface {
	GetSandbox(context.Context, *fastpathv1.GetRequest, ...grpc.CallOption) (*fastpathv1.SandboxInfo, error)
	ResolveEndpoint(context.Context, *fastpathv1.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv1.ResolveEndpointResponse, error)
}

type SandboxRef struct {
	Name      string
	Namespace string
}

type Route struct {
	SandboxUID       string
	TargetPort       uint32
	Endpoint         *url.URL
	RequiredHeaders  http.Header
	RouteGeneration  int64
	ExpiresAtUnixSec int64
}

// EndpointResolver converts a user-visible Sandbox name into a short-lived,
// instance-fenced Sandbox Proxy route.
type EndpointResolver struct {
	Control          EndpointControl
	ProxyBaseURL     string
	DefaultNamespace string
}

func (r *EndpointResolver) Resolve(ctx context.Context, sandbox SandboxRef, targetPort uint32) (Route, error) {
	if r == nil || r.Control == nil {
		return Route{}, errors.New("FastPath endpoint resolver is not configured")
	}
	if sandbox.Name == "" {
		return Route{}, errors.New("Sandbox name is required")
	}
	if targetPort == 0 {
		return Route{}, errors.New("target port is required")
	}
	namespace := sandbox.Namespace
	if namespace == "" {
		namespace = r.DefaultNamespace
	}
	if namespace == "" {
		namespace = "default"
	}
	info, err := r.Control.GetSandbox(ctx, &fastpathv1.GetRequest{SandboxName: sandbox.Name, Namespace: namespace})
	if err != nil {
		return Route{}, fmt.Errorf("get Sandbox %s/%s: %w", namespace, sandbox.Name, err)
	}
	if info.GetSandboxUid() == "" {
		return Route{}, fmt.Errorf("Sandbox %s/%s has no CRD UID", namespace, sandbox.Name)
	}
	resolved, err := r.Control.ResolveEndpoint(ctx, &fastpathv1.ResolveEndpointRequest{
		SandboxUid: info.GetSandboxUid(), TargetPort: targetPort, Protocol: "http",
	})
	if err != nil {
		return Route{}, fmt.Errorf("resolve Sandbox %s/%s port %d: %w", namespace, sandbox.Name, targetPort, err)
	}
	if resolved.GetSandboxUid() != info.GetSandboxUid() || resolved.GetTargetPort() != targetPort {
		return Route{}, errors.New("FastPath returned a route for a different Sandbox or target port")
	}
	endpoint, err := url.Parse(resolved.GetProxyEndpoint())
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return Route{}, fmt.Errorf("FastPath returned invalid proxy endpoint %q", resolved.GetProxyEndpoint())
	}
	if r.ProxyBaseURL != "" {
		endpoint, err = replaceRouteAuthority(endpoint, r.ProxyBaseURL)
		if err != nil {
			return Route{}, err
		}
	}
	headers := make(http.Header, len(resolved.GetRequiredHeaders()))
	for name, value := range resolved.GetRequiredHeaders() {
		if strings.TrimSpace(name) == "" {
			return Route{}, errors.New("FastPath returned an empty required-header name")
		}
		headers.Set(name, value)
	}
	return Route{
		SandboxUID: resolved.GetSandboxUid(), TargetPort: resolved.GetTargetPort(), Endpoint: endpoint,
		RequiredHeaders: headers, RouteGeneration: resolved.GetRouteGeneration(), ExpiresAtUnixSec: resolved.GetExpiresAtUnixSeconds(),
	}, nil
}

func replaceRouteAuthority(route *url.URL, proxyBaseURL string) (*url.URL, error) {
	base, err := url.Parse(proxyBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid Sandbox Proxy base URL %q", proxyBaseURL)
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("Sandbox Proxy base URL must not contain a query or fragment")
	}
	rewritten := *route
	rewritten.Scheme = base.Scheme
	rewritten.Host = base.Host
	rewritten.User = base.User
	rewritten.Path = strings.TrimRight(base.Path, "/") + route.Path
	rewritten.RawPath = ""
	return &rewritten, nil
}

func (r Route) RequestURL(path string, query url.Values) (*url.URL, error) {
	if r.Endpoint == nil {
		return nil, errors.New("Sandbox route endpoint is missing")
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("Infra Component path %q must be absolute", path)
	}
	result := *r.Endpoint
	result.Path = strings.TrimRight(r.Endpoint.Path, "/") + path
	result.RawPath = ""
	result.RawQuery = query.Encode()
	result.Fragment = ""
	return &result, nil
}

func (r Route) ApplyHeaders(request *http.Request) {
	for name, values := range r.RequiredHeaders {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
}
