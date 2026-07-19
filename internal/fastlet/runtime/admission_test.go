package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

type admissionRuntime struct {
	mu            sync.Mutex
	sandboxes     map[string]*SandboxMetadata
	managed       []*SandboxMetadata
	ensureCalls   int
	deleteCalls   int
	ensureError   error
	ensureEntered chan struct{}
	ensureBlock   chan struct{}
	deleteEntered chan struct{}
	deleteBlock   chan struct{}
	pullEntered   chan struct{}
	pullBlock     chan struct{}
	pulledImages  []string
	images        []string
}

func newAdmissionRuntime() *admissionRuntime {
	return &admissionRuntime{sandboxes: make(map[string]*SandboxMetadata)}
}

func (*admissionRuntime) Initialize(context.Context, string) error { return nil }
func (*admissionRuntime) SetNamespace(string)                      {}
func (*admissionRuntime) Close() error                             { return nil }
func (*admissionRuntime) ProbeCapabilities(context.Context) CapabilityReport {
	return CapabilityReport{State: runtimecatalog.CapabilityReady, Reason: "TestRuntimeReady"}
}

func (r *admissionRuntime) EnsureSandbox(_ context.Context, spec *api.SandboxSpec) (*SandboxMetadata, error) {
	r.mu.Lock()
	r.ensureCalls++
	err := r.ensureError
	entered, block := r.ensureEntered, r.ensureBlock
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	metadata := &SandboxMetadata{SandboxSpec: *spec, ContainerID: spec.SandboxID, Phase: "running", CreatedAt: time.Now().Unix()}
	r.mu.Lock()
	r.sandboxes[spec.SandboxID] = metadata
	r.mu.Unlock()
	copy := *metadata
	return &copy, nil
}

func (r *admissionRuntime) InspectSandbox(_ context.Context, sandboxID string) (*SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	metadata := r.sandboxes[sandboxID]
	if metadata == nil {
		return nil, ErrSandboxNotFound
	}
	copy := *metadata
	return &copy, nil
}

func (r *admissionRuntime) DeleteSandbox(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	r.deleteCalls++
	entered, block := r.deleteEntered, r.deleteBlock
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	r.mu.Lock()
	delete(r.sandboxes, sandboxID)
	r.mu.Unlock()
	return nil
}

func (r *admissionRuntime) ListManagedSandboxes(context.Context) ([]*SandboxMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.managed != nil {
		result := make([]*SandboxMetadata, 0, len(r.managed))
		for _, metadata := range r.managed {
			copy := *metadata
			result = append(result, &copy)
		}
		return result, nil
	}
	result := make([]*SandboxMetadata, 0, len(r.sandboxes))
	for _, metadata := range r.sandboxes {
		copy := *metadata
		result = append(result, &copy)
	}
	return result, nil
}

func (r *admissionRuntime) ListImages(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.images...), nil
}

func (r *admissionRuntime) PullImage(_ context.Context, image string) error {
	r.mu.Lock()
	entered, block := r.pullEntered, r.pullBlock
	r.pulledImages = append(r.pulledImages, image)
	r.images = append(r.images, image)
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return nil
}

func (r *admissionRuntime) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureCalls, r.deleteCalls
}

type admissionClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *admissionClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *admissionClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func newAdmissionManager(t *testing.T, runtime RuntimeDriver, capacity int) *SandboxManager {
	t.Helper()
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: capacity, FastletPodUID: "pod-uid-a",
	})
	require.NoError(t, err)
	return manager
}

func ensureRequest(uid string, generation, attempt int64) *api.EnsureSandboxRequest {
	return &api.EnsureSandboxRequest{
		Identity: api.SandboxIdentity{
			RequestID: "request-" + uid, SandboxUID: uid,
			InstanceGeneration: generation, AssignmentAttempt: attempt, FastletPodUID: "pod-uid-a",
		},
		Sandbox: api.SandboxSpec{ClaimUID: "claim-" + uid, ClaimName: uid, Image: "alpine:latest"},
	}
}

func requireFastletCode(t *testing.T, err error, code api.FastletErrorCode) {
	t.Helper()
	var failure *api.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, code, failure.Code)
}

