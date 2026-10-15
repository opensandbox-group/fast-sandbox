package fastpath

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func relayRequest(name string, handler string, payload []byte) *fastpathv2.InvokeHandlerActionRequest {
	return &fastpathv2.InvokeHandlerActionRequest{
		Sandbox: expectedReference(name, "default"),
		Handler: handler,
		Payload: payload,
	}
}

func TestInvokeHandlerActionRelaysToAssignedFastlet(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	created, err := server.CreateSandbox(context.Background(), createRequest("relay-a"))
	require.NoError(t, err)

	payload := []byte(`{"revision":7,"vault":{"token":"t"}}`)
	response, err := server.InvokeHandlerAction(context.Background(), relayRequest("relay-a", "egress", payload))
	require.NoError(t, err)
	require.Equal(t, uint32(http.StatusOK), response.Result.Status)
	require.Equal(t, []byte(`{"ok":true}`), response.Result.Body)

	fastlet.mu.Lock()
	defer fastlet.mu.Unlock()
	require.Len(t, fastlet.invokeRequests, 1)
	relayed := fastlet.invokeRequests[0]
	require.Equal(t, "egress", relayed.Handler)
	require.Equal(t, payload, relayed.Payload)
	require.Equal(t, created.Sandbox.GetIdentity().GetUid(), relayed.Identity.SandboxUID)
	require.Equal(t, "relay-a", relayed.Identity.Name)
	require.Equal(t, "default", relayed.Identity.Namespace)
	require.Equal(t, "pod-a", relayed.Identity.FastletPodUID)
	require.Equal(t, []string{"10.0.0.1"}, fastlet.invokeIPs)
}

func TestInvokeHandlerActionReturnsHandlerOutcomeVerbatim(t *testing.T) {
	server, _, _, fastlet := newV2Server(t)
	_, err := server.CreateSandbox(context.Background(), createRequest("relay-b"))
	require.NoError(t, err)

	// A 4xx/5xx handler status is the per-handler result, not an RPC error.
	fastlet.mu.Lock()
	fastlet.invokeResponse = &fastletapi.InvokeHandlerResponse{Status: http.StatusConflict, Body: []byte(`{"error":"exists"}`)}
	fastlet.mu.Unlock()
	response, err := server.InvokeHandlerAction(context.Background(), relayRequest("relay-b", "egress", []byte(`{}`)))
	require.NoError(t, err)
	require.Equal(t, uint32(http.StatusConflict), response.Result.Status)
	require.Equal(t, []byte(`{"error":"exists"}`), response.Result.Body)

	// The response body is relayed byte-for-byte, including the empty case.
	fastlet.mu.Lock()
	fastlet.invokeResponse = &fastletapi.InvokeHandlerResponse{Status: http.StatusNoContent}
	fastlet.mu.Unlock()
	response, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-b", "audit", []byte(`{}`)))
	require.NoError(t, err)
	require.Equal(t, uint32(http.StatusNoContent), response.Result.Status)
	require.Empty(t, response.Result.Body)
}

