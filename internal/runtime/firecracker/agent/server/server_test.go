package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"

	"github.com/stretchr/testify/require"
)

const (
	testImage = "registry.example.com/sandbox:v1.0.21"
	testPod   = "pod-1"
	testNS    = "tenant-a"
)

func imageKeyHex(image string) string {
	digest := sha256.Sum256([]byte(image))
	return hex.EncodeToString(digest[:])
}

// seedCache writes a committed pull (manifest + native artifacts with
// matching digests) into the cache, byte-identical to the pull layer's
// layout.
func seedCache(stateRoot, image string) error {
	dir := filepath.Join(stateRoot, "images", imageKeyHex(image))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	rootfs := []byte("rootfs-content-for-tests")
	vmstate := []byte("vmstate-content-for-tests")
	memory := []byte("memory-content-for-tests")
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), rootfs, 0o640); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "vmstate.snap"), vmstate, 0o640); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.snap"), memory, 0o640); err != nil {
		return err
	}
	manifest := map[string]any{
		"files": map[string]any{
			"rootfs.ext4":  map[string]any{"sha256": hexSHA256(rootfs), "sizeBytes": len(rootfs)},
			"vmstate.snap": map[string]any{"sha256": hexSHA256(vmstate), "sizeBytes": len(vmstate)},
			"memory.snap":  map[string]any{"sha256": hexSHA256(memory), "sizeBytes": len(memory)},
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), payload, 0o640)
}

func hexSHA256(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// fakePuller counts pulls per image and seeds the cache on demand.
type fakePuller struct {
	mu    sync.Mutex
	pulls map[string]int
	fail  map[string]error
	seed  func(stateRoot, image string) error
}

func newFakePuller() *fakePuller {
	return &fakePuller{pulls: make(map[string]int), fail: make(map[string]error), seed: seedCache}
}

func (f *fakePuller) PullImage(_ context.Context, stateRoot, image string) error {
	f.mu.Lock()
	f.pulls[image]++
	fail := f.fail[image]
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	return f.seed(stateRoot, image)
}

func (f *fakePuller) PullCheckpoint(_ context.Context, stateRoot, reference, _, _ string) error {
	f.mu.Lock()
	f.pulls[reference]++
	fail := f.fail[reference]
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	return f.seed(stateRoot, reference)
}

func (f *fakePuller) pullCount(image string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[image]
}

// newServerFixture wires the Service + state + Server over a real Unix socket.
type serverFixture struct {
	server     *Server
	client     *http.Client
	socketPath string
	stateRoot  string
	state      *agentstate.State
	puller     *fakePuller
}

// socketCounter keeps test socket names unique; Unix socket paths are
// bounded (104 bytes on macOS), so the file lives in the OS temp root with
// a short name instead of under the long t.TempDir() path.
var socketCounter int64

func testSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("fast-sandbox-agent-%d-%d.sock", os.Getpid(), atomic.AddInt64(&socketCounter, 1)))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func newServerFixture(t *testing.T) *serverFixture {
	t.Helper()
	stateRoot := t.TempDir()
	puller := newFakePuller()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	socketPath := testSocketPath(t)
	service := NewService(puller, state, stateRoot, WithServiceClock(func() time.Time { return time.Unix(1720000000, 0) }))
	server := New(service, socketPath)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = server.Serve(ctx) }()
	t.Cleanup(cancel)
	// Wait for the listener so dials never race socket creation.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime-agent socket %s never appeared", socketPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
			ForceAttemptHTTP2: false,
		},
	}
	fixture := &serverFixture{server: server, client: client, socketPath: socketPath, stateRoot: stateRoot, state: state, puller: puller}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return fixture
}

func (f *serverFixture) do(t *testing.T, path string, body any) (int, agentprotocol.ErrorResponse, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, "http://agent"+path, bytes.NewReader(payload))
	require.NoError(t, err)
	response, err := f.client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var wireError agentprotocol.ErrorResponse
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &wireError)
	}
	return response.StatusCode, wireError, raw
}

func identity(requestID string) agentprotocol.Identity {
	return agentprotocol.Identity{RequestID: requestID, Namespace: testNS, PodUID: testPod}
}

func TestServerPinImagePullsAndIsIdempotent(t *testing.T) {
	fixture := newServerFixture(t)

	status, wireError, raw := fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: identity("req-1"), Image: testImage,
	})
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, wireError.Code)
	var response agentprotocol.PinImageResponse
	require.NoError(t, json.Unmarshal(raw, &response))
	require.True(t, response.Ready)
	require.NotEmpty(t, response.ManifestDigest)
	require.Equal(t, 1, fixture.puller.pullCount(testImage))
	require.Equal(t, 1, fixture.state.Snapshot().PinCount)

	// The same request id replays the recorded outcome without re-pulling.
	status, wireError, raw = fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: identity("req-1"), Image: testImage,
	})
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, wireError.Code)
	var replayed agentprotocol.PinImageResponse
	require.NoError(t, json.Unmarshal(raw, &replayed))
	require.Equal(t, response, replayed)
	require.Equal(t, 1, fixture.puller.pullCount(testImage))

	// A second pin with a different request id pulls nothing (cache ready)
	// but records another reference.
	status, _, _ = fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: identity("req-2"), Image: testImage,
	})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, fixture.puller.pullCount(testImage))
	require.Equal(t, 2, fixture.state.Snapshot().PinCount)
}

