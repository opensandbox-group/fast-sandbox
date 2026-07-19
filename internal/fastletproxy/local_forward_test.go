package fastletproxy

import (
	"bufio"
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/routeauth"

	"github.com/stretchr/testify/require"
)

func TestEncodeLocalForwardPreamble(t *testing.T) {
	credential, err := fastletnetwork.GenerateLocalForwardCredential()
	require.NoError(t, err)
	preamble, err := EncodeLocalForwardPreamble(18080, credential)
	require.NoError(t, err)
	require.Len(t, preamble, localForwardPreambleSize)
	require.Equal(t, []byte{'F', 'S', 'B', 'F', 1, 1, 0x46, 0xa0}, preamble[:8])
	_, err = EncodeLocalForwardPreamble(0, credential)
	require.Error(t, err)
}

func TestLocalForwardTransportRejectsNonLoopbackEndpoint(t *testing.T) {
	credential, err := fastletnetwork.GenerateLocalForwardCredential()
	require.NoError(t, err)
	_, err = newLocalForwardTransport(fastletnetwork.AccessDescriptor{
		Kind: fastletnetwork.AccessKindLocalForward, Address: "10.0.0.8:19090", Credential: credential,
	}, 8080, nil)
	require.EqualError(t, err, "local-forward endpoint must use a loopback IP")
}

func TestProxyForwardsThroughLocalTunnelWithSignedTargetPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	const targetPort = uint32(32123)
	credential, err := fastletnetwork.GenerateLocalForwardCredential()
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			result <- acceptErr
			return
		}
		defer connection.Close()
		decodedPort, readErr := fastletnetwork.DecodeLocalForwardPreamble(connection, credential)
		if readErr != nil || decodedPort != targetPort {
			result <- fmt.Errorf("unexpected local-forward handshake port=%d err=%v", decodedPort, readErr)
			return
		}
		request, readErr := http.ReadRequest(bufio.NewReader(connection))
		if readErr != nil {
			result <- readErr
			return
		}
		if request.URL.Path != "/health" || request.Header.Get("Authorization") != "" || request.Header.Get(HeaderRouteGeneration) != "" {
			result <- fmt.Errorf("unexpected tunneled request path=%q headers=%v", request.URL.Path, request.Header)
			return
		}
		_, writeErr := io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 15\r\nConnection: close\r\n\r\nboxlite-tunnel\n")
		result <- writeErr
	}()

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := Route{
		Namespace: "default", SandboxUID: "uid-boxlite", FastletPodUID: "pod-a", AssignmentAttempt: 1, RouteGeneration: 1,
		Access: fastletnetwork.AccessDescriptor{
			Kind: fastletnetwork.AccessKindLocalForward, Address: listener.Addr().String(), Credential: credential,
		}, State: RouteReady,
	}
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: targetPort,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, RoutePath(route.SandboxUID, targetPort)+"/health", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	for name, values := range RouteHeaders(route) {
		request.Header[name] = values
	}
	response := httptest.NewRecorder()
	(&Proxy{Store: store, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "boxlite-tunnel\n", response.Body.String())
	require.NoError(t, <-result)
}
