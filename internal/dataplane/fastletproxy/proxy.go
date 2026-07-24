package fastletproxy

import (
	"errors"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	routeauth "fast-sandbox/internal/dataplane/auth"
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
	defer func() {
		span.SetAttributes(
			attribute.String("fast_sandbox.access_kind", metricAccess),
			attribute.String("fast_sandbox.proxy_result", metricResult),
		)
		observability.End(span, nil)
		observeFastletProxy(metricAccess, metricResult, started)
	}()
	sandboxUID, targetPort, suffix, err := dataplane.ParseRoutePath(request.URL.Path)
	if err != nil {
		metricResult = "invalid_route"
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if p.Store == nil || p.Verifier == nil {
		metricResult = "unconfigured"
		http.Error(writer, "Fastlet Proxy is not configured", http.StatusServiceUnavailable)
		return
	}
	route, err := p.Store.Lookup(sandboxUID)
	if err != nil {
		metricResult = "route_unavailable"
		status := http.StatusNotFound
		if errors.Is(err, ErrRouteDraining) {
			status = http.StatusServiceUnavailable
		}
		http.Error(writer, err.Error(), status)
		return
	}
	request = request.WithContext(observability.WithIdentity(request.Context(), observability.Identity{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, FastletPodUID: route.FastletPodUID,
		AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration, TargetPort: targetPort,
	}))
	if err := validateFenceHeaders(request.Header, route); err != nil {
		metricResult = "stale_fence"
		http.Error(writer, err.Error(), http.StatusConflict)
		return
	}
	token, err := bearerToken(request.Header.Get("Authorization"))
	if err != nil {
		metricResult = "missing_credential"
		http.Error(writer, err.Error(), http.StatusUnauthorized)
		return
	}
	_, err = p.Verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: targetPort,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	if err != nil {
		metricResult = "credential_rejected"
		http.Error(writer, "route credential rejected", http.StatusForbidden)
		return
	}
	var upstream string
	transport := p.Transport
	switch route.Access.Kind {
	case dataplane.AccessKindDirectIP:
		metricAccess = string(dataplane.AccessKindDirectIP)
		if net.ParseIP(route.Access.Address) == nil {
			metricResult = "invalid_access"
			http.Error(writer, "direct IP route address is invalid", http.StatusNotImplemented)
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
			http.Error(writer, "local-forward route is invalid: "+err.Error(), http.StatusNotImplemented)
			return
		}
		// DialContext ignores this logical authority and connects to the
		// runtime-local endpoint after writing the target-port preamble.
		upstream = "sandbox.local"
	default:
		metricResult = "unsupported_access"
		http.Error(writer, "route access kind is not supported", http.StatusNotImplemented)
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
			stripUpstreamHeaders(proxyRequest.Out.Header, route.UpstreamHeadersByPort)
			for name, value := range route.UpstreamHeadersByPort[targetPort] {
				proxyRequest.Out.Header.Set(name, value)
			}
			observability.InjectHTTP(proxyRequest.Out.Context(), proxyRequest.Out.Header)
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			metricResult = "upstream_error"
			http.Error(response, "sandbox upstream unavailable: "+proxyErr.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(writer, request)
}

func validateFenceHeaders(headers http.Header, route Route) error {
	if headers.Get(dataplane.HeaderFastletPodUID) != route.FastletPodUID || headers.Get(dataplane.HeaderForwardedNamespace) != route.Namespace {
		return errors.New("request targets a stale Fastlet Pod or namespace")
	}
	attempt, err := strconv.ParseInt(headers.Get(dataplane.HeaderAssignmentAttempt), 10, 64)
	if err != nil || attempt != route.AssignmentAttempt {
		return errors.New("request targets a stale assignment attempt")
	}
	generation, err := strconv.ParseInt(headers.Get(dataplane.HeaderRouteGeneration), 10, 64)
	if err != nil || generation != route.RouteGeneration {
		return errors.New("request targets a stale route generation")
	}
	return nil
}

func bearerToken(value string) (string, error) {
	prefix := "Bearer "
	if !strings.HasPrefix(value, prefix) || strings.TrimSpace(strings.TrimPrefix(value, prefix)) == "" {
		return "", errors.New("Bearer route credential is required")
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix)), nil
}

func stripRouteHeaders(headers http.Header) {
	headers.Del("Authorization")
	headers.Del(dataplane.HeaderFastletPodUID)
	headers.Del(dataplane.HeaderAssignmentAttempt)
	headers.Del(dataplane.HeaderRouteGeneration)
	headers.Del(dataplane.HeaderForwardedNamespace)
}

func stripUpstreamHeaders(headers http.Header, byPort map[uint32]map[string]string) {
	for _, scoped := range byPort {
		for name := range scoped {
			headers.Del(name)
		}
	}
}

func RouteHeaders(route Route) http.Header {
	headers := make(http.Header)
	headers.Set(dataplane.HeaderFastletPodUID, route.FastletPodUID)
	headers.Set(dataplane.HeaderAssignmentAttempt, strconv.FormatInt(route.AssignmentAttempt, 10))
	headers.Set(dataplane.HeaderRouteGeneration, strconv.FormatInt(route.RouteGeneration, 10))
	headers.Set(dataplane.HeaderForwardedNamespace, route.Namespace)
	return headers
}