func TestAdmissionNeverExceedsCapacityUnderConcurrency(t *testing.T) {
	runtime := newAdmissionRuntime()
	manager := newAdmissionManager(t, runtime, 5)
	var successes atomic.Int64
	var rejected atomic.Int64
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			_, err := manager.EnsureSandboxV2(context.Background(), ensureRequest(fmt.Sprintf("sandbox-%03d", index), 1, 1))
			if err == nil {
				successes.Add(1)
				return
			}
			var failure *api.FastletError
			if errors.As(err, &failure) && failure.Code == api.ErrorCapacityRejected {
				rejected.Add(1)
				return
			}
			t.Errorf("unexpected Ensure error: %v", err)
		}(i)
	}
	close(start)
	group.Wait()
	require.EqualValues(t, 5, successes.Load())
	require.EqualValues(t, 95, rejected.Load())
	admission, _, _ := manager.State()
	require.Equal(t, 5, admission.Used)
	require.Equal(t, 5, admission.Running)
}

func TestDuplicateEnsureCreatesRuntimeOnce(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureEntered = make(chan struct{}, 1)
	runtime.ensureBlock = make(chan struct{})
	manager := newAdmissionManager(t, runtime, 5)
	request := ensureRequest("sandbox-a", 1, 1)

	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := manager.EnsureSandboxV2(context.Background(), request)
			if err != nil {
				var failure *api.FastletError
				require.True(t, errors.As(err, &failure))
				require.Equal(t, api.ErrorInProgress, failure.Code)
			}
		}()
	}
	close(start)
	<-runtime.ensureEntered
	time.Sleep(10 * time.Millisecond)
	close(runtime.ensureBlock)
	group.Wait()
	ensureCalls, _ := runtime.counts()
	require.Equal(t, 1, ensureCalls)
}

func TestEnsureFailureReleasesCapacity(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureError = errors.New("create failed")
	manager := newAdmissionManager(t, runtime, 1)
	_, err := manager.EnsureSandboxV2(context.Background(), ensureRequest("sandbox-a", 1, 1))
	requireFastletCode(t, err, api.ErrorRuntimeUnavailable)
	runtime.mu.Lock()
	runtime.ensureError = nil
	runtime.mu.Unlock()
	_, err = manager.EnsureSandboxV2(context.Background(), ensureRequest("sandbox-b", 1, 1))
	require.NoError(t, err)
	admission, _, _ := manager.State()
	require.Equal(t, 1, admission.Running)
}

func TestReservationTTLAndCancelReleaseCapacity(t *testing.T) {
	clock := &admissionClock{now: time.Unix(100, 0)}
	var token atomic.Int64
	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", Clock: clock, ReservationTTL: 5 * time.Second,
		TokenGenerator: func() (string, error) { return fmt.Sprintf("token-%d", token.Add(1)), nil },
	})
	require.NoError(t, err)
	first, err := manager.ReserveSandbox(&api.ReserveSandboxRequest{RequestID: "request-a", CreateSpecHash: "spec-a", FastletPodUID: "pod-uid-a"})
	require.NoError(t, err)
	_, err = manager.ReserveSandbox(&api.ReserveSandboxRequest{RequestID: "request-b", CreateSpecHash: "spec-b", FastletPodUID: "pod-uid-a"})
	requireFastletCode(t, err, api.ErrorCapacityRejected)
	clock.Advance(6 * time.Second)
	second, err := manager.ReserveSandbox(&api.ReserveSandboxRequest{RequestID: "request-b", CreateSpecHash: "spec-b", FastletPodUID: "pod-uid-a"})
	require.NoError(t, err)
	require.NotEqual(t, first.ReservationToken, second.ReservationToken)
	_, err = manager.CancelReservation(&api.CancelReservationRequest{RequestID: "request-b", ReservationToken: second.ReservationToken})
	require.NoError(t, err)
	_, err = manager.ReserveSandbox(&api.ReserveSandboxRequest{RequestID: "request-c", CreateSpecHash: "spec-c", FastletPodUID: "pod-uid-a"})
	require.NoError(t, err)
}

func TestIdentityFencingAndClaimConflict(t *testing.T) {
	manager := newAdmissionManager(t, newAdmissionRuntime(), 2)
	request := ensureRequest("sandbox-a", 2, 3)
	_, err := manager.EnsureSandboxV2(context.Background(), request)
	require.NoError(t, err)

	stale := request.Identity
	stale.InstanceGeneration = 1
	_, err = manager.InspectSandboxV2(&api.InspectSandboxRequest{Identity: stale})
	requireFastletCode(t, err, api.ErrorStaleGeneration)

	wrongPod := request.Identity
	wrongPod.FastletPodUID = "pod-uid-b"
	_, err = manager.InspectSandboxV2(&api.InspectSandboxRequest{Identity: wrongPod})
	requireFastletCode(t, err, api.ErrorStaleAssignment)

	conflict := *request
	conflict.Sandbox = request.Sandbox
	conflict.Sandbox.ClaimUID = "another-claim"
	_, err = manager.EnsureSandboxV2(context.Background(), &conflict)
	requireFastletCode(t, err, api.ErrorConflict)
}