func TestInvokeHandlerActionValidatesReferenceAndHandler(t *testing.T) {
	server, k8sClient, _, fastlet := newV2Server(t)
	_, err := server.CreateSandbox(context.Background(), createRequest("relay-c"))
	require.NoError(t, err)

	_, err = server.InvokeHandlerAction(context.Background(), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = server.InvokeHandlerAction(context.Background(), &fastpathv2.InvokeHandlerActionRequest{Handler: "egress"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-c", "", []byte(`{}`)))
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = server.InvokeHandlerAction(context.Background(), relayRequest("missing-sandbox", "egress", []byte(`{}`)))
	require.Equal(t, codes.NotFound, status.Code(err))

	wrong := relayRequest("relay-c", "egress", []byte(`{}`))
	wrong.Sandbox.ExpectedUid = "replacement-uid"
	_, err = server.InvokeHandlerAction(context.Background(), wrong)
	require.Equal(t, codes.Aborted, status.Code(err))

	_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-c", "vault", []byte(`{}`)))
	require.Equal(t, codes.NotFound, status.Code(err))
	fastlet.mu.Lock()
	require.Empty(t, fastlet.invokeRequests, "validation failures never reach a Fastlet")
	fastlet.mu.Unlock()

	// A Sandbox the Fastlet does not manage is surfaced, not swallowed.
	unassigned := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "relay-unassigned", Namespace: "default", UID: types.UID("uid-unassigned")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	require.NoError(t, k8sClient.Create(context.Background(), unassigned))
	_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-unassigned", "egress", []byte(`{}`)))
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestInvokeHandlerActionSurfacesFastletFailures(t *testing.T) {
	t.Run("fastlet gone", func(t *testing.T) {
		server, _, registry, _ := newV2Server(t)
		_, err := server.CreateSandbox(context.Background(), createRequest("relay-d"))
		require.NoError(t, err)
		registry.fastlets = nil
		_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-d", "egress", []byte(`{}`)))
		require.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("sandbox unknown to fastlet", func(t *testing.T) {
		server, _, _, fastlet := newV2Server(t)
		_, err := server.CreateSandbox(context.Background(), createRequest("relay-e"))
		require.NoError(t, err)
		fastlet.mu.Lock()
		fastlet.invokeFailure = &fastletapi.FastletError{Code: fastletapi.ErrorNotFound, Message: "Sandbox is not managed by this Fastlet"}
		fastlet.mu.Unlock()
		_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-e", "egress", []byte(`{}`)))
		require.Equal(t, codes.NotFound, status.Code(err))
	})
	t.Run("handler unreachable", func(t *testing.T) {
		server, _, _, fastlet := newV2Server(t)
		_, err := server.CreateSandbox(context.Background(), createRequest("relay-f"))
		require.NoError(t, err)
		fastlet.mu.Lock()
		fastlet.invokeFailure = &fastletapi.FastletError{Code: fastletapi.ErrorActionUnavailable, Message: "handler loopback connection refused", Retryable: true}
		fastlet.mu.Unlock()
		_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-f", "egress", []byte(`{}`)))
		require.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("transport lost", func(t *testing.T) {
		server, _, _, fastlet := newV2Server(t)
		_, err := server.CreateSandbox(context.Background(), createRequest("relay-g"))
		require.NoError(t, err)
		fastlet.mu.Lock()
		fastlet.invokeFailure = errors.New("connection refused")
		fastlet.mu.Unlock()
		_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-g", "egress", []byte(`{}`)))
		require.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("no handler result", func(t *testing.T) {
		server, _, _, fastlet := newV2Server(t)
		_, err := server.CreateSandbox(context.Background(), createRequest("relay-h"))
		require.NoError(t, err)
		fastlet.mu.Lock()
		fastlet.invokeResponse = &fastletapi.InvokeHandlerResponse{}
		fastlet.mu.Unlock()
		_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-h", "egress", []byte(`{}`)))
		require.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("malformed handler status", func(t *testing.T) {
		server, _, _, fastlet := newV2Server(t)
		_, err := server.CreateSandbox(context.Background(), createRequest("relay-i"))
		require.NoError(t, err)
		for _, malformed := range []int32{-1, 42, 600, -2147483648} {
			fastlet.mu.Lock()
			fastlet.invokeResponse = &fastletapi.InvokeHandlerResponse{Status: malformed, Body: []byte(`{"ok":true}`)}
			fastlet.mu.Unlock()
			_, err = server.InvokeHandlerAction(context.Background(), relayRequest("relay-i", "egress", []byte(`{}`)))
			require.Equal(t, codes.Unavailable, status.Code(err), "status %d", malformed)
			require.Contains(t, status.Convert(err).Message(), "invalid handler status")
		}
	})
}
