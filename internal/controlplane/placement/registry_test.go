package placement

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletsandbox "fast-sandbox/internal/fastlet/sandbox"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/testutil"
)

var registryNow = time.Unix(1000, 0)

func readyFastlet(id string, used, capacity int, images ...string) FastletInfo {
	return FastletInfo{
		ID: FastletID(id), Namespace: "default", PodName: id, PodUID: "uid-" + id,
		PodIP: "10.0.0.1", NodeName: "node-a", PoolName: "pool-a",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		PodReady: true, RuntimeReady: true, InfraReady: true,
		Admission: fastletapi.AdmissionStatus{Capacity: capacity, Used: used, Running: used},
		Images:    append([]string(nil), images...), CacheEpoch: "boot-a", CacheRevision: 1, CacheComplete: true,
		SandboxStatuses: make(map[string]fastletapi.SandboxStatus), HeartbeatSequence: 1, LastHeartbeat: registryNow, PodObservedAt: registryNow,
	}
}

func seedFastlet(tb testing.TB, registry *InMemoryRegistry, info FastletInfo) {
	tb.Helper()
	pod := FastletInfo{
		ID: info.ID, Namespace: info.Namespace, PodName: info.PodName, PodUID: info.PodUID,
		PodIP: info.PodIP, NodeName: info.NodeName, PoolName: info.PoolName,
		RuntimeName: info.RuntimeName, RuntimeProfileHash: info.RuntimeProfileHash,
		ResourceProfileHash: info.ResourceProfileHash,
		InfraRevision:       info.InfraRevision, PodReady: info.PodReady, PodObservedAt: info.PodObservedAt,
		ResourceCapacity: info.ResourceCapacity,
	}
	registry.UpsertPod(pod)
	sequence := info.HeartbeatSequence
	if sequence == 0 {
		sequence = 1
	}
	if previous, exists := registry.GetFastletByID(info.ID); exists && previous.PodUID == info.PodUID && sequence <= previous.HeartbeatSequence {
		sequence = previous.HeartbeatSequence + 1
	}
	statuses := make([]fastletapi.SandboxStatus, 0, len(info.SandboxStatuses))
	for _, status := range info.SandboxStatuses {
		statuses = append(statuses, status)
	}
	epoch := info.CacheEpoch
	if epoch == "" {
		epoch = "boot-a"
	}
	heartbeat := &fastletapi.HeartbeatResponse{
		FastletStatus: fastletapi.FastletStatus{
			FastletPodUID: info.PodUID, RuntimeReady: info.RuntimeReady, Draining: info.Draining,
			RuntimeProfileHash: info.RuntimeProfileHash, Admission: info.Admission, ResourceProfileHash: info.ResourceProfileHash,
			InfraRevision: info.InfraRevision, InfraReady: info.InfraReady,
			PreparedArtifacts: info.PreparedArtifacts, SandboxStatuses: statuses,
		},
		Sequence: sequence,
		Cache: fastletapi.CacheSnapshot{
			Epoch: epoch, Revision: info.CacheRevision, Full: true, Complete: info.CacheComplete, Images: info.Images,
		},
	}
	observedAt := info.LastHeartbeat
	if observedAt.IsZero() {
		observedAt = registryNow
	}
	require.NoError(tb, registry.ApplyHeartbeat(info.ID, info.PodUID, heartbeat, observedAt))
}

func candidate(image, stableKey string) CandidateRequest {
	return CandidateRequest{
		Namespace: "default", PoolName: "pool-a", RuntimeName: apiv1alpha2.RuntimeContainer,
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		Image: image, StableKey: stableKey, Now: registryNow,
	}
}

func resourceAwareFastlet(id string, used int, state fastletapi.AdmissionState, revision uint64, images ...string) FastletInfo {
	info := readyFastlet(id, used, 10, images...)
	info.ResourceCapacity = ResourceDemand{CPU: resource.MustParse("4"), Memory: resource.MustParse("8Gi")}
	info.Admission.Resources = &fastletapi.ResourceAdmissionStatus{
		Version: "v1", Epoch: "resource-epoch-a", Revision: revision, State: state,
	}
	return info
}