func TestDeleteIsIdempotentAndFenced(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.deleteEntered = make(chan struct{}, 1)
	runtime.deleteBlock = make(chan struct{})
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 2)
	_, err := manager.EnsureSandboxV2(context.Background(), request)
	require.NoError(t, err)

	stale := request.Identity
	stale.AssignmentAttempt = 1
	_, err = manager.DeleteSandboxV2(&api.DeleteSandboxV2Request{Identity: stale})
	requireFastletCode(t, err, api.ErrorStaleGeneration)

	_, err = manager.DeleteSandboxV2(&api.DeleteSandboxV2Request{Identity: request.Identity})
	require.NoError(t, err)
	<-runtime.deleteEntered
	_, err = manager.DeleteSandboxV2(&api.DeleteSandboxV2Request{Identity: request.Identity})
	require.NoError(t, err)
	close(runtime.deleteBlock)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	_, deletes := runtime.counts()
	require.Equal(t, 1, deletes)
}

func TestDeleteDuringCreateWinsWithoutOrphan(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.ensureEntered = make(chan struct{}, 1)
	runtime.ensureBlock = make(chan struct{})
	manager := newAdmissionManager(t, runtime, 1)
	request := ensureRequest("sandbox-a", 1, 1)
	result := make(chan error, 1)
	go func() {
		_, err := manager.EnsureSandboxV2(context.Background(), request)
		result <- err
	}()
	<-runtime.ensureEntered
	_, err := manager.DeleteSandboxV2(&api.DeleteSandboxV2Request{Identity: request.Identity})
	require.NoError(t, err)
	close(runtime.ensureBlock)
	requireFastletCode(t, <-result, api.ErrorConflict)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
	_, deletes := runtime.counts()
	require.Equal(t, 1, deletes)
}

func TestRecoveryBlocksReadinessAndRestoresCapacity(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.managed = []*SandboxMetadata{
		{SandboxSpec: api.SandboxSpec{SandboxID: "sandbox-a", ClaimUID: "claim-a", FastletPodUID: "pod-uid-a", InstanceGeneration: 2, AssignmentAttempt: 3}, Phase: "running"},
		{SandboxSpec: api.SandboxSpec{SandboxID: "stale-sandbox", ClaimUID: "claim-stale", FastletPodUID: "old-pod-uid", InstanceGeneration: 1, AssignmentAttempt: 1}, Phase: "running"},
	}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RecoverOnStart: true,
	})
	require.NoError(t, err)
	require.False(t, manager.Ready())
	_, err = manager.EnsureSandboxV2(context.Background(), ensureRequest("sandbox-b", 1, 1))
	requireFastletCode(t, err, api.ErrorRuntimeUnavailable)
	require.NoError(t, manager.Recover(context.Background()))
	require.True(t, manager.Ready())
	admission, recovering, _ := manager.State()
	require.False(t, recovering)
	require.Equal(t, 1, admission.Running)
	_, err = manager.EnsureSandboxV2(context.Background(), ensureRequest("sandbox-b", 1, 1))
	requireFastletCode(t, err, api.ErrorCapacityRejected)
}

func TestWarmImagesAreAsynchronousAndProtectedFromEviction(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.pullEntered = make(chan struct{}, 2)
	runtime.pullBlock = make(chan struct{})
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", WarmImages: []string{"alpine:latest", "ubuntu:24.04"},
	})
	require.NoError(t, err)
	require.True(t, manager.Ready(), "warmImages never gate Fastlet readiness")
	completed := make(chan error, 1)
	go func() { completed <- manager.WarmCache(context.Background()) }()
	<-runtime.pullEntered
	require.True(t, manager.Ready())
	close(runtime.pullBlock)
	require.NoError(t, <-completed)
	runtime.mu.Lock()
	require.ElementsMatch(t, []string{"alpine:latest", "ubuntu:24.04"}, runtime.pulledImages)
	runtime.mu.Unlock()
	require.Equal(t, []string{"cold:1"}, manager.PlanCacheEviction([]string{"ubuntu:24.04", "cold:1", "alpine:latest"}))
}
