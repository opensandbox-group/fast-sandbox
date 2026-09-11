package reconciler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type controllerRegistry struct {
	candidates []placement.FastletInfo
	fastlets   map[placement.FastletID]placement.FastletInfo
}

func (r *controllerRegistry) TopK(placement.CandidateRequest, int) []placement.FastletInfo {
	return append([]placement.FastletInfo(nil), r.candidates...)
}

func (r *controllerRegistry) GetFastletByID(id placement.FastletID) (placement.FastletInfo, bool) {
	value, ok := r.fastlets[id]
	return value, ok
}

func (*controllerRegistry) RecordFeedback(placement.FastletID, placement.LocalFeedback) {}

type controllerFastlet struct {
	mu                   sync.Mutex
	ensureErr            error
	inspectErr           error
	ensurePhase          string
	runtimes             map[string]string
	ensureCall           int
	deleteCall           int
	snapshotCreateErr    error
	snapshotInspectErr   error
	snapshotInspectPhase fastletapi.SnapshotPhase
	snapshotDeleteCall   int
	lastSnapshotInspect  *fastletapi.SnapshotIdentity
}

func (f *controllerFastlet) CreateSandbox(_ context.Context, _ string, request *fastletapi.CreateSandboxRequest) (*fastletapi.CreateSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCall++
	if f.ensureErr != nil {
		return &fastletapi.CreateSandboxResponse{}, f.ensureErr
	}
	phase := f.ensurePhase
	if phase == "" {
		phase = "running"
	}
	f.runtimes[request.Identity.SandboxUID] = phase
	status := controllerObservation(request.Identity.SandboxUID, phase)
	status.AcceptedGeneration = request.SpecGeneration
	status.AppliedGeneration = request.SpecGeneration
	return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionCreated, Sandbox: status}, nil
}

