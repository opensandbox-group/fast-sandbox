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

	fastpathv2 "fast-sandbox/api/proto/v2"

	"google.golang.org/grpc"
)

// EndpointControl is the small FastPath surface needed by data-plane
// adapters. The generated FastPathServiceClient satisfies this interface.
type EndpointControl interface {
	ResolveEndpoint(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error)
}

type SandboxRef struct {
	Name      string
	Namespace string
}

type RouteTarget struct {
	ComponentName string
	Port          uint32
}

func ComponentTarget(name string) RouteTarget {
	return RouteTarget{ComponentName: name}
}

func PortTarget(port uint32) RouteTarget {
	return RouteTarget{Port: port}
}

type RouteResolver interface {
	Resolve(context.Context, SandboxRef, RouteTarget) (Route, error)
}

type Route struct {
	SandboxUID       string
	ComponentName    string
	Protocol         string
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

func (r *EndpointResolver) Resolve(ctx context.Context, sandbox SandboxRef, target RouteTarget) (Route, error) {
	if r == nil || r.Control == nil {
		return Route{}, errors.New("FastPath endpoint resolver is not configured")
	}
	if sandbox.Name == "" {
		return Route{}, errors.New("Sandbox name is required")
	}
	componentName := strings.TrimSpace(target.ComponentName)
	switch {
	case componentName != "" && target.Port != 0:
		return Route{}, errors.New("component name and target port may not both be set")
	case componentName == "" && target.Port == 0:
		return Route{}, errors.New("component name or target port is required")
	}
	namespace := sandbox.Namespace
	if namespace == "" {
		namespace = r.DefaultNamespace
	}
	if namespace == "" {
		namespace = "fast-sandbox"
	}
	requestTarget := &fastpathv2.EndpointTarget{}
	targetDescription := ""
	if componentName != "" {
		requestTarget.Target = &fastpathv2.EndpointTarget_ComponentName{ComponentName: componentName}
		targetDescription = "component " + componentName
	} else {
		requestTarget.Target = &fastpathv2.EndpointTarget_Port{Port: target.Port}
		targetDescription = fmt.Sprintf("port %d", target.Port)
	}
	request := &fastpathv2.ResolveEndpointRequest{
		Sandbox:    &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Namespace: namespace, Name: sandbox.Name}},
		Target:     requestTarget,
		AccessMode: fastpathv2.EndpointAccessMode_CENTRAL_PROXY,
	}
	resolved, err := r.Control.ResolveEndpoint(ctx, request)
	if err != nil {
		return Route{}, fmt.Errorf("resolve Sandbox %s/%s %s: %w", namespace, sandbox.Name, targetDescription, err)
	}
	if resolved.GetSandboxUid() == "" {
		return Route{}, errors.New("FastPath returned a route without a Sandbox UID")
	}
	endpointInfo := resolved.GetEndpoint()
	if endpointInfo == nil {
		return Route{}, errors.New("FastPath returned a route without a resolved endpoint")
	}
	if componentName != "" {
		if endpointInfo.GetComponentName() != componentName || endpointInfo.GetPort() == 0 {
			return Route{}, errors.New("FastPath returned a route for a different Sandbox or Infra Component")
		}
	} else if endpointInfo.GetComponentName() != "" || endpointInfo.GetPort() != target.Port {
		return Route{}, errors.New("FastPath returned a route for a different Sandbox or target port")
	}
	endpoint, err := url.Parse(resolved.GetProxyEndpoint())
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return Route{}, fmt.Errorf("fastPath returned invalid proxy endpoint %q", resolved.GetProxyEndpoint())
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
		SandboxUID: resolved.GetSandboxUid(), ComponentName: endpointInfo.GetComponentName(),
		Protocol: endpointInfo.GetProtocol(), TargetPort: endpointInfo.GetPort(), Endpoint: endpoint,
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
		return nil, fmt.Errorf("infra Component path %q must be absolute", path)
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
