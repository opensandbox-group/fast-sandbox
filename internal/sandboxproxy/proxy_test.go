package sandboxproxy

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/fastletproxy"
	"fast-sandbox/internal/routeauth"
	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	cached     Route
	fresh      Route
	err        error
	freshCalls int
}

func (r *fakeResolver) Resolve(context.Context, string) (Route, error) {
	if r.err != nil {
		return Route{}, r.err
	}
	return r.cached, nil
}

func (r *fakeResolver) ResolveFresh(context.Context, string) (Route, error) {
	r.freshCalls++
	if r.err != nil {
		return Route{}, r.err
	}
	return r.fresh, nil
}

func TestSandboxProxyFallsBackOnStaleWatchAndForwardsToAssignedFastlet(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/health", request.URL.Path)
		_, _ = io.WriteString(writer, "ready")
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	backendPort, err := strconv.ParseUint(backendURL.Port(), 10, 16)
	require.NoError(t, err)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	store := fastletproxy.NewStore()
	localRoute := fastletproxy.Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-new", AssignmentAttempt: 2, RouteGeneration: 2,
		Access: fastletnetwork.AccessDescriptor{Kind: fastletnetwork.AccessKindDirectIP, Address: backendURL.Hostname()}, State: fastletproxy.RouteReady,
	}
	_, err = store.Apply(localRoute)
	require.NoError(t, err)
	fastlet := httptest.NewServer(&fastletproxy.Proxy{Store: store, Verifier: verifier})
	defer fastlet.Close()
	fastletURL, err := url.Parse(fastlet.URL)
	require.NoError(t, err)
	fastletPort, err := strconv.Atoi(fastletURL.Port())
	require.NoError(t, err)

	current := Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-new", FastletPodIP: fastletURL.Hostname(),
		AssignmentAttempt: 2, RouteGeneration: 2,
	}
	resolver := &fakeResolver{cached: Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-old", FastletPodIP: fastletURL.Hostname(),
		AssignmentAttempt: 1, RouteGeneration: 1,
	}, fresh: current}
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetPort: uint32(backendPort), FastletPodUID: "pod-new",
		AssignmentAttempt: 2, RouteGeneration: 2,
	})
	require.NoError(t, err)

	server := httptest.NewServer(&Proxy{Resolver: resolver, Verifier: verifier, FastletPort: fastletPort})
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+fastletproxy.RoutePath("uid-a", uint32(backendPort))+"/health", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(fastletproxy.HeaderFastletPodUID, "attacker-value")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "ready", string(body))
	require.Equal(t, 1, resolver.freshCalls)
}

func TestSandboxProxyReportsRetryableNotReady(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodGet, fastletproxy.RoutePath("uid-a", 8080), nil)
	request.Header.Set("Authorization", "Bearer invalid")
	response := httptest.NewRecorder()
	(&Proxy{Resolver: &fakeResolver{err: ErrSandboxNotReady}, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "1", response.Header().Get("Retry-After"))
}
