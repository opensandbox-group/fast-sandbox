package fastletpool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	fastletruntime "fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/testutil"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var registryNow = time.Unix(1000, 0)

func readyFastlet(id string, used, capacity int, images ...string) FastletInfo {
	return FastletInfo{
		ID: FastletID(id), Namespace: "default", PodName: id, PodUID: "uid-" + id,
		PodIP: "10.0.0.1", NodeName: "node-a", PoolName: "pool-a",
		RuntimeName: apiv1alpha1.RuntimeContainer, RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		PodReady: true, RuntimeReady: true, Capacity: capacity, Allocated: used,
		Admission: api.AdmissionStatus{Capacity: capacity, Used: used, Running: used},
		Images:    append([]string(nil), images...), CacheEpoch: "boot-a", CacheRevision: 1, CacheComplete: true,
		SandboxStatuses: make(map[string]api.SandboxStatus), HeartbeatSequence: 1, LastHeartbeat: registryNow, PodObservedAt: registryNow,
	}
}

func candidate(image, stableKey string) CandidateRequest {
	return CandidateRequest{
		Namespace: "default", PoolName: "pool-a", RuntimeName: apiv1alpha1.RuntimeContainer,
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		Image: image, StableKey: stableKey, Now: registryNow,
	}
}

func TestTopKPrefersImageThenLoadAndDoesNotAllocate(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.RegisterOrUpdate(readyFastlet("cache-hit", 4, 5, "alpine:latest"))
	registry.RegisterOrUpdate(readyFastlet("cache-miss", 0, 5, "ubuntu:24.04"))

	selected := registry.TopK(candidate("docker.io/library/alpine:latest", "request-a"), 2)
	require.Len(t, selected, 2)
	require.Equal(t, FastletID("cache-hit"), selected[0].ID, "cache hit precedes a less-loaded miss")
	stored, ok := registry.GetFastletByID("cache-hit")
	require.True(t, ok)
	require.Equal(t, 4, stored.Used(), "selection must not consume Registry capacity")
}

func TestTopKAllowsDuplicateSandboxPorts(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.clock = func() time.Time { return registryNow }
	registry.RegisterOrUpdate(readyFastlet("fastlet-a", 0, 5))
	first := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec:       apiv1alpha1.SandboxSpec{PoolRef: "pool-a", Image: "alpine:latest", ExposedPorts: []int32{8080}},
	}
	second := first.DeepCopy()
	second.Name, second.UID = "sandbox-b", types.UID("uid-b")
	firstChoice, err := registry.Allocate(first)
	require.NoError(t, err)
	secondChoice, err := registry.Allocate(second)
	require.NoError(t, err)
	require.Equal(t, firstChoice.ID, secondChoice.ID)
	stored, _ := registry.GetFastletByID(firstChoice.ID)
	require.Equal(t, 0, stored.Used())
}

func TestTopKHardFiltersStaleDrainingProfilesAndCapacity(t *testing.T) {
	registry := NewInMemoryRegistry()
	stale := readyFastlet("stale", 0, 5)
	stale.LastHeartbeat = registryNow.Add(-time.Minute)
	draining := readyFastlet("draining", 0, 5)
	draining.Draining = true
	wrongProfile := readyFastlet("wrong-profile", 0, 5)
	wrongProfile.RuntimeProfileHash = "other"
	full := readyFastlet("full", 5, 5)
	ready := readyFastlet("ready", 1, 5)
	for _, info := range []FastletInfo{stale, draining, wrongProfile, full, ready} {
		registry.RegisterOrUpdate(info)
	}
	selected := registry.TopK(candidate("alpine:latest", "request-a"), 10)
	require.Len(t, selected, 1)
	require.Equal(t, FastletID("ready"), selected[0].ID)
}

func TestStablePerturbationIsDeterministic(t *testing.T) {
	registry := NewInMemoryRegistry()
	for _, id := range []string{"fastlet-a", "fastlet-b", "fastlet-c"} {
		registry.RegisterOrUpdate(readyFastlet(id, 0, 5))
	}
	first := registry.TopK(candidate("alpine:latest", "request-a"), 3)
	second := registry.TopK(candidate("alpine:latest", "request-a"), 3)
	require.Equal(t, first, second)
	other := registry.TopK(candidate("alpine:latest", "request-b"), 3)
	require.NotEqual(t, []FastletID{first[0].ID, first[1].ID, first[2].ID}, []FastletID{other[0].ID, other[1].ID, other[2].ID})
}

