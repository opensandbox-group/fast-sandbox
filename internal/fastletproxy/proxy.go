package fastletproxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/routeauth"
)

const (
	DefaultDataAddress       = ":5780"
	HeaderFastletPodUID      = "X-Fast-Sandbox-Fastlet-Pod-Uid"
	HeaderAssignmentAttempt  = "X-Fast-Sandbox-Assignment-Attempt"
	HeaderRouteGeneration    = "X-Fast-Sandbox-Route-Generation"
	HeaderForwardedNamespace = "X-Fast-Sandbox-Namespace"
)

type Proxy struct {
	Store       *Store
	Verifier    *routeauth.Verifier
	Transport   http.RoundTripper
	DialContext DialContextFunc
}

func (p *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	sandboxUID, targetPort, suffix, err := ParseRoutePath(request.URL.Path)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if p.Store == nil || p.Verifier == nil {
		http.Error(writer, "Fastlet Proxy is not configured", http.StatusServiceUnavailable)
		return
	}
	route, err := p.Store.Lookup(sandboxUID)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, ErrRouteDraining) {
			status = http.StatusServiceUnavailable
		}
		http.Error(writer, err.Error(), status)
		return
	}
	if err := validateFenceHeaders(request.Header, route); err != nil {
		http.Error(writer, err.Error(), http.StatusConflict)
		return
	}
	token, err := bearerToken(request.Header.Get("Authorization"))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusUnauthorized)
		return
	}
	_, err = p.Verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: targetPort,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	if err != nil {
		http.Error(writer, "route credential rejected", http.StatusForbidden)
		return
	}
	var upstream string
	transport := p.Transport
	switch route.Access.Kind {
	case fastletnetwork.AccessKindDirectIP:
		if net.ParseIP(route.Access.Address) == nil {
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
	case fastletnetwork.AccessKindLocalForward:
		transport, err = newLocalForwardTransport(route.Access.Address, targetPort, p.DialContext)
		if err != nil {
			http.Error(writer, "local-forward route is invalid: "+err.Error(), http.StatusNotImplemented)
			return
		}
		// DialContext ignores this logical authority and connects to the
		// runtime-local endpoint after writing the target-port preamble.
		upstream = "sandbox.local"
	default:
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
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			http.Error(response, "sandbox upstream unavailable: "+proxyErr.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(writer, request)
}

func ParseRoutePath(path string) (string, uint32, string, error) {
	const prefix = "/v1/sandboxes/"
	if !strings.HasPrefix(path, prefix) {
		return "", 0, "", errors.New("route path must start with /v1/sandboxes/")
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] != "ports" || parts[2] == "" {
		return "", 0, "", errors.New("route path must be /v1/sandboxes/{uid}/ports/{port}/...")
	}
	uid, err := url.PathUnescape(parts[0])
	if err != nil || uid == "" || strings.Contains(uid, "/") {
		return "", 0, "", errors.New("invalid sandbox UID")
	}
	portValue, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil || portValue == 0 {
		return "", 0, "", errors.New("target port must be between 1 and 65535")
	}
	suffix := "/"
	if len(parts) == 4 && parts[3] != "" {
		suffix += parts[3]
	}
	return uid, uint32(portValue), suffix, nil
}

func validateFenceHeaders(headers http.Header, route Route) error {
	if headers.Get(HeaderFastletPodUID) != route.FastletPodUID || headers.Get(HeaderForwardedNamespace) != route.Namespace {
		return errors.New("request targets a stale Fastlet Pod or namespace")
	}
	attempt, err := strconv.ParseInt(headers.Get(HeaderAssignmentAttempt), 10, 64)
	if err != nil || attempt != route.AssignmentAttempt {
		return errors.New("request targets a stale assignment attempt")
	}
	generation, err := strconv.ParseInt(headers.Get(HeaderRouteGeneration), 10, 64)
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
	headers.Del(HeaderFastletPodUID)
	headers.Del(HeaderAssignmentAttempt)
	headers.Del(HeaderRouteGeneration)
	headers.Del(HeaderForwardedNamespace)
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
	headers.Set(HeaderFastletPodUID, route.FastletPodUID)
	headers.Set(HeaderAssignmentAttempt, strconv.FormatInt(route.AssignmentAttempt, 10))
	headers.Set(HeaderRouteGeneration, strconv.FormatInt(route.RouteGeneration, 10))
	headers.Set(HeaderForwardedNamespace, route.Namespace)
	return headers
}

func RoutePath(sandboxUID string, targetPort uint32) string {
	return fmt.Sprintf("/v1/sandboxes/%s/ports/%d", url.PathEscape(sandboxUID), targetPort)
}