func TestServerPinImageNotPublished(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.puller.fail[testImage] = fmt.Errorf("%w: %q", runtimecontract.ErrImageNotReady, testImage)

	status, wireError, _ := fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: identity("req-1"), Image: testImage,
	})
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, agentprotocol.ErrorNotFound, wireError.Code)
}

func TestServerIdentityEnforcement(t *testing.T) {
	fixture := newServerFixture(t)

	// Empty PodUID is rejected on every RPC.
	status, wireError, _ := fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: agentprotocol.Identity{RequestID: "req-1"}, Image: testImage,
	})
	require.Equal(t, http.StatusForbidden, status)
	require.Equal(t, agentprotocol.ErrorUnauthorized, wireError.Code)

	status, wireError, _ = fixture.do(t, agentprotocol.RouteHealth, agentprotocol.Identity{})
	require.Equal(t, http.StatusForbidden, status)
	require.Equal(t, agentprotocol.ErrorUnauthorized, wireError.Code)

	status, wireError, _ = fixture.do(t, agentprotocol.RouteCompatibility, agentprotocol.Identity{Namespace: testNS})
	require.Equal(t, http.StatusForbidden, status)
	require.Equal(t, agentprotocol.ErrorUnauthorized, wireError.Code)

	// A mutating RPC without its idempotency key is rejected.
	status, wireError, _ = fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: agentprotocol.Identity{PodUID: testPod}, Image: testImage,
	})
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, agentprotocol.ErrorInvalidRequest, wireError.Code)
}

func TestServerCrossPodConflict(t *testing.T) {
	fixture := newServerFixture(t)

	status, _, _ := fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: identity("req-1"), Image: testImage,
	})
	require.Equal(t, http.StatusOK, status)

	// Replaying pod-1's request id as pod-2 is a conflict.
	status, wireError, _ := fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: agentprotocol.Identity{RequestID: "req-1", Namespace: testNS, PodUID: "pod-2"},
		Image:    testImage,
	})
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, agentprotocol.ErrorConflict, wireError.Code)
}

func TestServerLeaseDevicesFlow(t *testing.T) {
	fixture := newServerFixture(t)

	status, wireError, raw := fixture.do(t, agentprotocol.RouteLeaseDevices, agentprotocol.LeaseDevicesRequest{
		Identity: identity("req-1"), SandboxID: "sandbox-1", Image: testImage, MemSizeMiB: 4096,
	})
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, wireError.Code)
	var response agentprotocol.LeaseDevicesResponse
	require.NoError(t, json.Unmarshal(raw, &response))
	require.NotEmpty(t, response.LeaseID)
	require.Equal(t, filepath.Join(fixture.stateRoot, "images", imageKeyHex(testImage), "rootfs.img"), response.RootfsDev)
	require.Equal(t, filepath.Join(fixture.stateRoot, "images", imageKeyHex(testImage), "memory.snap"), response.MemDev)
	require.Equal(t, 1, fixture.state.Snapshot().LeaseCount)

	// Business idempotency: a different request id for the same sandbox
	// returns the same lease.
	status, _, raw = fixture.do(t, agentprotocol.RouteLeaseDevices, agentprotocol.LeaseDevicesRequest{
		Identity: identity("req-2"), SandboxID: "sandbox-1", Image: testImage, MemSizeMiB: 4096,
	})
	require.Equal(t, http.StatusOK, status)
	var replayed agentprotocol.LeaseDevicesResponse
	require.NoError(t, json.Unmarshal(raw, &replayed))
	require.Equal(t, response.LeaseID, replayed.LeaseID)
	require.Equal(t, 1, fixture.state.Snapshot().LeaseCount)

	// Release by the owning pod.
	status, wireError, _ = fixture.do(t, agentprotocol.RouteReleaseDevices, agentprotocol.ReleaseDevicesRequest{
		Identity: identity("req-3"), LeaseID: response.LeaseID,
	})
	require.Equal(t, http.StatusNoContent, status)
	require.Empty(t, wireError.Code)
	require.Equal(t, 0, fixture.state.Snapshot().LeaseCount)

	// Cross-pod release of another sandbox's lease is a conflict.
	status, wireError, raw = fixture.do(t, agentprotocol.RouteLeaseDevices, agentprotocol.LeaseDevicesRequest{
		Identity: identity("req-4"), SandboxID: "sandbox-2", Image: testImage, MemSizeMiB: 4096,
	})
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, wireError.Code)
	var second agentprotocol.LeaseDevicesResponse
	require.NoError(t, json.Unmarshal(raw, &second))
	status, wireError, _ = fixture.do(t, agentprotocol.RouteReleaseDevices, agentprotocol.ReleaseDevicesRequest{
		Identity: agentprotocol.Identity{RequestID: "req-5", Namespace: testNS, PodUID: "pod-2"},
		LeaseID:  second.LeaseID,
	})
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, agentprotocol.ErrorConflict, wireError.Code)

	// Releasing an unknown lease is an idempotent no-op, not an error.
	status, wireError, _ = fixture.do(t, agentprotocol.RouteReleaseDevices, agentprotocol.ReleaseDevicesRequest{
		Identity: identity("req-6"), LeaseID: "does-not-exist",
	})
	require.Equal(t, http.StatusNoContent, status)
	require.Empty(t, wireError.Code)
}