func TestPodReplacementClearsOldHeartbeatState(t *testing.T) {
	registry := NewInMemoryRegistry()
	old := readyFastlet("fastlet-a", 1, 5, "alpine:latest")
	registry.RegisterOrUpdate(old)
	registry.UpsertPod(FastletInfo{
		ID: old.ID, Namespace: old.Namespace, PodName: old.PodName, PodUID: "replacement-uid",
		PodIP: "10.0.0.2", PoolName: old.PoolName, PodReady: true, PodObservedAt: registryNow,
	})
	replaced, _ := registry.GetFastletByID(old.ID)
	require.Equal(t, "replacement-uid", replaced.PodUID)
	require.False(t, replaced.RuntimeReady)
	require.Zero(t, replaced.Capacity)
	require.Empty(t, replaced.Images)
	require.True(t, replaced.LastHeartbeat.IsZero())
}

func TestSamePodWatchUpdatePreservesHeartbeatButRefreshesExpectedProfiles(t *testing.T) {
	registry := NewInMemoryRegistry()
	existing := readyFastlet("fastlet-a", 1, 5, "alpine:latest")
	registry.RegisterOrUpdate(existing)

	registry.UpsertPod(FastletInfo{
		ID: existing.ID, Namespace: existing.Namespace, PodName: existing.PodName, PodUID: existing.PodUID,
		PodIP: existing.PodIP, PoolName: existing.PoolName, PodReady: true, PodObservedAt: registryNow.Add(time.Second),
		RuntimeProfileHash: "new-runtime-hash", ResourceProfileHash: "new-resource-hash",
	})

	updated, _ := registry.GetFastletByID(existing.ID)
	require.True(t, updated.RuntimeReady)
	require.Equal(t, existing.HeartbeatSequence, updated.HeartbeatSequence)
	require.Equal(t, existing.Images, updated.Images)
	require.Equal(t, "new-runtime-hash", updated.RuntimeProfileHash)
	require.Equal(t, "new-resource-hash", updated.ResourceProfileHash)
}

func TestApplyHeartbeatFencesPodUIDSequenceAndCacheRevision(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.UpsertPod(FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-uid-a", PodIP: "10.0.0.1",
		Namespace: "default", PoolName: "pool-a", PodReady: true,
	})
	heartbeat := &api.HeartbeatResponse{
		FastletStatus: api.FastletStatus{
			FastletPodUID: "pod-uid-a", RuntimeReady: true, ResourceProfileHash: "resource-hash",
			Admission: api.AdmissionStatus{Capacity: 5, Used: 2, Running: 2},
		},
		Sequence: 1, Cache: api.CacheSnapshot{Epoch: "boot-a", Revision: 1, Full: true, Complete: true, Images: []string{"alpine:latest"}},
		Diagnostics: api.RuntimeDiagnostics{RuntimeProfileHash: "runtime-hash"},
	}
	require.NoError(t, registry.ApplyHeartbeat("fastlet-a", "pod-uid-a", heartbeat, registryNow))
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.True(t, stored.RuntimeReady)
	require.Equal(t, 2, stored.Used())
	require.Equal(t, []string{"alpine:latest"}, stored.Images)

	unchanged := *heartbeat
	unchanged.Sequence = 2
	unchanged.Cache = api.CacheSnapshot{Epoch: "boot-a", Revision: 1, Full: false, Complete: true}
	require.NoError(t, registry.ApplyHeartbeat("fastlet-a", "pod-uid-a", &unchanged, registryNow.Add(time.Second)))
	stored, _ = registry.GetFastletByID("fastlet-a")
	require.Equal(t, []string{"alpine:latest"}, stored.Images)

	gap := unchanged
	gap.Sequence = 3
	gap.Cache.Revision = 2
	require.NoError(t, registry.ApplyHeartbeat("fastlet-a", "pod-uid-a", &gap, registryNow.Add(2*time.Second)))
	stored, _ = registry.GetFastletByID("fastlet-a")
	require.False(t, stored.CacheComplete)
	require.Empty(t, stored.Images)

	require.ErrorIs(t, registry.ApplyHeartbeat("fastlet-a", "wrong-pod", heartbeat, registryNow), ErrStalePodIdentity)
	require.ErrorIs(t, registry.ApplyHeartbeat("fastlet-a", "pod-uid-a", heartbeat, registryNow), ErrStaleHeartbeat)
}

