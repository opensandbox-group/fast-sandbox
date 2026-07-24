package reconciler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
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
	mu          sync.Mutex
	ensureErr   error
	inspectErr  error
	ensurePhase string
	runtimes    map[string]string
	ensureCall  int
	deleteCall  int
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
	return &fastletapi.CreateSandboxResponse{Accepted: true, Sandbox: &fastletapi.SandboxStatus{SandboxID: request.Identity.SandboxUID, Phase: phase}}, nil
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
	return &fastletapi.InspectSandboxResponse{Sandbox: &fastletapi.SandboxStatus{SandboxID: request.Identity.SandboxUID, Phase: phase}}, nil
}

func (f *controllerFastlet) DeleteSandboxV2(_ context.Context, _ string, request *fastletapi.DeleteSandboxV2Request) (*fastletapi.DeleteSandboxV2Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCall++
	delete(f.runtimes, request.Identity.SandboxUID)
	return &fastletapi.DeleteSandboxV2Response{Accepted: true}, nil
}

func TestDeclarativeCreateWithoutCapacityStaysPending(t *testing.T) {
	reconciler, registry, _, sandbox := newControllerHarness(t)
	registry.candidates = nil
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.Nil(t, current.Status.Assignment)
	require.Equal(t, apiv1alpha1.ObservedStatePending, current.Status.RuntimeState)
}

func TestDeclarativeCreateUsesSharedV2Orchestrator(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotNil(t, current.Status.Assignment)
	require.Equal(t, "pod-a", current.Status.Assignment.FastletPodUID)
	require.Equal(t, int64(1), current.Status.AssignmentAttempt)
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.DataPlaneState)
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
	require.Equal(t, DataPlanePollInterval, result.RequeueAfter)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStateCreating, current.Status.DataPlaneState)

	fastlet.ensurePhase = "running"
	result, err = reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	require.Equal(t, DefaultRequeueInterval, result.RequeueAfter)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Equal(t, apiv1alpha1.ObservedStateReady, current.Status.DataPlaneState)
}

func TestExplicitCapacityRejectionPreservesDurableAssignmentAndAttemptFence(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	failure := &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "full", Retryable: true, Outcome: fastletapi.OutcomeRejectedBeforeSideEffects}
	fastlet.ensureErr = failure
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotNil(t, current.Status.Assignment)
	require.Equal(t, "fastlet-a", current.Status.Assignment.FastletName)
	require.NotEmpty(t, current.Annotations["sandbox.fast.io/assignment"])
	require.Equal(t, int64(1), current.Status.AssignmentAttempt)
	require.Equal(t, apiv1alpha1.ObservedStatePending, current.Status.RuntimeState)
}

func TestUnknownOutcomePreservesDurableAssignment(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	fastlet.ensureErr = errors.New("response lost")
	fastlet.inspectErr = errors.New("connection unavailable")
	reconcileTwice(t, reconciler, sandbox.Name)
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotNil(t, current.Status.Assignment)
	require.Equal(t, "fastlet-a", current.Status.Assignment.FastletName)
}

func TestPodLostPolicyManualAndAutoRecreate(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		policy apiv1alpha1.FailurePolicy
		auto   bool
	}{
		{name: "manual", policy: apiv1alpha1.FailurePolicyManual},
		{name: "auto", policy: apiv1alpha1.FailurePolicyAutoRecreate, auto: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reconciler, registry, _, sandbox := newControllerHarness(t)
			assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
			current := getControllerSandbox(t, reconciler, sandbox.Name)
			current.Spec.FailurePolicy = testCase.policy
			require.NoError(t, reconciler.Update(context.Background(), current))
			current = getControllerSandbox(t, reconciler, sandbox.Name)
			current.Status = readyControllerStatus(&assignment)
			require.NoError(t, reconciler.Status().Update(context.Background(), current))
			var fastletPod corev1.Pod
			require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-a"}, &fastletPod))
			require.NoError(t, reconciler.Delete(context.Background(), &fastletPod))
			registry.fastlets = map[placement.FastletID]placement.FastletInfo{}
			reconcileTwice(t, reconciler, sandbox.Name)
			current = getControllerSandbox(t, reconciler, sandbox.Name)
			if testCase.auto {
				require.Nil(t, current.Status.Assignment)
				require.Equal(t, int64(2), current.Status.InstanceGeneration)
			} else {
				require.NotNil(t, current.Status.Assignment)
				require.Equal(t, apiv1alpha1.ObservedStateUnavailable, current.Status.RuntimeState)
				require.True(t, current.Status.HasCondition(apiv1alpha1.SandboxConditionRuntimeReady, metav1.ConditionFalse, orchestration.ReasonFastletPodLost))
			}
		})
	}
}