func TestServerListLeasesAndHealth(t *testing.T) {
	fixture := newServerFixture(t)
	_, _, _ = fixture.do(t, agentprotocol.RouteLeaseDevices, agentprotocol.LeaseDevicesRequest{
		Identity: identity("req-1"), SandboxID: "sandbox-1", Image: testImage, MemSizeMiB: 4096,
	})

	status, _, raw := fixture.do(t, agentprotocol.RouteListLeases, identity(""))
	require.Equal(t, http.StatusOK, status)
	var list agentprotocol.ListLeasesResponse
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Leases, 1)
	require.Equal(t, "sandbox-1", list.Leases[0].SandboxID)
	require.Equal(t, testPod, list.Leases[0].PodUID)

	status, _, raw = fixture.do(t, agentprotocol.RouteHealth, identity(""))
	require.Equal(t, http.StatusOK, status)
	var health agentprotocol.HealthResponse
	require.NoError(t, json.Unmarshal(raw, &health))
	require.True(t, health.OK)
	require.Equal(t, 1, health.LeaseCount)
	require.Equal(t, 1, health.ImageCount)
	require.True(t, health.CacheBytes > 0)
}

func TestServerCompatibilityPlaceholder(t *testing.T) {
	fixture := newServerFixture(t)
	status, _, raw := fixture.do(t, agentprotocol.RouteCompatibility, identity(""))
	require.Equal(t, http.StatusOK, status)
	var response agentprotocol.CompatibilityResponse
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, compatibilityPlaceholder, response.CompatibilityClass)
}

func TestServerUnpinLifecycle(t *testing.T) {
	fixture := newServerFixture(t)
	_, _, _ = fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
		Identity: identity("req-1"), Image: testImage,
	})
	require.Equal(t, 1, fixture.state.Snapshot().PinCount)

	status, wireError, _ := fixture.do(t, agentprotocol.RouteUnpinImage, agentprotocol.UnpinImageRequest{
		Identity: identity("req-2"), Image: testImage,
	})
	require.Equal(t, http.StatusNoContent, status)
	require.Empty(t, wireError.Code)
	require.Equal(t, 0, fixture.state.Snapshot().PinCount)

	// Replaying the unpin request id stays a no-op (no negative count).
	status, _, _ = fixture.do(t, agentprotocol.RouteUnpinImage, agentprotocol.UnpinImageRequest{
		Identity: identity("req-2"), Image: testImage,
	})
	require.Equal(t, http.StatusNoContent, status)
	require.Equal(t, 0, fixture.state.Snapshot().PinCount)
}

func TestServerUnknownRouteAndMethod(t *testing.T) {
	fixture := newServerFixture(t)

	response, err := fixture.client.Post("http://agent"+agentprotocol.RouteHealth, "application/json", nil)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode) // missing identity

	request, err := http.NewRequest(http.MethodGet, "http://agent"+agentprotocol.RouteHealth, nil)
	require.NoError(t, err)
	response, err = fixture.client.Do(request)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, response.StatusCode)

	request, err = http.NewRequest(http.MethodPost, "http://agent/unknown", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	response, err = fixture.client.Do(request)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusNotFound, response.StatusCode)
}

func TestServerRejectsUnknownFields(t *testing.T) {
	fixture := newServerFixture(t)
	status, wireError, _ := fixture.doRaw(t, agentprotocol.RoutePinImage, `{
		"requestId":"req-1","podUid":"pod-1","namespace":"ns",
		"image":"img","unexpected":true
	}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, agentprotocol.ErrorInvalidRequest, wireError.Code)
}

func (f *serverFixture) doRaw(t *testing.T, path, body string) (int, agentprotocol.ErrorResponse, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://agent"+path, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	response, err := f.client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var wireError agentprotocol.ErrorResponse
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &wireError)
	}
	return response.StatusCode, wireError, raw
}

func TestServerConcurrentSameRequestID(t *testing.T) {
	fixture := newServerFixture(t)

	var wg sync.WaitGroup
	results := make([]int, 6)
	for index := 0; index < len(results); index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], _, _ = fixture.do(t, agentprotocol.RoutePinImage, agentprotocol.PinImageRequest{
				Identity: identity("req-1"), Image: testImage,
			})
		}(index)
	}
	wg.Wait()
	for _, status := range results {
		require.Equal(t, http.StatusOK, status)
	}
	require.Equal(t, 1, fixture.puller.pullCount(testImage))
	require.Equal(t, 1, fixture.state.Snapshot().PinCount)
}