func (f *controllerFastlet) InspectSandbox(_ context.Context, _ string, request *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	phase, ok := f.runtimes[request.Identity.SandboxUID]
	if !ok {
		failure := &fastletapi.FastletError{Code: fastletapi.ErrorNotFound, Message: "not found"}
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	return &fastletapi.InspectSandboxResponse{Sandbox: controllerObservation(request.Identity.SandboxUID, phase)}, nil
}

func (f *controllerFastlet) DeleteSandbox(_ context.Context, _ string, request *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCall++
	delete(f.runtimes, request.Identity.SandboxUID)
	return &fastletapi.DeleteSandboxResponse{}, nil
}

func (f *controllerFastlet) ReconcileBindings(_ context.Context, _ string, request *fastletapi.ReconcileBindingsRequest) (*fastletapi.ReconcileBindingsResponse, error) {
	f.mu.Lock()
	phase := f.runtimes[request.Identity.SandboxUID]
	f.mu.Unlock()
	if phase == "" {
		phase = "running"
	}
	return &fastletapi.ReconcileBindingsResponse{Sandbox: &fastletapi.SandboxStatus{
		SandboxID:          request.Identity.SandboxUID,
		Runtime:            controllerObservation(request.Identity.SandboxUID, phase).Runtime,
		DataPlane:          controllerObservation(request.Identity.SandboxUID, phase).DataPlane,
		AcceptedGeneration: request.SpecGeneration, AppliedGeneration: request.SpecGeneration,
	}}, nil
}

func (f *controllerFastlet) CreateSnapshot(_ context.Context, _ string, request *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshotCreateErr != nil {
		return &fastletapi.CreateSnapshotResponse{}, f.snapshotCreateErr
	}
	f.runtimes[request.Identity.Sandbox.SandboxUID] = "running"
	return &fastletapi.CreateSnapshotResponse{
		Disposition: fastletapi.CreateDispositionCreated,
		Snapshot: &fastletapi.SnapshotStatus{
			SnapshotID: "snap-" + request.Identity.SnapshotUID, Phase: fastletapi.SnapshotPhaseCreating,
		},
	}, nil
}

func (f *controllerFastlet) InspectSnapshot(_ context.Context, _ string, request *fastletapi.InspectSnapshotRequest) (*fastletapi.InspectSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSnapshotInspect = &request.Identity
	if f.snapshotInspectErr != nil {
		return &fastletapi.InspectSnapshotResponse{}, f.snapshotInspectErr
	}
	phase := f.snapshotInspectPhase
	if phase == "" {
		phase = fastletapi.SnapshotPhaseSucceeded
	}
	return &fastletapi.InspectSnapshotResponse{Snapshot: &fastletapi.SnapshotStatus{
		SnapshotID: "snap-" + request.Identity.SnapshotUID, Phase: phase,
		ManifestRef: "s3://bucket/publish/abc123/manifest.json", ArtifactDigest: "deadbeef", SizeBytes: 42,
	}}, nil
}

func (f *controllerFastlet) DeleteSnapshot(_ context.Context, _ string, _ *fastletapi.DeleteSnapshotRequest) (*fastletapi.DeleteSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotDeleteCall++
	return &fastletapi.DeleteSnapshotResponse{}, nil
}

func controllerObservation(sandboxID, phase string) *fastletapi.SandboxStatus {
	status := &fastletapi.SandboxStatus{SandboxID: sandboxID}
	switch phase {
	case "running":
		status.Runtime.State = fastletapi.RuntimeStateReady
		status.DataPlane.State = fastletapi.DataPlaneStateReady
	case "infra-pending", "route-pending":
		status.Runtime.State = fastletapi.RuntimeStateReady
		status.DataPlane.State = fastletapi.DataPlaneStatePublishing
	case "infra-unavailable", "route-unavailable":
		status.Runtime.State = fastletapi.RuntimeStateReady
		status.DataPlane.State = fastletapi.DataPlaneStateUnavailable
	default:
		status.Runtime.State = fastletapi.RuntimeStateCreating
		status.DataPlane.State = fastletapi.DataPlaneStatePending
	}
	return status
}

func TestDeclarativeCreateWithoutCapacityStaysPending(t *testing.T) {
	reconciler, registry, _, sandbox := newControllerHarness(t)
	registry.candidates = nil
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.Empty(t, current.Status.Placement.FastletName)
	require.Equal(t, apiv1alpha2.RuntimePending, current.Status.Runtime.State)
}

func TestDeclarativeCreateUsesSharedV2Orchestrator(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotEmpty(t, current.Status.Placement.FastletName)
	require.Equal(t, types.UID("pod-a"), current.Status.Placement.FastletPodUID)
	require.Equal(t, int64(1), current.Status.Placement.Attempt)
	require.Equal(t, apiv1alpha2.RuntimeReady, current.Status.Runtime.State)
	require.Equal(t, apiv1alpha2.DataPlaneReady, current.Status.DataPlane.State)
	fastlet.mu.Lock()
	require.Equal(t, 1, fastlet.ensureCall)
	fastlet.mu.Unlock()
}

func TestDeclarativeCreatePollsDataPlaneWithoutBlockingRuntimeReady(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	fastlet.ensurePhase = "infra-pending"

	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	result, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	require.Equal(t, ObservationPollInterval, result.RequeueAfter)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.Equal(t, apiv1alpha2.RuntimeReady, current.Status.Runtime.State)
	require.Equal(t, apiv1alpha2.DataPlanePublishing, current.Status.DataPlane.State)

	fastlet.ensurePhase = "running"
	fastlet.mu.Lock()
	fastlet.runtimes[string(current.UID)] = "running"
	fastlet.mu.Unlock()
	result, err = reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	require.Equal(t, ReadyRequeueInterval, result.RequeueAfter)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Equal(t, apiv1alpha2.DataPlaneReady, current.Status.DataPlane.State)
}

func TestExplicitCapacityRejectionPreservesDurableAssignmentAndAttemptFence(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	failure := &fastletapi.CreateCallError{Disposition: fastletapi.CreateDispositionRejectedBeforeSideEffects, Failure: &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "full", Retryable: true}}
	fastlet.ensureErr = failure
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotEmpty(t, current.Status.Placement.FastletName)
	require.Equal(t, "fastlet-a", current.Status.Placement.FastletName)
	require.NotEmpty(t, current.Annotations["sandbox.fast.io/assignment"])
	require.Equal(t, int64(1), current.Status.Placement.Attempt)
	require.Equal(t, apiv1alpha2.RuntimePending, current.Status.Runtime.State)
}