func resourceAwareCandidate(image string) CandidateRequest {
	request := candidate(image, "resource-request-a")
	request.ResourceMode = apiv1alpha2.SandboxResourceModeResourceAware
	request.Demand = ResourceDemand{CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi")}
	return request
}

func TestTopKPrefersImageThenLoadAndDoesNotAllocate(t *testing.T) {
	registry := NewInMemoryRegistry()
	seedFastlet(t, registry, readyFastlet("cache-hit", 4, 5, "alpine:latest"))
	seedFastlet(t, registry, readyFastlet("cache-miss", 0, 5, "ubuntu:24.04"))

	selected := registry.TopK(candidate("docker.io/library/alpine:latest", "request-a"), 2)
	require.Len(t, selected, 2)
	require.Equal(t, FastletID("cache-hit"), selected[0].ID, "cache hit precedes a less-loaded miss")
	stored, ok := registry.GetFastletByID("cache-hit")
	require.True(t, ok)
	require.Equal(t, 4, stored.Used(), "selection must not consume Registry capacity")
}

func TestTopKDoesNotMutateAdmission(t *testing.T) {
	registry := NewInMemoryRegistry()
	registry.clock = func() time.Time { return registryNow }
	seedFastlet(t, registry, readyFastlet("fastlet-a", 0, 5))
	firstChoice := registry.TopK(candidate("alpine:latest", "request-a"), 1)
	secondChoice := registry.TopK(candidate("alpine:latest", "request-b"), 1)
	require.Len(t, firstChoice, 1)
	require.Len(t, secondChoice, 1)
	require.Equal(t, firstChoice[0].ID, secondChoice[0].ID)
	stored, _ := registry.GetFastletByID(firstChoice[0].ID)
	require.Equal(t, 0, stored.Used())
}

func TestTopKExcludesPreviouslyTriedFastletsBeforeRanking(t *testing.T) {
	registry := NewInMemoryRegistry()
	seedFastlet(t, registry, readyFastlet("tried-hit", 0, 5, "alpine:latest"))
	seedFastlet(t, registry, readyFastlet("spare-miss", 0, 5))
	request := candidate("alpine:latest", "request-a")
	request.ExcludedFastlets = map[FastletID]struct{}{"tried-hit": {}}
	selected := registry.TopK(request, 1)
	require.Len(t, selected, 1)
	require.Equal(t, FastletID("spare-miss"), selected[0].ID)
	stored, _ := registry.GetFastletByID("tried-hit")
	require.Zero(t, stored.Used())
	require.Len(t, registry.TopK(candidate("alpine:latest", "request-b"), 2), 2, "exclusion is request-local")
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
		seedFastlet(t, registry, info)
	}
	selected := registry.TopK(candidate("alpine:latest", "request-a"), 10)
	require.Len(t, selected, 1)
	require.Equal(t, FastletID("ready"), selected[0].ID)
}

func TestTopKResourceAwareFiltersCommitmentBeforeCacheRanking(t *testing.T) {
	registry := NewInMemoryRegistry()
	cpuFull := resourceAwareFastlet("cpu-full-cache-hit", 4, fastletapi.AdmissionStateOpen, 1, "alpine:latest")
	memoryFull := resourceAwareFastlet("memory-full-cache-hit", 2, fastletapi.AdmissionStateOpen, 1, "alpine:latest")
	memoryFull.ResourceCapacity.Memory = resource.MustParse("4Gi")
	eligible := resourceAwareFastlet("eligible-cache-miss", 1, fastletapi.AdmissionStateOpen, 1)
	for _, info := range []FastletInfo{cpuFull, memoryFull, eligible} {
		seedFastlet(t, registry, info)
	}

	selected := registry.TopK(resourceAwareCandidate("alpine:latest"), 3)
	require.Len(t, selected, 1)
	require.Equal(t, FastletID("eligible-cache-miss"), selected[0].ID)
}

func TestTopKResourceAwareRequiresFreshEligibleStateAndRanksOpenFirst(t *testing.T) {
	registry := NewInMemoryRegistry()
	open := resourceAwareFastlet("open-cache-miss", 1, fastletapi.AdmissionStateOpen, 1)
	degraded := resourceAwareFastlet("degraded-cache-hit", 0, fastletapi.AdmissionStateDegraded, 1, "alpine:latest")
	blocked := resourceAwareFastlet("blocked", 0, fastletapi.AdmissionStateBlocked, 1)
	unknown := resourceAwareFastlet("unknown", 0, fastletapi.AdmissionStateUnknown, 1)
	stale := resourceAwareFastlet("stale", 0, fastletapi.AdmissionStateOpen, 1)
	stale.LastHeartbeat = registryNow.Add(-time.Minute)
	for _, info := range []FastletInfo{open, degraded, blocked, unknown, stale} {
		seedFastlet(t, registry, info)
	}

	selected := registry.TopK(resourceAwareCandidate("alpine:latest"), 10)
	require.Len(t, selected, 2)
	require.Equal(t, FastletID("open-cache-miss"), selected[0].ID)
	require.Equal(t, FastletID("degraded-cache-hit"), selected[1].ID)
}

