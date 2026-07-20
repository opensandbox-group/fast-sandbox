package sandboxproxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"fast-sandbox/internal/fastletproxy"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/routeauth"
	"go.opentelemetry.io/otel/attribute"
)

const DefaultAddress = ":8080"

type Proxy struct {
	Resolver    Resolver
	Verifier    *routeauth.Verifier
	Transport   http.RoundTripper
	FastletPort int
}

func (p *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request, span := observability.StartHTTPServer(request, "sandbox-proxy")
	started := time.Now()
	metricResult := "success"
	defer func() {
		span.SetAttributes(attribute.String("fast_sandbox.proxy_result", metricResult))
		observability.End(span, nil)
		observeSandboxProxy(metricResult, started)
	}()
	sandboxUID, targetPort, _, err := fastletproxy.ParseRoutePath(request.URL.Path)
	if err != nil {
		metricResult = "invalid_route"
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if p.Resolver == nil || p.Verifier == nil {
		metricResult = "unconfigured"
		http.Error(writer, "Sandbox Proxy is not configured", http.StatusServiceUnavailable)
		return
	}
	token, err := routeBearerToken(request.Header.Get("Authorization"))
	if err != nil {
		metricResult = "missing_credential"
		http.Error(writer, err.Error(), http.StatusUnauthorized)
		return
	}
	route, err := p.Resolver.Resolve(request.Context(), sandboxUID)
	if err != nil {
		metricResult = "resolve_error"
		writeResolveError(writer, err)
		return
	}
	request = request.WithContext(observability.WithIdentity(request.Context(), observability.Identity{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, FastletPodUID: route.FastletPodUID,
		AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration, TargetPort: targetPort,
	}))
	if _, err = p.verify(token, targetPort, route); err != nil {
		// Watch delivery may lag behind a freshly issued credential. One direct
		// API-server read distinguishes temporary cache lag from a stale token.
		route, err = p.Resolver.ResolveFresh(request.Context(), sandboxUID)
		if err != nil {
			metricResult = "resolve_error"
			writeResolveError(writer, err)
			return
		}
		request = request.WithContext(observability.WithIdentity(request.Context(), observability.Identity{
			Namespace: route.Namespace, SandboxUID: route.SandboxUID, FastletPodUID: route.FastletPodUID,
			AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration, TargetPort: targetPort,
		}))
		if _, err = p.verify(token, targetPort, route); err != nil {
			metricResult = "credential_rejected"
			http.Error(writer, "route credential rejected", http.StatusForbidden)
			return
		}
	}
	port := p.FastletPort
	if port <= 0 {
		port = 5780
	}
	upstream := "http://" + route.FastletPodIP + ":" + strconv.Itoa(port)
	transport := p.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false, DisableCompression: true,
			MaxIdleConns: 512, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second,
		}
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(mustParseURL(upstream))
			proxyRequest.Out.Host = proxyRequest.In.Host
			stripForwardingAuthority(proxyRequest.Out.Header)
			proxyRequest.Out.Header.Set("Authorization", "Bearer "+token)
			proxyRequest.Out.Header.Set(fastletproxy.HeaderForwardedNamespace, route.Namespace)
			proxyRequest.Out.Header.Set(fastletproxy.HeaderFastletPodUID, route.FastletPodUID)
			proxyRequest.Out.Header.Set(fastletproxy.HeaderAssignmentAttempt, strconv.FormatInt(route.AssignmentAttempt, 10))
			proxyRequest.Out.Header.Set(fastletproxy.HeaderRouteGeneration, strconv.FormatInt(route.RouteGeneration, 10))
			observability.InjectHTTP(proxyRequest.Out.Context(), proxyRequest.Out.Header)
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			metricResult = "upstream_error"
			http.Error(response, "assigned Fastlet Proxy unavailable: "+proxyErr.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(writer, request)
}

func (p *Proxy) verify(token string, targetPort uint32, route Route) (routeauth.Claims, error) {
	return p.Verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: targetPort,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
}

func routeBearerToken(value string) (string, error) {
	const prefix = "Bearer "
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return "", errors.New("Bearer route credential is required")
	}
	return value[len(prefix):], nil
}

func stripForwardingAuthority(headers http.Header) {
	headers.Del(fastletproxy.HeaderForwardedNamespace)
	headers.Del(fastletproxy.HeaderFastletPodUID)
	headers.Del(fastletproxy.HeaderAssignmentAttempt)
	headers.Del(fastletproxy.HeaderRouteGeneration)
}

func writeResolveError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSandboxNotFound):
		http.Error(writer, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrSandboxNotReady), errors.Is(err, ErrFastletUnavailable):
		writer.Header().Set("Retry-After", "1")
		http.Error(writer, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(writer, err.Error(), http.StatusBadGateway)
	}
}

func mustParseURL(value string) *url.URL {
	parsed, err := url.Parse(value)
	if err != nil {
		panic(fmt.Sprintf("invalid internal proxy URL %q: %v", value, err))
	}
	return parsed
}