func TestUnknownOutcomePreservesDurableAssignment(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	fastlet.ensureErr = errors.New("response lost")
	fastlet.inspectErr = errors.New("connection unavailable")
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotEmpty(t, current.Status.Placement.FastletName)
	require.Equal(t, "fastlet-a", current.Status.Placement.FastletName)
}

func TestPodLostPolicyManualAndAutoRecreate(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		policy apiv1alpha2.FailurePolicy
		auto   bool
	}{
		{name: "manual", policy: apiv1alpha2.FailurePolicyManual},
		{name: "auto", policy: apiv1alpha2.FailurePolicyAutoRecreate, auto: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reconciler, registry, _, sandbox := newControllerHarness(t)
			now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
			reconciler.Now = func() time.Time { return now }
			placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
			current := getControllerSandbox(t, reconciler, sandbox.Name)
			current.Spec.FailurePolicy = testCase.policy
			current.Spec.RecoveryTimeoutSeconds = 1
			require.NoError(t, reconciler.Update(context.Background(), current))
			current = getControllerSandbox(t, reconciler, sandbox.Name)
			current = seedReadyControllerAssignment(t, reconciler, current, placementStatus)
			var fastletPod corev1.Pod
			require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-a"}, &fastletPod))
			require.NoError(t, reconciler.Delete(context.Background(), &fastletPod))
			registry.fastlets = map[placement.FastletID]placement.FastletInfo{}
			registry.candidates = nil
			reconcileTwice(t, reconciler, sandbox.Name)
			current = getControllerSandbox(t, reconciler, sandbox.Name)
			require.NotNil(t, current.Status.Placement.Recovery)
			require.True(t, current.Status.Placement.Recovery.Deadline.Time.Equal(now.Add(time.Second)))
			now = now.Add(2 * time.Second)
			reconcileTwice(t, reconciler, sandbox.Name)
			current = getControllerSandbox(t, reconciler, sandbox.Name)
			if testCase.auto {
				require.Empty(t, current.Status.Placement.FastletName)
				require.Equal(t, int64(2), current.Status.Runtime.Generation)
			} else {
				require.NotEmpty(t, current.Status.Placement.FastletName)
				require.Equal(t, apiv1alpha2.RuntimeUnavailable, current.Status.Runtime.State)
				require.True(t, current.Status.HasCondition(apiv1alpha2.SandboxConditionReady, metav1.ConditionFalse, orchestration.ReasonFastletPodLost))
			}
		})
	}
}

func TestRegistryMissDoesNotMeanFastletPodLost(t *testing.T) {
	reconciler, registry, _, sandbox := newControllerHarness(t)
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Spec.FailurePolicy = apiv1alpha2.FailurePolicyAutoRecreate
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current = seedReadyControllerAssignment(t, reconciler, current, placementStatus)
	registry.fastlets = map[placement.FastletID]placement.FastletInfo{}

	reconcileTwice(t, reconciler, sandbox.Name)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotEmpty(t, current.Status.Placement.FastletName)
	require.Equal(t, int64(1), current.Status.Runtime.Generation)
	require.Equal(t, apiv1alpha2.RuntimeUnavailable, current.Status.Runtime.State)
	require.False(t, current.Status.HasCondition(apiv1alpha2.SandboxConditionReady, metav1.ConditionFalse, orchestration.ReasonFastletPodLost))
}

