package fastletproxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	routeauth "fast-sandbox/internal/dataplane/auth"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fast-sandbox/internal/observability"

	"go.opentelemetry.io/otel/attribute"
)

const DefaultDataAddress = ":5780"

type Proxy struct {
	Store       *Store
	Verifier    *routeauth.Verifier
	Transport   http.RoundTripper
	DialContext DialContextFunc
}

func (p *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request, span := observability.StartHTTPServer(request, "fastlet-proxy")
	started := time.Now()
	metricAccess, metricResult := "", "success"
	var requestErr error
	defer func() {
		span.SetAttributes(
			attribute.String("fast_sandbox.access_kind", metricAccess),
			attribute.String("fast_sandbox.proxy_result", metricResult),
		)
		observability.End(span, requestErr)
		observeFastletProxy(metricAccess, metricResult, started)
	}()
	sandboxUID, targetPort, componentName, suffix, err := parseTarget(request.URL.Path)
	if err != nil {
		metricResult = "invalid_route"
		requestErr = err
		writeProxyError(writer, http.StatusBadRequest, dataplane.ProxyErrorRouteUnavailable, err.Error())
		return
	}
	if p.Store == nil || p.Verifier == nil {
		metricResult = "unconfigured"
		requestErr = errors.New("Fastlet Proxy is not configured")
		writeProxyError(writer, http.StatusServiceUnavailable, dataplane.ProxyErrorRouteUnavailable, requestErr.Error())
		return
	}
	route, err := p.Store.Lookup(sandboxUID)
	if err != nil {
		metricResult = "route_unavailable"
		requestErr = err
		status := http.StatusNotFound
		if errors.Is(err, ErrRouteDraining) {
			status = http.StatusServiceUnavailable
		}
		writeProxyError(writer, status, dataplane.ProxyErrorRouteUnavailable, err.Error())
		return
	}
	if componentName != "" {
		component, found := route.Components[componentName]
		if !found {
			metricResult = "component_not_found"
			requestErr = errors.New("Infra Component route not found")
			writeProxyError(writer, http.StatusNotFound, dataplane.ProxyErrorComponentNotFound, requestErr.Error())
			return
		}
		if !strings.EqualFold(component.Protocol, "HTTP") {
			metricResult = "unsupported_component_protocol"
			requestErr = fmt.Errorf("infra Component protocol %q is not supported", component.Protocol)
			writeProxyError(writer, http.StatusNotImplemented, dataplane.ProxyErrorComponentNotReady, requestErr.Error())
			return
		}
		targetPort = component.Port
	}
	targetKind := routeauth.TargetKindPort
	protocol := "HTTP"
	if componentName != "" {
		targetKind = routeauth.TargetKindComponent
		protocol = route.Components[componentName].Protocol
	}
	request = request.WithContext(observability.WithIdentity(request.Context(), observability.Identity{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, FastletPodUID: route.FastletPodUID,
		AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration, TargetPort: targetPort,
	}))
	token, err := routeCredential(request.Header.Get(dataplane.HeaderRouteCredential))
	if err != nil {
		metricResult = "missing_credential"
		requestErr = err
		writeProxyError(writer, http.StatusUnauthorized, dataplane.ProxyErrorCredentialRejected, err.Error())
		return
	}
	_, err = p.Verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: targetPort,
		TargetKind: targetKind, ComponentName: componentName, Protocol: protocol,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	if err != nil {
		metricResult = "credential_rejected"
		requestErr = errors.New("route credential rejected")
		writeProxyError(writer, http.StatusForbidden, dataplane.ProxyErrorCredentialRejected, "route credential rejected")
		return
	}
	var upstream string
	transport := p.Transport
	switch route.Access.Kind {
	case dataplane.AccessKindDirectIP:
		metricAccess = string(dataplane.AccessKindDirectIP)
		if net.ParseIP(route.Access.Address) == nil {
			metricResult = "invalid_access"
			requestErr = errors.New("direct IP route address is invalid")
			writeProxyError(writer, http.StatusNotImplemented, dataplane.ProxyErrorRouteUnavailable, requestErr.Error())
			return
		}
		upstream = net.JoinHostPort(route.Access.Address, strconv.Itoa(int(targetPort)))
		if transport == nil {
			transport = &http.Transport{
				Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false, DisableCompression: true,
				MaxIdleConns: 256, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
			}
		}
	case dataplane.AccessKindLocalForward:
		metricAccess = string(dataplane.AccessKindLocalForward)
		transport, err = newLocalForwardTransport(route.Access, targetPort, p.DialContext)
		if err != nil {
			metricResult = "invalid_access"
			requestErr = err
			writeProxyError(writer, http.StatusNotImplemented, dataplane.ProxyErrorRouteUnavailable, "local-forward route is invalid: "+err.Error())
			return
		}
		// DialContext ignores this logical authority and connects to the
		// runtime-local endpoint after writing the target-port preamble.
		upstream = "sandbox.local"
	default:
		metricResult = "unsupported_access"
		requestErr = fmt.Errorf("route access kind %q is not supported", route.Access.Kind)
		writeProxyError(writer, http.StatusNotImplemented, dataplane.ProxyErrorRouteUnavailable, requestErr.Error())
		return
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.Out.URL.Scheme = "http"
			proxyRequest.Out.URL.Host = upstream
			proxyRequest.Out.URL.Path = suffix
			proxyRequest.Out.URL.RawPath = ""
			proxyRequest.Out.Host = proxyRequest.In.Host
			stripRouteHeaders(proxyRequest.Out.Header)
			observability.InjectHTTP(proxyRequest.Out.Context(), proxyRequest.Out.Header)
		},
		ModifyResponse: func(response *http.Response) error {
			// A Sandbox application cannot forge a platform routing error.
			response.Header.Del(dataplane.HeaderProxyError)
			return nil
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			metricResult = "upstream_error"
			requestErr = proxyErr
			writeProxyError(response, http.StatusBadGateway, dataplane.ProxyErrorUpstreamUnavailable, "sandbox upstream unavailable: "+proxyErr.Error())
		},
	}
	proxy.ServeHTTP(writer, request)
}

func writeProxyError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set(dataplane.HeaderProxyError, code)
	http.Error(writer, message, status)
}

func parseTarget(path string) (sandboxUID string, port uint32, component, suffix string, err error) {
	if strings.HasPrefix(path, "/v2/sandboxes/") {
		sandboxUID, component, suffix, err = dataplane.ParseComponentRoutePath(path)
		return
	}
	sandboxUID, port, suffix, err = dataplane.ParseRoutePath(path)
	return
}

func routeCredential(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("route credential is required")
	}
	return strings.TrimSpace(value), nil
}

func stripRouteHeaders(headers http.Header) {
	headers.Del(dataplane.HeaderRouteCredential)
	headers.Del(dataplane.HeaderFastletPodUID)
	headers.Del(dataplane.HeaderAssignmentAttempt)
	headers.Del(dataplane.HeaderRouteGeneration)
	headers.Del(dataplane.HeaderForwardedNamespace)
}

func RouteHeaders(route Route) http.Header {
	headers := make(http.Header)
	headers.Set(dataplane.HeaderFastletPodUID, route.FastletPodUID)
	headers.Set(dataplane.HeaderAssignmentAttempt, strconv.FormatInt(route.AssignmentAttempt, 10))
	headers.Set(dataplane.HeaderRouteGeneration, strconv.FormatInt(route.RouteGeneration, 10))
	headers.Set(dataplane.HeaderForwardedNamespace, route.Namespace)
	return headers
}
