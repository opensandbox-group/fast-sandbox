package action

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	actionapi "fast-sandbox/internal/protocol/action"
)

func (f *fakeCaller) snapshotRelay() []recordedRelay {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRelay(nil), f.relayCalls...)
}

func TestRelayHandlerResolvesDeclaredPortAndPassesThrough(t *testing.T) {
	caller := &fakeCaller{instanceID: "handler-1", relayStatus: http.StatusConflict, relayBody: []byte(`{"error":"exists"}`)}
	manager := readyManager(t, []apiv1alpha2.ActionHandler{handler("egress", 18080)}, caller)
	attachment := attachment("uid-a")
	require.NoError(t, manager.RegisterDesired(attachment, 1, nil))

	status, body, err := manager.RelayHandler(context.Background(), attachment, "egress", http.MethodPut, "/vault/uid-a", []byte(`{"revision":1}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, status, "the handler's status is the result, not an error")
	require.Equal(t, []byte(`{"error":"exists"}`), body)

	calls := caller.snapshotRelay()
	require.Len(t, calls, 1)
	require.Equal(t, int32(18080), calls[0].port)
	require.Equal(t, "uid-a", calls[0].sandboxUID)
	require.Equal(t, http.MethodPut, calls[0].method)
	require.Equal(t, "/vault/uid-a", calls[0].path)
	require.Equal(t, []byte(`{"revision":1}`), calls[0].body)
}

func TestRelayHandlerRejectsUnknownHandler(t *testing.T) {
	caller := &fakeCaller{instanceID: "handler-1"}
	manager := readyManager(t, []apiv1alpha2.ActionHandler{handler("egress", 18080)}, caller)
	attachment := attachment("uid-a")
	require.NoError(t, manager.RegisterDesired(attachment, 1, nil))
	_, _, err := manager.RelayHandler(context.Background(), attachment, "vault", http.MethodPost, "/vault/uid-a", nil)
	require.ErrorIs(t, err, ErrUnknownHandler)
	require.Empty(t, caller.snapshotRelay())
}

func TestHTTPCallerRelayDefaultsMethodToPost(t *testing.T) {
	var seenMethod, seenSandboxUID string
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenSandboxUID = r.Header.Get(actionapi.HeaderSandboxUID)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(handlerServer.Close)
	status, _, err := NewHTTPCaller().Relay(context.Background(),
		int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port), actionapi.APIVersion, actionapi.Owner{SandboxUID: "uid-a"}, "", "/vault/uid-a", []byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, http.MethodPost, seenMethod)
	require.Equal(t, "uid-a", seenSandboxUID)
}

func TestHTTPCallerRelayIsVerbatimOverRealHTTP(t *testing.T) {
	var seenMethod, seenURI, seenContentType, seenSandboxUID string
	var seenBody []byte
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenURI, seenContentType = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
		seenSandboxUID = r.Header.Get(actionapi.HeaderSandboxUID)
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Handler", "yes")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"exists"}`))
	}))
	t.Cleanup(handlerServer.Close)
	port := int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port)

	status, body, err := NewHTTPCaller().Relay(context.Background(), port, actionapi.APIVersion, actionapi.Owner{SandboxUID: "uid-a"}, http.MethodPut, "/vault/uid-a?x=1", []byte(`{"revision":1}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, []byte(`{"error":"exists"}`), body)
	require.Equal(t, http.MethodPut, seenMethod)
	require.Equal(t, "/vault/uid-a?x=1", seenURI)
	require.Equal(t, "application/json", seenContentType)
	require.Equal(t, "uid-a", seenSandboxUID)
	require.Equal(t, []byte(`{"revision":1}`), seenBody)
}

func TestHTTPCallerRelaySurfacesTransportFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := int32(listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())

	_, _, err = NewHTTPCaller().Relay(context.Background(), port, "", actionapi.Owner{SandboxUID: "s-a"}, http.MethodGet, "/vault/s-a", nil)
	require.Error(t, err, "an unreachable Handler is a relay failure, not a handler outcome")
}

func TestHTTPCallerRelayBoundsResponsesAndRedirects(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/vault/moved")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	status, body, err := NewHTTPCaller().Relay(context.Background(),
		int32(redirect.Listener.Addr().(*net.TCPAddr).Port), actionapi.APIVersion, actionapi.Owner{SandboxUID: "uid-a"}, http.MethodGet, "/vault/s-a", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, status, "redirects are surfaced verbatim, never followed")
	require.Empty(t, body)

	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxRelayResponseSize+1))
	}))
	t.Cleanup(oversized.Close)
	_, _, err = NewHTTPCaller().Relay(context.Background(),
		int32(oversized.Listener.Addr().(*net.TCPAddr).Port), actionapi.APIVersion, actionapi.Owner{SandboxUID: "uid-a"}, http.MethodGet, "/vault/s-a", nil)
	require.Error(t, err)
	require.ErrorContains(t, err, fmt.Sprintf("exceeds %d bytes", int64(maxRelayResponseSize)))
}

func TestHTTPCallerRelaySurfacesContextCancellation(t *testing.T) {
	release := make(chan struct{})
	held := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	t.Cleanup(held.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	var relayErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, relayErr = NewHTTPCaller().Relay(ctx, int32(held.Listener.Addr().(*net.TCPAddr).Port), actionapi.APIVersion, actionapi.Owner{SandboxUID: "uid-a"}, http.MethodGet, "/vault/s-a", nil)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not honor context cancellation")
	}
	require.ErrorIs(t, relayErr, context.Canceled)
}

// The handler-relay response bound is the relay's own (128 KiB): a policy
// record larger than the management 32 KiB bound round-trips verbatim,
// while the management channel stays at 32 KiB.
func TestHTTPCallerRelayReturnsResponsesOverManagementBound(t *testing.T) {
	large := make([]byte, 40<<10)
	for i := range large {
		large[i] = byte('a' + i%26)
	}
	document := []byte(`{"revision":2,"policy":` + string(large) + `}`)
	require.Greater(t, len(document), 32<<10, "the fixture must exceed the management bound")

	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	defer handlerServer.Close()
	port, err := func() (int32, error) {
		address := handlerServer.Listener.Addr().(*net.TCPAddr)
		return int32(address.Port), nil
	}()
	require.NoError(t, err)

	status, body, relayErr := NewHTTPCaller().Relay(context.Background(), port, actionapi.APIVersion, actionapi.Owner{SandboxUID: "uid-a"}, http.MethodGet, "/policy/uid-a", nil)
	require.NoError(t, relayErr)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, document, body, "the relay returns handler-owned documents verbatim")

	// The management channel keeps the 32 KiB bound: the same body through
	// Status is a transport-level refusal.
	caller := &HTTPCaller{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(document))}, nil
	})}}
	_, statusErr := caller.Status(context.Background(), port, actionapi.APIVersion)
	require.ErrorContains(t, statusErr, "exceeds")
}

func TestHTTPCallerActionsV2UsesDedicatedRoutesAndFullOwner(t *testing.T) {
	owner := actionapi.Owner{SandboxUID: "uid-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a", InstanceGeneration: 2,
		AssignmentAttempt: 3, RouteGeneration: 4, AttachmentID: "attachment-a"}
	var actionVersion, actionOwner, relayPath string
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_fastlet/v2/actions/status":
			_ = json.NewEncoder(w).Encode(actionapi.HandlerStatus{APIVersion: actionapi.APIVersionV2, Ready: true, InstanceID: "egress-1", OwnerStatus: true})
		case "/_fastlet/v2/actions":
			var request actionapi.Request
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			actionVersion, actionOwner = request.APIVersion, request.Owner.SandboxUID
			w.WriteHeader(http.StatusOK)
		case "/_fastlet/v2/relay/vault/uid-a":
			relayPath = r.URL.Path
			var got actionapi.Owner
			decoded, err := base64.RawURLEncoding.DecodeString(r.Header.Get(actionapi.HeaderOwner))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(decoded, &got))
			require.Equal(t, owner, got)
			require.Equal(t, owner.SandboxUID, r.Header.Get(actionapi.HeaderSandboxUID))
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer handlerServer.Close()
	port := int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port)
	caller := NewHTTPCaller()
	_, err := caller.Status(context.Background(), port, actionapi.APIVersionV2)
	require.NoError(t, err)
	_, err = caller.Status(context.Background(), port, actionapi.APIVersion)
	require.Error(t, err, "a v2-only receiver must not be retried through v1")

	request := actionapi.Request{APIVersion: actionapi.APIVersionV2, Operation: actionapi.OperationSetBinding,
		Sandbox: actionapi.SandboxIdentity{UID: owner.SandboxUID}, Owner: &owner,
		Revision: actionapi.Revision{SpecGeneration: 1, RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID, RouteGeneration: owner.RouteGeneration}}
	require.NoError(t, caller.Invoke(context.Background(), port, request))
	status, _, err := caller.Relay(context.Background(), port, actionapi.APIVersionV2, owner, http.MethodGet, "/vault/uid-a", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, actionapi.APIVersionV2, actionVersion)
	require.Equal(t, owner.SandboxUID, actionOwner)
	require.Equal(t, "/_fastlet/v2/relay/vault/uid-a", relayPath)
}
