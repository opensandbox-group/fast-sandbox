package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletaction "fast-sandbox/internal/fastlet/action"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func relayIdentity(uid string) fastletapi.SandboxIdentity {
	return fastletapi.SandboxIdentity{SandboxUID: uid, Namespace: "default", Name: uid, InstanceGeneration: 1,
		RuntimeInstanceID: "runtime-" + uid, AssignmentAttempt: 1, RouteGeneration: 1, FastletPodUID: "pod-a"}
}

func newRelayManager(t *testing.T, handlerPort int32) *SandboxManager {
	t.Helper()
	handlers := []apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: handlerPort}}
	actionManager, err := fastletaction.NewManager(handlers, nil)
	require.NoError(t, err)
	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	return manager
}

func TestInvokeHandlerRelaysOverHandlerLoopback(t *testing.T) {
	var seenMethod, seenURI, seenContentType, seenSandboxUID string
	var seenBody []byte
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenURI, seenContentType = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
		seenSandboxUID = r.Header.Get(actionapi.HeaderSandboxUID)
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"exists"}`))
	}))
	t.Cleanup(handlerServer.Close)
	manager := newRelayManager(t, int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port))
	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: relayIdentity("uid-a")}, Phase: sandboxStateRunning, AppliedGeneration: 1}

	response, err := manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Method: http.MethodPut, Path: "/vault/uid-a", Payload: []byte(`{"revision":7}`),
	})
	require.NoError(t, err)
	require.Nil(t, response.Error)
	require.Equal(t, int32(http.StatusConflict), response.Status, "the handler's error status is the verbatim result")
	require.Equal(t, []byte(`{"error":"exists"}`), response.Body)
	require.Equal(t, http.MethodPut, seenMethod)
	require.Equal(t, "/vault/uid-a", seenURI)
	require.Equal(t, "application/json", seenContentType)
	require.Equal(t, []byte(`{"revision":7}`), seenBody)
	// The trusted identity header is the fence-validated Sandbox UID — the
	// Handler scopes per-subject state by it, not by the caller-controlled
	// path.
	require.Equal(t, "uid-a", seenSandboxUID)
}

func TestInvokeHandlerUnreachableHandlerSurfacesError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	manager := newRelayManager(t, int32(listener.Addr().(*net.TCPAddr).Port))
	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: relayIdentity("uid-a")}, Phase: sandboxStateRunning, AppliedGeneration: 1}

	response, relayErr := manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Path: "/vault/uid-a", Payload: []byte(`{}`),
	})
	require.Zero(t, response.Status)
	require.NotNil(t, response.Error)
	require.Equal(t, fastletapi.ErrorActionUnavailable, response.Error.Code)
	require.Error(t, relayErr)
}

func TestInvokeHandlerCarriesValidatedFullOwnerToActionsV2Relay(t *testing.T) {
	var seenURI string
	var seenOwner actionapi.Owner
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURI = r.URL.RequestURI()
		decoded, err := base64.RawURLEncoding.DecodeString(r.Header.Get(actionapi.HeaderOwner))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(decoded, &seenOwner))
		require.Equal(t, "uid-a", r.Header.Get(actionapi.HeaderSandboxUID))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(handlerServer.Close)
	port := int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port)
	strictHandler := apiv1alpha2.ActionHandler{Name: "egress", TargetHTTPPort: port}
	actionManager, err := fastletaction.NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{strictHandler}, actionapi.APIVersionV2, nil)
	require.NoError(t, err)
	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-a", ActionManager: actionManager,
	})
	require.NoError(t, err)
	identity := relayIdentity("uid-a")
	metadata := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{
		Identity: identity, ActionsAPIVersion: actionapi.APIVersionV2,
	}, Phase: sandboxStateRunning, AppliedGeneration: 1}
	manager.sandboxes["uid-a"] = metadata
	attachment := actionAttachment(metadata)
	require.NoError(t, actionManager.RegisterDesired(attachment, 1, nil))

	response, err := manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: identity, Handler: "egress", Method: http.MethodPut, Path: "/vault/uid-a", Payload: []byte(`{"revision":1}`),
	})
	require.NoError(t, err)
	require.Equal(t, int32(http.StatusOK), response.Status)
	require.Equal(t, "/_fastlet/v2/relay/vault/uid-a", seenURI)
	require.Equal(t, actionapi.Owner{
		SandboxUID: identity.SandboxUID, FastletPodUID: identity.FastletPodUID,
		RuntimeInstanceID: identity.RuntimeInstanceID, InstanceGeneration: identity.InstanceGeneration,
		AssignmentAttempt: identity.AssignmentAttempt, RouteGeneration: identity.RouteGeneration, AttachmentID: actionAttachmentID(metadata),
	}, seenOwner)
}

func TestInvokeHandlerFencesAndValidation(t *testing.T) {
	manager := newRelayManager(t, 18080)

	// Shape validation happens before any identity or state lookup.
	missing, relayErr := manager.InvokeHandler(context.Background(), nil)
	require.Equal(t, fastletapi.ErrorConflict, missing.Error.Code)
	require.Error(t, relayErr)
	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress",
	})
	require.ErrorContains(t, relayErr, "relay path is required")
	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Method: "TRACE", Path: "/vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "relay method")
	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Path: "vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "must start with '/'")

	// Identity target and fence validation.
	badPod := relayIdentity("uid-a")
	badPod.FastletPodUID = "pod-b"
	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: badPod, Handler: "egress", Path: "/vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "different Fastlet Pod UID")

	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Path: "/vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "not managed by this Fastlet")

	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: relayIdentity("uid-a")}, Phase: sandboxStateRunning, AppliedGeneration: 1}
	stale := relayIdentity("uid-a")
	stale.RuntimeInstanceID = "runtime-replaced"
	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: stale, Handler: "egress", Path: "/vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "runtimeInstanceId conflicts")

	// Unknown handler and missing Action configuration.
	_, relayErr = manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "vault", Path: "/vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "is not declared")

	bare, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-a"})
	require.NoError(t, err)
	bare.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: relayIdentity("uid-a")}, Phase: sandboxStateRunning, AppliedGeneration: 1}
	_, relayErr = bare.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Path: "/vault/uid-a",
	})
	require.ErrorContains(t, relayErr, "no Sandbox Action configuration")
}

func TestInvokeHandlerRejectsNonReadyRuntime(t *testing.T) {
	manager := newRelayManager(t, 18080)
	manager.sandboxes["uid-a"] = &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: relayIdentity("uid-a")}, Phase: sandboxStateCreating, AppliedGeneration: 1}

	response, relayErr := manager.InvokeHandler(context.Background(), &fastletapi.InvokeHandlerRequest{
		Identity: relayIdentity("uid-a"), Handler: "egress", Path: "/vault/uid-a",
	})
	require.Equal(t, fastletapi.ErrorInProgress, response.Error.Code)
	require.Error(t, relayErr)
}
