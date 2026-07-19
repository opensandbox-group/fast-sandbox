package fastletproxy

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/routeauth"
	"github.com/stretchr/testify/require"
)

func TestProxyForwardsArbitraryPortAndStripsRouteAuthority(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/command/run", request.URL.Path)
		require.Equal(t, "large=true", request.URL.RawQuery)
		require.Empty(t, request.Header.Get("Authorization"))
		require.Empty(t, request.Header.Get(HeaderRouteGeneration))
		require.Equal(t, "internal-secret", request.Header.Get("X-Upstream-Auth"))
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: ready\n\n")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	portNumber, err := parseTestPort(upstreamURL.Port())
	require.NoError(t, err)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-a", AssignmentAttempt: 4, RouteGeneration: 7,
		Access: fastletnetwork.AccessDescriptor{Kind: fastletnetwork.AccessKindDirectIP, Address: upstreamURL.Hostname()},
		State:  RouteReady, UpstreamHeaders: map[string]string{"X-Upstream-Auth": "internal-secret"},
	}
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: portNumber,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	require.NoError(t, err)

	proxy := httptest.NewServer(&Proxy{Store: store, Verifier: verifier})
	defer proxy.Close()
	request, err := http.NewRequest(http.MethodPost, proxy.URL+RoutePath("uid-a", portNumber)+"/command/run?large=true", strings.NewReader("payload"))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	for name, values := range RouteHeaders(route) {
		request.Header[name] = values
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "data: ready\n\n", string(body))
}

func TestProxyRejectsStaleCredentialAndFenceHeader(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := testRoute(2)
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: 8080,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: 1,
	})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, RoutePath(route.SandboxUID, 8080), nil)
	request.Header.Set("Authorization", "Bearer "+token)
	for name, values := range RouteHeaders(route) {
		request.Header[name] = values
	}
	response := httptest.NewRecorder()
	(&Proxy{Store: store, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)

	request = httptest.NewRequest(http.MethodGet, RoutePath(route.SandboxUID, 8080), nil)
	request.Header.Set("Authorization", "Bearer "+token)
	for name, values := range RouteHeaders(route) {
		request.Header[name] = values
	}
	request.Header.Set(HeaderAssignmentAttempt, "1")
	response = httptest.NewRecorder()
	(&Proxy{Store: store, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code)
}

func parseTestPort(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint32(parsed), nil
}