func TestTopKPropagatesFreshNodeMemoryPressureAcrossFastlets(t *testing.T) {
	registry := NewInMemoryRegistry()
	pressured := resourceAwareFastlet("pressured", 0, fastletapi.AdmissionStateBlocked, 2)
	pressured.Admission.Resources.Reason = fastletapi.AdmissionReasonNodeMemoryPressure
	sameNodeOpen := resourceAwareFastlet("same-node-open", 0, fastletapi.AdmissionStateOpen, 2)
	otherNodeOpen := resourceAwareFastlet("other-node-open", 0, fastletapi.AdmissionStateOpen, 2)
	otherNodeOpen.NodeName = "node-b"
	for _, info := range []FastletInfo{pressured, sameNodeOpen, otherNodeOpen} {
		seedFastlet(t, registry, info)
	}

	selected := registry.TopK(resourceAwareCandidate("alpine:latest"), 10)
	require.Len(t, selected, 1)
	require.Equal(t, FastletID("other-node-open"), selected[0].ID)
}

func TestStablePerturbationIsDeterministic(t *testing.T) {
	registry := NewInMemoryRegistry()
	for _, id := range []string{"fastlet-a", "fastlet-b", "fastlet-c"} {
		seedFastlet(t, registry, readyFastlet(id, 0, 5))
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
	seedFastlet(t, registry, old)
	registry.UpsertPod(FastletInfo{
		ID: old.ID, Namespace: old.Namespace, PodName: old.PodName, PodUID: "replacement-uid",
		PodIP: "10.0.0.2", PoolName: old.PoolName, PodReady: true, PodObservedAt: registryNow,
	})
	replaced, _ := registry.GetFastletByID(old.ID)
	require.Equal(t, "replacement-uid", replaced.PodUID)
	require.False(t, replaced.RuntimeReady)
	require.Zero(t, replaced.Capacity())
	require.Empty(t, replaced.Images)
	require.True(t, replaced.LastHeartbeat.IsZero())
}

func TestSamePodWatchUpdatePreservesHeartbeatButRefreshesExpectedProfiles(t *testing.T) {
	registry := NewInMemoryRegistry()
	existing := readyFastlet("fastlet-a", 1, 5, "alpine:latest")
	seedFastlet(t, registry, existing)

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
	heartbeat := &fastletapi.HeartbeatResponse{
		FastletStatus: fastletapi.FastletStatus{
			FastletPodUID: "pod-uid-a", RuntimeReady: true, RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
			Admission: fastletapi.AdmissionStatus{Capacity: 5, Used: 2, Running: 2},
		},
		Sequence: 1, Cache: fastletapi.CacheSnapshot{Epoch: "boot-a", Revision: 1, Full: true, Complete: true, Images: []string{"alpine:latest"}},
	}
	require.NoError(t, registry.ApplyHeartbeat("fastlet-a", "pod-uid-a", heartbeat, registryNow))
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.True(t, stored.RuntimeReady)
	require.Equal(t, 2, stored.Used())
	require.Equal(t, []string{"alpine:latest"}, stored.Images)

	unchanged := *heartbeat
	unchanged.Sequence = 2
	unchanged.Cache = fastletapi.CacheSnapshot{Epoch: "boot-a", Revision: 1, Full: false, Complete: true}
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

func heartbeatWithProfilesForRegistry(runtimeHash, resourceHash string) *fastletapi.HeartbeatResponse {
	return &fastletapi.HeartbeatResponse{
		FastletStatus: fastletapi.FastletStatus{
			FastletPodUID: "pod-uid-a", RuntimeReady: true, RuntimeProfileHash: runtimeHash, ResourceProfileHash: resourceHash,
			Admission: fastletapi.AdmissionStatus{Capacity: 5},
		},
		Sequence: 1, Cache: fastletapi.CacheSnapshot{Epoch: "boot-a", Revision: 1, Full: true, Complete: true},
	}
}

func TestLocalCapacityFeedbackSuppressesCandidateUntilHeartbeat(t *testing.T) {
	registry := NewInMemoryRegistry()
	seedFastlet(t, registry, readyFastlet("fastlet-a", 0, 5))
	registry.RecordFeedback("fastlet-a", LocalFeedback{
		Code: fastletapi.ErrorCapacityRejected, ObservedAt: registryNow, RetryAfter: 10 * time.Second,
	})
	require.Empty(t, registry.TopK(candidate("alpine:latest", "request-a"), 1))
	request := candidate("alpine:latest", "request-a")
	request.Now = registryNow.Add(11 * time.Second)
	require.Len(t, registry.TopK(request, 1), 1)
}

func TestResourceRejectionFeedbackFencesOlderHealthyHeartbeat(t *testing.T) {
	registry := NewInMemoryRegistry()
	initial := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateOpen, 1)
	seedFastlet(t, registry, initial)

	rejected := initial.Admission
	rejected.Resources = &fastletapi.ResourceAdmissionStatus{
		Version: "v1", Epoch: "resource-epoch-a", Revision: 3,
		State: fastletapi.AdmissionStateBlocked, Reason: fastletapi.AdmissionReasonFastletMemoryHighWatermark,
	}
	registry.RecordFeedback("fastlet-a", LocalFeedback{
		Code: fastletapi.ErrorCapacityRejected, Reason: fastletapi.AdmissionReasonFastletMemoryHighWatermark,
		Admission: &rejected, ObservedAt: registryNow.Add(time.Second), RetryAfter: time.Minute,
	})

	older := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateOpen, 2)
	older.HeartbeatSequence = 2
	seedFastlet(t, registry, older)
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.Equal(t, uint64(3), stored.Admission.Resources.Revision)
	require.Equal(t, fastletapi.AdmissionStateBlocked, stored.Admission.Resources.State)

	recovered := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateOpen, 4)
	recovered.HeartbeatSequence = 3
	recovered.LastHeartbeat = registryNow.Add(2 * time.Second)
	seedFastlet(t, registry, recovered)
	stored, _ = registry.GetFastletByID("fastlet-a")
	require.Equal(t, uint64(4), stored.Admission.Resources.Revision)
	require.Equal(t, fastletapi.AdmissionStateOpen, stored.Admission.Resources.State)
	require.True(t, stored.RejectedUntil.IsZero())
}