func TestReplacementPodWithSameNameCannotClaimOldAssignment(t *testing.T) {
	reconciler, registry, _, sandbox := newControllerHarness(t)
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	reconciler.Now = func() time.Time { return now }
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Spec.FailurePolicy = apiv1alpha2.FailurePolicyAutoRecreate
	current.Spec.RecoveryTimeoutSeconds = 1
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current = seedReadyControllerAssignment(t, reconciler, current, placementStatus)
	var oldPod corev1.Pod
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-a"}, &oldPod))
	require.NoError(t, reconciler.Delete(context.Background(), &oldPod))
	replacement := oldPod.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("pod-b")
	require.NoError(t, reconciler.Create(context.Background(), replacement))
	registry.fastlets = map[placement.FastletID]placement.FastletInfo{}
	registry.candidates = nil

	reconcileTwice(t, reconciler, sandbox.Name)
	now = now.Add(2 * time.Second)
	reconcileTwice(t, reconciler, sandbox.Name)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Empty(t, current.Status.Placement.FastletName)
	require.Equal(t, int64(2), current.Status.Runtime.Generation)
	require.Equal(t, int64(1), current.Status.Placement.Attempt)
}

func TestDeletionFinalizerWaitsForV2RuntimeDeletion(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current = seedReadyControllerAssignment(t, reconciler, current, placementStatus)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	fastlet.runtimes[string(current.UID)] = "running"
	require.NoError(t, reconciler.Delete(context.Background(), current))

	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	fastlet.mu.Lock()
	require.Equal(t, 1, fastlet.deleteCall)
	fastlet.mu.Unlock()
	_, err = reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	var deleted apiv1alpha2.Sandbox
	err = reconciler.Get(context.Background(), client.ObjectKeyFromObject(current), &deleted)
	require.True(t, apierrors.IsNotFound(err))
}

func TestResetDeletesOldRuntimeThenAdvancesGeneration(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	resetAt := metav1.NewTime(time.Now().Add(time.Minute))
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Spec.ResetRevision = &resetAt
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current = seedReadyControllerAssignment(t, reconciler, current, placementStatus)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	fastlet.runtimes[string(current.UID)] = "running"

	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Empty(t, current.Status.Placement.FastletName)
	require.Equal(t, int64(2), current.Status.Runtime.Generation)
	require.NotNil(t, current.Status.Runtime.AcceptedResetRevision)
	require.Equal(t, resetAt.Unix(), current.Status.Runtime.AcceptedResetRevision.Unix())
}

func TestExpiredSandboxRecoversWhenExpireTimeIsExtended(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reconciler.Now = func() time.Time { return now }
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	expiredAt := metav1.NewTime(now.Add(-time.Minute))
	current.Spec.ExpireTime = &expiredAt
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = seedReadyControllerAssignment(t, reconciler, getControllerSandbox(t, reconciler, sandbox.Name), placementStatus)
	fastlet.runtimes[string(current.UID)] = "running"

	// First pass requests deletion; the second observes the runtime gone and
	// commits the stopped state with the next instance generation.
	reconcileTwice(t, reconciler, sandbox.Name)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Empty(t, current.Status.Placement.FastletName)
	require.Equal(t, apiv1alpha2.RuntimeStopped, current.Status.Runtime.State)
	require.Equal(t, int64(2), current.Status.Runtime.Generation)

	extended := metav1.NewTime(now.Add(time.Hour))
	current.Spec.ExpireTime = &extended
	require.NoError(t, reconciler.Update(context.Background(), current))
	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Equal(t, apiv1alpha2.RuntimeReady, current.Status.Runtime.State)
	require.Equal(t, int64(2), current.Status.Runtime.Generation)
	require.NotEmpty(t, current.Status.Placement.FastletName)
}

func TestExpiredSandboxRecoversWithNewerResetRevision(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reconciler.Now = func() time.Time { return now }
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	expiredAt := metav1.NewTime(now.Add(-time.Minute))
	current.Spec.ExpireTime = &expiredAt
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = seedReadyControllerAssignment(t, reconciler, getControllerSandbox(t, reconciler, sandbox.Name), placementStatus)
	fastlet.runtimes[string(current.UID)] = "running"
	reconcileTwice(t, reconciler, sandbox.Name)

	current = getControllerSandbox(t, reconciler, sandbox.Name)
	resetAt := metav1.NewTime(now.Add(time.Minute))
	current.Spec.ResetRevision = &resetAt
	require.NoError(t, reconciler.Update(context.Background(), current))
	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotNil(t, current.Status.Runtime.AcceptedResetRevision)
	require.Equal(t, resetAt.Unix(), current.Status.Runtime.AcceptedResetRevision.Unix())
	require.Equal(t, apiv1alpha2.RuntimeReady, current.Status.Runtime.State)
	require.Equal(t, int64(2), current.Status.Runtime.Generation)
}

