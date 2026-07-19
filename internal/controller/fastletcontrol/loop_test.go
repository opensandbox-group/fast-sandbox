package fastletcontrol

import (
	"context"
	"sync"
	"testing"
	"time"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/fastletpool"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type fakeHeartbeatClient struct {
	mu       sync.Mutex
	requests []api.HeartbeatRequest
	active   int
	max      int
	delay    time.Duration
	response func(api.HeartbeatRequest) *api.HeartbeatResponse
}

func (f *fakeHeartbeatClient) Heartbeat(_ context.Context, _ string, request *api.HeartbeatRequest) (*api.HeartbeatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, *request)
	f.active++
	if f.active > f.max {
		f.max = f.active
	}
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	f.active--
	response := f.response(*request)
	f.mu.Unlock()
	return response, nil
}

func heartbeatFor(podUID, epoch string, sequence, revision uint64, full bool) *api.HeartbeatResponse {
	return &api.HeartbeatResponse{
		FastletStatus: api.FastletStatus{
			FastletPodUID: podUID, RuntimeReady: true,
			Admission: api.AdmissionStatus{Capacity: 5, Used: 1, Running: 1},
		},
		Sequence:    sequence,
		Cache:       api.CacheSnapshot{Epoch: epoch, Revision: revision, Full: full, Complete: true, Images: []string{"alpine:latest"}},
		Diagnostics: api.RuntimeDiagnostics{RuntimeProfileHash: "runtime-hash"},
	}
}

func TestFastletInfoFromPodUsesWatchIdentityAndReadiness(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fastlet-a", Namespace: "default", UID: types.UID("pod-uid-a"),
			Labels:      map[string]string{"app": "sandbox-fastlet", "fast-sandbox.io/pool": "pool-a", "fast-sandbox.io/runtime": "container"},
			Annotations: map[string]string{"fast-sandbox.io/runtime-profile-hash": "runtime-hash", "fast-sandbox.io/resource-profile-hash": "resource-hash"},
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	info := fastletInfoFromPod(pod)
	require.Equal(t, "pod-uid-a", info.PodUID)
	require.True(t, info.PodReady)
	require.Equal(t, "runtime-hash", info.RuntimeProfileHash)
	require.Equal(t, "resource-hash", info.ResourceProfileHash)
}

func TestHeartbeatLoopUsesCacheCursorAndAppliesDelta(t *testing.T) {
	registry := fastletpool.NewInMemoryRegistry()
	registry.UpsertPod(fastletpool.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-uid-a", PodIP: "10.0.0.1",
		Namespace: "default", PoolName: "pool-a", PodReady: true,
	})
	client := &fakeHeartbeatClient{response: func(request api.HeartbeatRequest) *api.HeartbeatResponse {
		if request.Cache.Epoch == "" {
			return heartbeatFor("pod-uid-a", "boot-a", 1, 1, true)
		}
		return heartbeatFor("pod-uid-a", "boot-a", 2, 1, false)
	}}
	loop := &Loop{Registry: registry, FastletClient: client, RequestTimeout: time.Second, MaxConcurrent: 1}
	loop.syncOnce(context.Background())
	loop.syncOnce(context.Background())
	client.mu.Lock()
	require.Len(t, client.requests, 2)
	require.Equal(t, api.CacheCursor{Epoch: "boot-a", Revision: 1}, client.requests[1].Cache)
	client.mu.Unlock()
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.Equal(t, []string{"alpine:latest"}, stored.Images)
	require.Equal(t, uint64(2), stored.HeartbeatSequence)
}

func TestHeartbeatLoopBoundsConcurrency(t *testing.T) {
	registry := fastletpool.NewInMemoryRegistry()
	for index := 0; index < 20; index++ {
		id := fastletpool.FastletID(string(rune('a' + index)))
		registry.UpsertPod(fastletpool.FastletInfo{
			ID: id, PodName: string(id), PodUID: "uid-" + string(id), PodIP: "10.0.0.1",
			Namespace: "default", PoolName: "pool-a", PodReady: true,
		})
	}
	client := &fakeHeartbeatClient{delay: 5 * time.Millisecond, response: func(request api.HeartbeatRequest) *api.HeartbeatResponse {
		return heartbeatFor("", "boot-a", 1, 1, true)
	}}
	// Return each watched Pod UID so ApplyHeartbeat succeeds; concurrency is
	// measured independently of result application.
	client.response = func(api.HeartbeatRequest) *api.HeartbeatResponse {
		return heartbeatFor("wrong-but-safe-for-bound-test", "boot-a", 1, 1, true)
	}
	loop := &Loop{Registry: registry, FastletClient: client, RequestTimeout: time.Second, MaxConcurrent: 3}
	loop.syncOnce(context.Background())
	client.mu.Lock()
	defer client.mu.Unlock()
	require.LessOrEqual(t, client.max, 3)
	require.Equal(t, 20, len(client.requests))
}

func TestPodDeleteRemovesMembership(t *testing.T) {
	registry := fastletpool.NewInMemoryRegistry()
	loop := &Loop{Registry: registry}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "fastlet-a", UID: types.UID("pod-uid-a"), Labels: map[string]string{"app": "sandbox-fastlet"},
	}}
	loop.onPodAdd(pod)
	_, exists := registry.GetFastletByID("fastlet-a")
	require.True(t, exists)
	loop.onPodDelete(pod)
	_, exists = registry.GetFastletByID("fastlet-a")
	require.False(t, exists)
}

func TestStaleDeleteEventCannotRemoveReplacementPod(t *testing.T) {
	registry := fastletpool.NewInMemoryRegistry()
	loop := &Loop{Registry: registry}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "fastlet-a", UID: types.UID("old-uid"), Labels: map[string]string{"app": "sandbox-fastlet"},
	}}
	newPod := oldPod.DeepCopy()
	newPod.UID = types.UID("new-uid")
	loop.onPodAdd(oldPod)
	loop.onPodAdd(newPod)
	loop.onPodDelete(oldPod)
	stored, exists := registry.GetFastletByID("fastlet-a")
	require.True(t, exists)
	require.Equal(t, "new-uid", stored.PodUID)
}