func TestSameResourceRevisionRefreshesLocalFreshness(t *testing.T) {
	registry := NewInMemoryRegistry()
	info := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateOpen, 7)
	seedFastlet(t, registry, info)

	refresh := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateOpen, 7)
	refresh.HeartbeatSequence = 2
	refresh.LastHeartbeat = registryNow.Add(20 * time.Second)
	seedFastlet(t, registry, refresh)
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.Equal(t, registryNow.Add(20*time.Second), stored.ResourceReceivedAt)
}

func TestNewResourceEpochHeartbeatSupersedesRejectionFence(t *testing.T) {
	registry := NewInMemoryRegistry()
	initial := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateBlocked, 8)
	initial.Admission.Resources.Reason = fastletapi.AdmissionReasonFastletMemoryHighWatermark
	seedFastlet(t, registry, initial)
	registry.RecordFeedback("fastlet-a", LocalFeedback{
		Code: fastletapi.ErrorCapacityRejected, Admission: &initial.Admission, ObservedAt: registryNow,
	})

	restarted := resourceAwareFastlet("fastlet-a", 1, fastletapi.AdmissionStateOpen, 1)
	restarted.Admission.Resources.Epoch = "resource-epoch-b"
	restarted.HeartbeatSequence = 2
	restarted.CacheEpoch = "boot-b"
	restarted.LastHeartbeat = registryNow.Add(time.Second)
	seedFastlet(t, registry, restarted)
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.Equal(t, "resource-epoch-b", stored.Admission.Resources.Epoch)
	require.Equal(t, fastletapi.AdmissionStateOpen, stored.Admission.Resources.State)
	require.True(t, stored.RejectedUntil.IsZero())
}