func TestReadyConditionReasonIsNeverUsedAsControllerState(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := seedReadyControllerAssignment(t, reconciler, getControllerSandbox(t, reconciler, sandbox.Name), placementStatus)
	fastlet.runtimes[string(current.UID)] = "running"
	condition := metav1.Condition{
		Type: apiv1alpha2.SandboxConditionReady, Status: metav1.ConditionFalse,
		Reason: orchestration.ReasonExpired, Message: "stale external diagnostic", LastTransitionTime: metav1.Now(),
	}
	current.Status.Conditions = []metav1.Condition{condition}
	require.NoError(t, reconciler.Status().Update(context.Background(), current))

	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotEmpty(t, current.Status.Placement.FastletName)
	require.Equal(t, apiv1alpha2.RuntimeReady, current.Status.Runtime.State)
}

func newControllerHarness(t *testing.T) (*SandboxReconciler, *controllerRegistry, *controllerFastlet, *apiv1alpha2.Sandbox) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, Capacity: apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: 8,
			SandboxResources: apiv1alpha2.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("512Mi"), PIDs: 256,
			},
			FastletTemplate: corev1.PodTemplateSpec{},
		},
	}
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("sandbox-uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	fastletPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "fastlet-a", Namespace: "default", UID: types.UID("pod-a"), Labels: map[string]string{"app": "sandbox-fastlet"},
	}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha2.Sandbox{}).WithObjects(pool, sandbox, fastletPod).Build()
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "container-runtime-profile-v1",
		ResourceProfileHash: pool.Spec.SandboxResources.Hash(), InfraRevision: "infra-minimal-v1", InfraReady: true,
	}
	registry := &controllerRegistry{
		candidates: []placement.FastletInfo{candidate},
		fastlets:   map[placement.FastletID]placement.FastletInfo{"fastlet-a": candidate},
	}
	fastlet := &controllerFastlet{runtimes: make(map[string]string)}
	orchestrator := &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	reconciler := &SandboxReconciler{Client: k8sClient, Scheme: scheme, Orchestrator: orchestrator}
	return reconciler, registry, fastlet, sandbox
}

func readyControllerStatus(placementStatus apiv1alpha2.PlacementStatus) apiv1alpha2.SandboxStatus {
	return apiv1alpha2.SandboxStatus{
		Placement: placementStatus,
		Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
		DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 1},
	}
}

func seedReadyControllerAssignment(t *testing.T, reconciler *SandboxReconciler, current *apiv1alpha2.Sandbox, placementStatus apiv1alpha2.PlacementStatus) *apiv1alpha2.Sandbox {
	t.Helper()
	var pool apiv1alpha2.SandboxPool
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: current.Namespace, Name: current.Spec.PoolRef}, &pool))
	envelope := assignment.AssignmentEnvelope{
		Version:     assignment.AssignmentEnvelopeVersion,
		FastletName: placementStatus.FastletName, FastletPodUID: string(placementStatus.FastletPodUID), NodeName: "node-a",
		Attempt: placementStatus.Attempt, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-a",
		RuntimeProfileHash: "container-runtime-profile-v1", ResourceProfileHash: pool.Spec.SandboxResources.Hash(), InfraRevision: "infra-minimal-v1",
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(current, envelope))
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, current.Name)
	current.Status = readyControllerStatus(placementStatus)
	require.NoError(t, reconciler.Status().Update(context.Background(), current))
	return getControllerSandbox(t, reconciler, current.Name)
}

func reconcileTwice(t *testing.T, reconciler *SandboxReconciler, name string) {
	t.Helper()
	_, err := reconciler.Reconcile(context.Background(), requestFor(name))
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), requestFor(name))
	require.NoError(t, err)
}

func requestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getControllerSandbox(t *testing.T, reconciler *SandboxReconciler, name string) *apiv1alpha2.Sandbox {
	t.Helper()
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &sandbox))
	return &sandbox
}