func TestRegistryMissDoesNotMeanFastletPodLost(t *testing.T) {
	reconciler, registry, _, sandbox := newControllerHarness(t)
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Spec.FailurePolicy = apiv1alpha1.FailurePolicyAutoRecreate
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current.Status = readyControllerStatus(&assignment)
	require.NoError(t, reconciler.Status().Update(context.Background(), current))
	registry.fastlets = map[placement.FastletID]placement.FastletInfo{}

	reconcileTwice(t, reconciler, sandbox.Name)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.NotNil(t, current.Status.Assignment)
	require.Equal(t, int64(1), current.Status.InstanceGeneration)
	require.Equal(t, apiv1alpha1.ObservedStateUnavailable, current.Status.RuntimeState)
	require.False(t, current.Status.HasCondition(apiv1alpha1.SandboxConditionRuntimeReady, metav1.ConditionFalse, orchestration.ReasonFastletPodLost))
}

func TestReplacementPodWithSameNameCannotClaimOldAssignment(t *testing.T) {
	reconciler, registry, _, sandbox := newControllerHarness(t)
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Spec.FailurePolicy = apiv1alpha1.FailurePolicyAutoRecreate
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current.Status = readyControllerStatus(&assignment)
	require.NoError(t, reconciler.Status().Update(context.Background(), current))
	var oldPod corev1.Pod
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-a"}, &oldPod))
	require.NoError(t, reconciler.Delete(context.Background(), &oldPod))
	replacement := oldPod.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("pod-b")
	require.NoError(t, reconciler.Create(context.Background(), replacement))
	registry.fastlets = map[placement.FastletID]placement.FastletInfo{}

	reconcileTwice(t, reconciler, sandbox.Name)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Nil(t, current.Status.Assignment)
	require.Equal(t, int64(2), current.Status.InstanceGeneration)
	require.Equal(t, int64(1), current.Status.AssignmentAttempt)
}

func TestDeletionFinalizerWaitsForV2RuntimeDeletion(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current.Status = readyControllerStatus(&assignment)
	require.NoError(t, reconciler.Status().Update(context.Background(), current))
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
	var deleted apiv1alpha1.Sandbox
	err = reconciler.Get(context.Background(), client.ObjectKeyFromObject(current), &deleted)
	require.True(t, apierrors.IsNotFound(err))
}

func TestResetDeletesOldRuntimeThenAdvancesGeneration(t *testing.T) {
	reconciler, _, fastlet, sandbox := newControllerHarness(t)
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	resetAt := metav1.NewTime(time.Now().Add(time.Minute))
	current := getControllerSandbox(t, reconciler, sandbox.Name)
	current.Spec.ResetRevision = &resetAt
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	current.Status = readyControllerStatus(&assignment)
	require.NoError(t, reconciler.Status().Update(context.Background(), current))
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	fastlet.runtimes[string(current.UID)] = "running"

	_, err := reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), requestFor(sandbox.Name))
	require.NoError(t, err)
	current = getControllerSandbox(t, reconciler, sandbox.Name)
	require.Nil(t, current.Status.Assignment)
	require.Equal(t, int64(2), current.Status.InstanceGeneration)
	require.NotNil(t, current.Status.AcceptedResetRevision)
	require.Equal(t, resetAt.Unix(), current.Status.AcceptedResetRevision.Unix())
}

func newControllerHarness(t *testing.T) (*SandboxReconciler, *controllerRegistry, *controllerFastlet, *apiv1alpha1.Sandbox) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeContainer, Capacity: apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: 8,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("512Mi"), PIDs: 256,
			},
			FastletTemplate: corev1.PodTemplateSpec{},
		},
	}
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("sandbox-uid-a")},
		Spec:       apiv1alpha1.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	fastletPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "fastlet-a", Namespace: "default", UID: types.UID("pod-a"), Labels: map[string]string{"app": "sandbox-fastlet"},
	}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha1.Sandbox{}).WithObjects(pool, sandbox, fastletPod).Build()
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeName: apiv1alpha1.RuntimeContainer, RuntimeProfileHash: "container-runtime-profile-v1",
		ResourceProfileHash: pool.Spec.SandboxResources.Hash(), InfraProfile: "minimal", InfraProfileHash: "infra-minimal-v1", InfraReady: true,
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

func readyControllerStatus(assignment *apiv1alpha1.SandboxAssignment) apiv1alpha1.SandboxStatus {
	return apiv1alpha1.SandboxStatus{
		Assignment: assignment, AssignmentAttempt: assignment.Attempt, InstanceGeneration: 1,
		RuntimeState: apiv1alpha1.ObservedStateReady, DataPlaneState: apiv1alpha1.ObservedStateReady,
	}
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

func getControllerSandbox(t *testing.T, reconciler *SandboxReconciler, name string) *apiv1alpha1.Sandbox {
	t.Helper()
	var sandbox apiv1alpha1.Sandbox
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &sandbox))
	return &sandbox
}
