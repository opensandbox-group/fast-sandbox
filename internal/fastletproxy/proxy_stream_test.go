package fastletproxy

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"io"
	"net"
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

type proxyHarness struct {
	server *httptest.Server
	route  Route
	token  string
	port   uint32
}

func newProxyHarness(t *testing.T, upstream *httptest.Server) *proxyHarness {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	parsedPort, err := strconv.ParseUint(upstreamURL.Port(), 10, 16)
	require.NoError(t, err)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := Route{
		Namespace: "default", SandboxUID: "uid-stream", FastletPodUID: "pod-a", AssignmentAttempt: 1, RouteGeneration: 1,
		Access: fastletnetwork.AccessDescriptor{Kind: fastletnetwork.AccessKindDirectIP, Address: upstreamURL.Hostname()}, State: RouteReady,
	}
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	port := uint32(parsedPort)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: port, FastletPodUID: route.FastletPodUID,
		AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	require.NoError(t, err)
	return &proxyHarness{server: httptest.NewServer(&Proxy{Store: store, Verifier: verifier}), route: route, token: token, port: port}
}

func (h *proxyHarness) request(t *testing.T, ctx context.Context, suffix string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+RoutePath(h.route.SandboxUID, h.port)+suffix, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+h.token)
	for name, values := range RouteHeaders(h.route) {
		request.Header[name] = values
	}
	return request
}

func TestProxyStreamsLargeChunkedResponseWithoutBuffering(t *testing.T) {
	chunk := strings.Repeat("x", 32*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		flusher := writer.(http.Flusher)
		for index := 0; index < 64; index++ {
			_, _ = io.WriteString(writer, chunk)
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	harness := newProxyHarness(t, upstream)
	defer harness.server.Close()
	response, err := http.DefaultClient.Do(harness.request(t, context.Background(), "/large"))
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, 64*len(chunk), len(body))
}

func TestProxyPropagatesRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	harness := newProxyHarness(t, upstream)
	defer harness.server.Close()
	requestContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(harness.request(t, requestContext, "/cancel"))
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	require.Error(t, <-done)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not observe cancellation")
	}
}

func TestProxyTransparentlyUpgradesWebSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "websocket", strings.ToLower(request.Header.Get("Upgrade")))
		connection, buffer, err := writer.(http.Hijacker).Hijack()
		require.NoError(t, err)
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		require.NoError(t, buffer.Flush())
		payload := make([]byte, 4)
		_, err = io.ReadFull(connection, payload)
		require.NoError(t, err)
		_, _ = connection.Write(payload)
	}))
	defer upstream.Close()
	harness := newProxyHarness(t, upstream)
	defer harness.server.Close()
	proxyURL, err := url.Parse(harness.server.URL)
	require.NoError(t, err)
	connection, err := net.Dial("tcp", proxyURL.Host)
	require.NoError(t, err)
	defer connection.Close()
	path := RoutePath(harness.route.SandboxUID, harness.port) + "/ws"
	_, err = io.WriteString(connection, "GET "+path+" HTTP/1.1\r\nHost: "+proxyURL.Host+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer "+harness.token+"\r\n"+
		HeaderFastletPodUID+": "+harness.route.FastletPodUID+"\r\n"+
		HeaderAssignmentAttempt+": 1\r\n"+HeaderRouteGeneration+": 1\r\n"+
		HeaderForwardedNamespace+": default\r\n\r\n")
	require.NoError(t, err)
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, statusLine, "101")
	for {
		line, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		if line == "\r\n" {
			break
		}
	}
	_, err = connection.Write([]byte("PING"))
	require.NoError(t, err)
	echo := make([]byte, 4)
	_, err = io.ReadFull(reader, echo)
	require.NoError(t, err)
	require.Equal(t, "PING", string(echo))
}