func TestRegistryViewsConvergeWithoutSharedAllocationState(t *testing.T) {
	left, right := NewInMemoryRegistry(), NewInMemoryRegistry()
	left.clock = func() time.Time { return registryNow }
	right.clock = func() time.Time { return registryNow }
	for _, registry := range []*InMemoryRegistry{left, right} {
		seedFastlet(t, registry, readyFastlet("fastlet-a", 1, 5, "alpine:latest"))
		seedFastlet(t, registry, readyFastlet("fastlet-b", 0, 5, "ubuntu:24.04"))
	}
	require.Equal(t, left.TopK(candidate("alpine:latest", "request-a"), 2), right.TopK(candidate("alpine:latest", "request-a"), 2))
	for _, id := range []FastletID{"fastlet-a", "fastlet-b"} {
		leftInfo, _ := left.GetFastletByID(id)
		rightInfo, _ := right.GetFastletByID(id)
		require.Equal(t, leftInfo, rightInfo, "selection does not create divergent slot ownership")
	}
}

func TestGetReturnsDeepCopy(t *testing.T) {
	registry := NewInMemoryRegistry()
	seedFastlet(t, registry, readyFastlet("fastlet-a", 0, 5, "alpine:latest"))
	copy, _ := registry.GetFastletByID("fastlet-a")
	copy.Images[0] = "mutated"
	copy.SandboxStatuses["x"] = fastletapi.SandboxStatus{SandboxID: "x"}
	stored, _ := registry.GetFastletByID("fastlet-a")
	require.Equal(t, []string{"alpine:latest"}, stored.Images)
	require.Empty(t, stored.SandboxStatuses)
}

func TestTopKReturnsDeepCopy(t *testing.T) {
	registry := NewInMemoryRegistry()
	info := readyFastlet("fastlet-a", 0, 5, "alpine:latest")
	info.PreparedArtifacts = []string{"component-artifact-a"}
	info.SandboxStatuses["sandbox-a"] = fastletapi.SandboxStatus{
		SandboxID: "sandbox-a",
		InfraComponents: []fastletapi.InfraComponentDiagnostic{{
			Component: "execd",
			State:     "ready",
		}},
	}
	seedFastlet(t, registry, info)

	selected := registry.TopK(candidate("alpine:latest", "request-a"), 1)
	require.Len(t, selected, 1)
	selected[0].Images[0] = "mutated"
	selected[0].PreparedArtifacts[0] = "mutated"
	selectedStatus := selected[0].SandboxStatuses["sandbox-a"]
	selectedStatus.SandboxID = "mutated"
	selectedStatus.InfraComponents[0].State = "mutated"
	selected[0].SandboxStatuses["sandbox-a"] = selectedStatus

	stored, ok := registry.GetFastletByID("fastlet-a")
	require.True(t, ok)
	require.Equal(t, []string{"alpine:latest"}, stored.Images)
	require.Equal(t, []string{"component-artifact-a"}, stored.PreparedArtifacts)
	require.Equal(t, "sandbox-a", stored.SandboxStatuses["sandbox-a"].SandboxID)
	require.Equal(t, "ready", stored.SandboxStatuses["sandbox-a"].InfraComponents[0].State)
}

func TestTopKIsSafeDuringConcurrentRegistryUpdates(t *testing.T) {
	registry := NewInMemoryRegistry()
	seedFastlet(t, registry, readyFastlet("fastlet-a", 0, 5, "alpine:latest"))

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
			seedFastlet(t, registry, readyFastlet("fastlet-a", index%4, 5, image))
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
	seedFastlet(t, registry, readyFastlet("fastlet-a", 0, 100))
	manager, err := fastletsandbox.NewSandboxManagerWithConfig(&testutil.FakeRuntime{}, fastletsandbox.SandboxManagerConfig{
		Capacity: 5, FastletPodUID: "uid-fastlet-a",
	})
	require.NoError(t, err)
	successes := 0
	for index := 0; index < 100; index++ {
		sandboxUID := fmt.Sprintf("sandbox-%03d", index)
		require.Len(t, registry.TopK(candidate("alpine:latest", sandboxUID), 1), 1)
		_, err = manager.CreateSandbox(context.Background(), &fastletapi.CreateSandboxRequest{
			RequestID:      "request-" + sandboxUID,
			Identity:       fastletapi.SandboxIdentity{SandboxUID: sandboxUID, Namespace: "default", Name: sandboxUID, InstanceGeneration: 1, RuntimeInstanceID: "runtime-" + sandboxUID, AssignmentAttempt: 1, FastletPodUID: "uid-fastlet-a"},
			Sandbox:        fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
			SpecGeneration: 1,
		})
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 5, successes)
	admission, _, _ := manager.State()
	require.Equal(t, 5, admission.Used)
}