func TestApplyHeartbeatFailsClosedOnProfileMismatch(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.UpsertPod(FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-uid-a", PodIP: "10.0.0.1",
		Namespace: "default", PoolName: "pool-a", PodReady: true,
		RuntimeProfileHash: "expected-runtime", ResourceProfileHash: "expected-resource",
	})
	heartbeat := heartbeatWithProfilesForRegistry("other-runtime", "expected-resource")
	require.ErrorIs(t, registry.ApplyHeartbeat("fastlet-a", "pod-uid-a", heartbeat, registryNow), ErrProfileMismatch)
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.False(t, stored.RuntimeReady)
}

func heartbeatWithProfilesForRegistry(runtimeHash, resourceHash string) *api.HeartbeatResponse {
	return &api.HeartbeatResponse{
		FastletStatus: api.FastletStatus{
			FastletPodUID: "pod-uid-a", RuntimeReady: true, ResourceProfileHash: resourceHash,
			Admission: api.AdmissionStatus{Capacity: 5},
		},
		Sequence: 1, Cache: api.CacheSnapshot{Epoch: "boot-a", Revision: 1, Full: true, Complete: true},
		Diagnostics: api.RuntimeDiagnostics{RuntimeProfileHash: runtimeHash},
	}
}

func TestLocalCapacityFeedbackSuppressesCandidateUntilHeartbeat(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.RegisterOrUpdate(readyFastlet("fastlet-a", 0, 5))
	registry.RecordFeedback("fastlet-a", LocalFeedback{
		Code: api.ErrorCapacityRejected, ObservedAt: registryNow, RetryAfter: 10 * time.Second,
	})
	require.Empty(t, registry.TopK(candidate("alpine:latest", "request-a"), 1))
	request := candidate("alpine:latest", "request-a")
	request.Now = registryNow.Add(11 * time.Second)
	require.Len(t, registry.TopK(request, 1), 1)
}

func TestStaleCleanupNeverDeletesPodWatchMembership(t *testing.T) {
	registry := NewInMemoryRegistry()
	stale := readyFastlet("fastlet-a", 0, 5)
	stale.LastHeartbeat = registryNow.Add(-time.Minute)
	registry.RegisterOrUpdate(stale)
	registry.clock = func() time.Time { return registryNow }
	require.Equal(t, 1, registry.CleanupStaleFastlets(30*time.Second))
	_, exists := registry.GetFastletByID("fastlet-a")
	require.True(t, exists)
}

