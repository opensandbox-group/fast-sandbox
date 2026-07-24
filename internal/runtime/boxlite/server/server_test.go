package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"

	"github.com/stretchr/testify/require"
)

func TestServerLifecycleAndStrictContract(t *testing.T) {
	backend := &fakeBackend{boxes: make(map[string]boxliteprotocol.Box)}
	server := &Server{Backend: backend}

	request := boxliteprotocol.EnsureRequest{
		Namespace: "ns", TunnelGuestPort: 19090,
		Sandbox: fastletapi.SandboxSpec{SandboxID: "uid-a", ClaimNamespace: "ns", FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	response := doJSONHandler(t, server, http.MethodPut, "/v1/boxes/uid-a", request)
	require.Equal(t, http.StatusOK, response.Code)
	response = doJSONHandler(t, server, http.MethodGet, "/v1/boxes/uid-a", nil)
	require.Equal(t, http.StatusOK, response.Code)
	response = doJSONHandler(t, server, http.MethodPost, "/v1/boxes/uid-a", nil)
	require.Equal(t, http.StatusOK, response.Code)
	response = doJSONHandler(t, server, http.MethodGet, "/v1/boxes?namespace=ns", nil)
	require.Equal(t, http.StatusOK, response.Code)
	var list boxliteprotocol.ListResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &list))
	require.Len(t, list.Boxes, 1)
	response = doJSONHandler(t, server, http.MethodDelete, "/v1/boxes/uid-a", nil)
	require.Equal(t, http.StatusNoContent, response.Code)
	response = doJSONHandler(t, server, http.MethodGet, "/v1/boxes/uid-a", nil)
	require.Equal(t, http.StatusNotFound, response.Code)

	response = doRaw(t, server, http.MethodPut, "/v1/boxes/uid-b", `{"sandbox":{"sandboxId":"uid-b"},"unknown":true}`)
	require.Equal(t, http.StatusBadRequest, response.Code)
}

func TestServerCapabilitiesAndImages(t *testing.T) {
	backend := &fakeBackend{boxes: make(map[string]boxliteprotocol.Box), images: []string{"busybox:latest"}}
	server := &Server{Backend: backend}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var capabilities boxliteprotocol.Capabilities
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &capabilities))
	require.True(t, capabilities.Ready)
	require.Equal(t, boxliteprotocol.ProtocolVersionV1, capabilities.ProtocolVersion)

	response = doJSONHandler(t, server, http.MethodGet, "/v1/images", nil)
	require.Equal(t, http.StatusOK, response.Code)
	response = doJSONHandler(t, server, http.MethodPost, "/v1/images/pull", boxliteprotocol.PullRequest{Image: "alpine:latest"})
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "alpine:latest", backend.pulled)
}

type fakeBackend struct {
	boxes  map[string]boxliteprotocol.Box
	images []string
	pulled string
}

func (b *fakeBackend) Capabilities(context.Context) boxliteprotocol.Capabilities {
	capabilities := make(map[string]bool, len(boxliteprotocol.RequiredCapabilities))
	for _, capability := range boxliteprotocol.RequiredCapabilities {
		capabilities[capability] = true
	}
	return boxliteprotocol.Capabilities{ProtocolVersion: boxliteprotocol.ProtocolVersionV1, Ready: true, Capabilities: capabilities}
}

func (b *fakeBackend) Ensure(_ context.Context, request boxliteprotocol.EnsureRequest) (boxliteprotocol.Box, error) {
	box := boxliteprotocol.Box{Sandbox: request.Sandbox, BoxID: "box-" + request.Sandbox.SandboxID, Phase: "running", CreatedAt: 1}
	b.boxes[request.Sandbox.SandboxID] = box
	return box, nil
}

func (b *fakeBackend) Inspect(_ context.Context, id string) (boxliteprotocol.Box, error) {
	box, found := b.boxes[id]
	if !found {
		return boxliteprotocol.Box{}, &Error{Code: boxliteprotocol.ErrorNotFound, Message: "box not found"}
	}
	return box, nil
}

func (b *fakeBackend) Recover(ctx context.Context, id string) (boxliteprotocol.Box, error) {
	return b.Inspect(ctx, id)
}

func (b *fakeBackend) Delete(_ context.Context, id string) error {
	delete(b.boxes, id)
	return nil
}

func (b *fakeBackend) List(_ context.Context, namespace string) ([]boxliteprotocol.Box, error) {
	boxes := make([]boxliteprotocol.Box, 0, len(b.boxes))
	for _, box := range b.boxes {
		if namespace == "" || box.Sandbox.ClaimNamespace == namespace {
			boxes = append(boxes, box)
		}
	}
	return boxes, nil
}

func (b *fakeBackend) ListImages(context.Context) ([]string, error) { return b.images, nil }
func (b *fakeBackend) PullImage(_ context.Context, image string) error {
	if image == "bad" {
		return errors.New("pull failed")
	}
	b.pulled = image
	return nil
}

func doJSONHandler(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, &payload))
	return response
}

func doRaw(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