func TestRegistryViewsConvergeWithoutSharedAllocationState(t *testing.T) {
	left, right := NewInMemoryRegistry(), NewInMemoryRegistry()
	left.clock = func() time.Time { return registryNow }
	right.clock = func() time.Time { return registryNow }
	for _, registry := range []*InMemoryRegistry{left, right} {
		registry.RegisterOrUpdate(readyFastlet("fastlet-a", 1, 5, "alpine:latest"))
		registry.RegisterOrUpdate(readyFastlet("fastlet-b", 0, 5, "ubuntu:24.04"))
	}
	require.Equal(t, left.TopK(candidate("alpine:latest", "request-a"), 2), right.TopK(candidate("alpine:latest", "request-a"), 2))
	_, err := left.Allocate(&apiv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default"}, Spec: apiv1alpha1.SandboxSpec{PoolRef: "pool-a"}})
	require.NoError(t, err)
	for _, id := range []FastletID{"fastlet-a", "fastlet-b"} {
		leftInfo, _ := left.GetFastletByID(id)
		rightInfo, _ := right.GetFastletByID(id)
		require.Equal(t, leftInfo, rightInfo, "selection does not create divergent slot ownership")
	}
}

func TestGetReturnsDeepCopy(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.RegisterOrUpdate(readyFastlet("fastlet-a", 0, 5, "alpine:latest"))
	copy, _ := registry.GetFastletByID("fastlet-a")
	copy.Images[0] = "mutated"
	copy.SandboxStatuses["x"] = api.SandboxStatus{SandboxID: "x"}
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.Equal(t, []string{"alpine:latest"}, stored.Images)
	require.Empty(t, stored.SandboxStatuses)
}

func TestTopKReturnsDeepCopy(t *testing.T) {
	registry := NewInMemoryRegistry()
	info := readyFastlet("fastlet-a", 0, 5, "alpine:latest")
	info.PreparedArtifacts = []string{"infra-profile-a"}
	info.SandboxStatuses["sandbox-a"] = api.SandboxStatus{
		SandboxID: "sandbox-a",
		InfraDiagnostics: []api.InfraComponentDiagnostic{{
			Component: "execd",
			State:     "ready",
		}},
	}
	registry.RegisterOrUpdate(info)

	selected := registry.TopK(candidate("alpine:latest", "request-a"), 1)
	require.Len(t, selected, 1)
	selected[0].Images[0] = "mutated"
	selected[0].PreparedArtifacts[0] = "mutated"
	selectedStatus := selected[0].SandboxStatuses["sandbox-a"]
	selectedStatus.SandboxID = "mutated"
	selectedStatus.InfraDiagnostics[0].State = "mutated"
	selected[0].SandboxStatuses["sandbox-a"] = selectedStatus

	stored, ok := registry.GetFastletByID("fastlet-a")
	require.True(t, ok)
	require.Equal(t, []string{"alpine:latest"}, stored.Images)
	require.Equal(t, []string{"infra-profile-a"}, stored.PreparedArtifacts)
	require.Equal(t, "sandbox-a", stored.SandboxStatuses["sandbox-a"].SandboxID)
	require.Equal(t, "ready", stored.SandboxStatuses["sandbox-a"].InfraDiagnostics[0].State)
}

func TestTopKIsSafeDuringConcurrentRegistryUpdates(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.RegisterOrUpdate(readyFastlet("fastlet-a", 0, 5, "alpine:latest"))

	start := make(chan struct{})
	var updates sync.WaitGroup
	updates.Add(1)
	go func() {
		defer updates.Done()
		<-start
		for index := 0; index < 200; index++ {
			image := "alpine:latest"
			if index%2 == 0 {
				image = "ubuntu:24.04"
			}
			registry.RegisterOrUpdate(readyFastlet("fastlet-a", index%4, 5, image))
		}
	}()

	close(start)
	for index := 0; index < 200; index++ {
		selected := registry.TopK(candidate("alpine:latest", "request-a"), 1)
		require.Len(t, selected, 1)
		selected[0].Images[0] = "caller-mutated"
	}
	updates.Wait()
	stored, ok := registry.GetFastletByID("fastlet-a")
	require.True(t, ok)
	require.NotEqual(t, "caller-mutated", stored.Images[0])
}

func TestStaleRegistryHintsCannotExceedFastletCapacity(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.clock = func() time.Time { return registryNow }
	// Deliberately stale/optimistic local view: it never observes any of the
	// successful admissions below and continues returning the same candidate.
	registry.RegisterOrUpdate(readyFastlet("fastlet-a", 0, 100))
	manager, err := fastletruntime.NewSandboxManagerWithConfig(&testutil.FakeRuntime{}, fastletruntime.SandboxManagerConfig{
		Capacity: 5, FastletPodUID: "uid-fastlet-a",
	})
	require.NoError(t, err)
	successes := 0
	for index := 0; index < 100; index++ {
		sandboxUID := fmt.Sprintf("sandbox-%03d", index)
		_, err := registry.Allocate(&apiv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: sandboxUID, Namespace: "default", UID: types.UID(sandboxUID)},
			Spec:       apiv1alpha1.SandboxSpec{PoolRef: "pool-a", Image: "alpine:latest"},
		})
		require.NoError(t, err)
		_, err = manager.EnsureSandboxV2(context.Background(), &api.EnsureSandboxRequest{
			Identity: api.SandboxIdentity{SandboxUID: sandboxUID, InstanceGeneration: 1, AssignmentAttempt: 1, FastletPodUID: "uid-fastlet-a"},
			Sandbox:  api.SandboxSpec{ClaimUID: "claim-" + sandboxUID, Image: "alpine:latest"},
		})
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 5, successes)
	admission, _, _ := manager.State()
	require.Equal(t, 5, admission.Used)
}
