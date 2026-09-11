package reconciler

import (
	"context"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type snapshotHarness struct {
	reconciler *SandboxSnapshotReconciler
	fastlet    *controllerFastlet
	k8sClient  client.Client
}

func newSnapshotReconcilerHarness(t *testing.T) (*snapshotHarness, *apiv1alpha2.SandboxSnapshot) {
	t.Helper()
	snapshot := &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
		Spec: apiv1alpha2.SandboxSnapshotSpec{
			SandboxRef:   apiv1alpha2.SandboxRef{Name: "sandbox-a", Namespace: "default", UID: "sandbox-uid-a"},
			TemplateName: "app-v2",
		},
	}
	return newSnapshotReconcilerHarnessWith(t, snapshot)
}

func newSnapshotReconcilerHarnessWith(t *testing.T, snapshot *apiv1alpha2.SandboxSnapshot) (*snapshotHarness, *apiv1alpha2.SandboxSnapshot) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	sandbox := snapshotTargetSandbox(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.Sandbox{}, &apiv1alpha2.SandboxSnapshot{}).
		WithObjects(sandbox, snapshot).Build()
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-hash",
		ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash", InfraReady: true,
	}
	registry := &controllerRegistry{
		candidates: []placement.FastletInfo{candidate},
		fastlets:   map[placement.FastletID]placement.FastletInfo{"fastlet-a": candidate},
	}
	fastlet := &controllerFastlet{runtimes: make(map[string]string)}
	orchestrator := &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	reconciler := &SandboxSnapshotReconciler{Client: k8sClient, Scheme: scheme, Orchestrator: orchestrator}
	return &snapshotHarness{reconciler: reconciler, fastlet: fastlet, k8sClient: k8sClient}, snapshot
}

// agedSnapshotCopy returns the harness snapshot seeded with the given
// creation age and admitted state.
func agedSnapshot(admitted bool, age time.Duration) *apiv1alpha2.SandboxSnapshot {
	snapshot := &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a"),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: apiv1alpha2.SandboxSnapshotSpec{
			SandboxRef:   apiv1alpha2.SandboxRef{Name: "sandbox-a", Namespace: "default", UID: "sandbox-uid-a"},
			TemplateName: "app-v2",
		},
	}
	if admitted {
		snapshot.Status.SnapshotID = "snap-known"
		snapshot.Status.Phase = apiv1alpha2.SandboxSnapshotPhaseCreating
	}
	return snapshot
}

func TestSnapshotPendingDeadlineFailsUnadmittedFence(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarnessWith(t, agedSnapshot(false, 11*time.Minute))
	harness.reconciler.Now = func() time.Time { return time.Now() }

	// Target Sandbox stays not-Ready: without the deadline this would retry
	// forever and hold the reentrancy fence indefinitely.
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Status.Runtime.State = apiv1alpha2.RuntimeCreating
	require.NoError(t, harness.reconciler.Status().Update(context.Background(), &sandbox))

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, current.Status.Phase)
	require.Equal(t, "SnapshotDeadlineExceeded", snapshotCompletedCondition(t, current).Reason)
	require.NotNil(t, current.Status.CompletedAt, "the fence is released with a terminal phase")
}

func TestSnapshotActiveDeadlineFailsLostAdmittedTask(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarnessWith(t, agedSnapshot(true, 46*time.Minute))
	harness.reconciler.Now = func() time.Time { return time.Now() }
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectPhase = fastletapi.SnapshotPhaseCreating
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, current.Status.Phase)
	require.Equal(t, "SnapshotDeadlineExceeded", snapshotCompletedCondition(t, current).Reason)
}

func TestSnapshotDeadlineDoesNotTerminateFreshSnapshots(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarnessWith(t, agedSnapshot(true, time.Minute))
	harness.reconciler.Now = func() time.Time { return time.Now() }

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, current.Status.Phase)
}

func TestSnapshotDeletionUnwedgesPastDeadline(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarnessWith(t, agedSnapshot(true, 46*time.Minute))
	harness.reconciler.Now = func() time.Time { return time.Now() }
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectPhase = fastletapi.SnapshotPhaseCreating
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
	}))

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue, "delegation past the deadline must not wait forever")
	var current apiv1alpha2.SandboxSnapshot
	require.Error(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &current),
		"finalizer removal lets the wedged object disappear")
}

func snapshotTargetSandbox(t *testing.T) *apiv1alpha2.Sandbox {
	t.Helper()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("sandbox-uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	envelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a", NodeName: "node-a",
		Attempt: 1, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-a",
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = apiv1alpha2.SandboxStatus{
		Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1},
		Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
		DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 1},
	}
	return sandbox
}

func snapshotRequestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getSnapshot(t *testing.T, harness *snapshotHarness, name string) *apiv1alpha2.SandboxSnapshot {
	t.Helper()
	var current apiv1alpha2.SandboxSnapshot
	require.NoError(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &current))
	return &current
}

func snapshotCompletedCondition(t *testing.T, snapshot *apiv1alpha2.SandboxSnapshot) *metav1.Condition {
	t.Helper()
	for index := range snapshot.Status.Conditions {
		if snapshot.Status.Conditions[index].Type == apiv1alpha2.SandboxSnapshotConditionCompleted {
			return &snapshot.Status.Conditions[index]
		}
	}
	t.Fatalf("Completed condition is missing")
	return nil
}

func TestSnapshotReconcileAddsFinalizerThenConverges(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.True(t, result.Requeue)
	first := getSnapshot(t, harness, "snap-a")
	require.Contains(t, first.Finalizers, SandboxSnapshotFinalizerName)

	result, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, SnapshotObservationPollInterval, result.RequeueAfter)
	second := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseCreating, second.Status.Phase)
	require.Equal(t, "fastlet-a", second.Status.FastletName)
	require.NotEmpty(t, second.Status.SnapshotID)

	result, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue, "terminal snapshots stop reconciling")
	final := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, final.Status.Phase)
	require.Equal(t, "s3://bucket/publish/abc123/manifest.json", final.Status.ManifestRef)
	require.Equal(t, int64(42), final.Status.SizeBytes)
	require.Equal(t, types.UID("sandbox-uid-a"), final.Status.SandboxUID)
	require.NotNil(t, final.Status.CompletedAt)
	require.Equal(t, metav1.ConditionTrue, snapshotCompletedCondition(t, final).Status)
}

func TestSnapshotReconcileFailsWhenTargetMissing(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), snapshotTargetSandbox(t)))

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, current.Status.Phase)
	require.Equal(t, "SandboxNotFound", snapshotCompletedCondition(t, current).Reason)
}

func TestSnapshotReconcileDeterministicRejectionFails(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotCreateErr = &fastletapi.FastletError{Code: fastletapi.ErrorSnapshotInProgress, Message: "busy"}
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, current.Status.Phase)
	require.Equal(t, "SnapshotInProgress", snapshotCompletedCondition(t, current).Reason)
}

func TestSnapshotReconcileDrainingIsTransientNotFailed(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotCreateErr = &fastletapi.FastletError{Code: fastletapi.ErrorDraining, Message: "rollout in progress"}
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, SnapshotRetryInterval, result.RequeueAfter, "draining keeps the intent Pending")
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhasePending, current.Status.Phase)
	for _, condition := range current.Status.Conditions {
		require.NotEqual(t, apiv1alpha2.SandboxSnapshotConditionCompleted, condition.Type,
			"a transient drain must not terminate the one-shot snapshot")
	}
}

func TestSnapshotFailedReasonFlowsToCondition(t *testing.T) {
	// The fastlet observation is a pure projection: a Failed task carrying a
	// stable reason must surface it verbatim in the Completed condition.
	status := &apiv1alpha2.SandboxSnapshotStatus{}
	orchestration.ProjectSnapshotStatus(status, &fastletapi.SnapshotStatus{
		Phase: fastletapi.SnapshotPhaseFailed, Reason: "InsufficientStorage",
		Message: "staging needs X bytes, Y free",
	})
	condition := status.Conditions[len(status.Conditions)-1]
	require.Equal(t, "InsufficientStorage", condition.Reason)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
}

func TestSnapshotReconcileLostTaskFails(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)

	current := getSnapshot(t, harness, "snap-a")
	current.Status.SnapshotID = "snap-known"
	current.Status.Phase = apiv1alpha2.SandboxSnapshotPhaseCreating
	require.NoError(t, harness.reconciler.Status().Update(context.Background(), current))

	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectErr = &fastletapi.FastletError{Code: fastletapi.ErrorNotFound, Message: "gone"}
	harness.fastlet.mu.Unlock()

	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	final := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, final.Status.Phase)
	require.Equal(t, "SnapshotLost", snapshotCompletedCondition(t, final).Reason)
}

func TestSnapshotDeletionWaitsForRunningDump(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectPhase = fastletapi.SnapshotPhaseCreating
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
	}))

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, SnapshotObservationPollInterval, result.RequeueAfter, "deletion waits for the running dump")
	current := getSnapshot(t, harness, "snap-a")
	require.Contains(t, current.Finalizers, SandboxSnapshotFinalizerName)
}

func TestSnapshotDeletionCleansUpAfterTerminal(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
	}))

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue)

	harness.fastlet.mu.Lock()
	deleteCalls := harness.fastlet.snapshotDeleteCall
	harness.fastlet.mu.Unlock()
	require.Equal(t, 1, deleteCalls, "node-local artifacts are cleaned up best-effort")

	var current apiv1alpha2.SandboxSnapshot
	require.Error(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &current),
		"finalizer removal lets the object disappear")
}

func TestSnapshotNotReadyTargetStaysPendingWithRequeue(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Status.Runtime.State = apiv1alpha2.RuntimeCreating
	require.NoError(t, harness.reconciler.Status().Update(context.Background(), &sandbox))

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, SnapshotRetryInterval, result.RequeueAfter, "a not-Ready target backs off without terminating")
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhasePending, current.Status.Phase)
	require.Contains(t, current.Status.Message, "Creating")
	require.Empty(t, current.Status.SnapshotID, "no trigger happened while the target is not Ready")
}

func TestSnapshotTerminalPhaseIsMonotonic(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, getSnapshot(t, harness, "snap-a").Status.Phase)

	// A stale in-flight observation (fastlet briefly reports Creating again)
	// must not roll the terminal phase back.
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectPhase = fastletapi.SnapshotPhaseCreating
	harness.fastlet.mu.Unlock()
	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue)
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, getSnapshot(t, harness, "snap-a").Status.Phase)
}

func TestSnapshotDeletionAfterSuccessWithTargetGone(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	succeeded := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, succeeded.Status.Phase)

	// The target Sandbox disappears before the object is deleted: the
	// terminal phase must survive the deletion reconcile untouched.
	require.NoError(t, harness.k8sClient.Delete(context.Background(), snapshotTargetSandbox(t)))
	require.NoError(t, harness.k8sClient.Delete(context.Background(), &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
	}))
	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue, "deletion of a terminal snapshot completes even without a target")
	var current apiv1alpha2.SandboxSnapshot
	require.Error(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &current))
}

func TestSnapshotObservePinnedAcrossReassignment(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	triggered := getSnapshot(t, harness, "snap-a").Status.Triggered
	require.NotNil(t, triggered)
	require.Equal(t, int64(1), triggered.AssignmentAttempt)
	require.Equal(t, "runtime-a", triggered.RuntimeInstanceID)

	// Reassign the Sandbox to another fastlet with a fresh fence; the
	// observation must keep resolving the pinned placement (attempt 1,
	// fastlet-a) instead of drifting to the new assignment.
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	newEnvelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-b", FastletPodUID: "pod-b", NodeName: "node-b",
		Attempt: 2, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-b",
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(&sandbox, newEnvelope))
	require.NoError(t, harness.reconciler.Update(context.Background(), &sandbox))
	// Keep the status projection consistent with the new annotation
	// (EffectiveAssignment fails closed on a conflict).
	require.NoError(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Status.Placement = apiv1alpha2.PlacementStatus{FastletName: "fastlet-b", FastletPodUID: "pod-b", Attempt: 2}
	require.NoError(t, harness.reconciler.Status().Update(context.Background(), &sandbox))

	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	harness.fastlet.mu.Lock()
	lastIdentity := harness.fastlet.lastSnapshotInspect
	harness.fastlet.mu.Unlock()
	require.NotNil(t, lastIdentity, "the pinned fastlet was observed despite the reassignment")
	require.Equal(t, int64(1), lastIdentity.Sandbox.AssignmentAttempt, "the observation replays the trigger-time fence")
	require.Equal(t, "runtime-a", lastIdentity.Sandbox.RuntimeInstanceID)
	require.Equal(t, "pod-a", lastIdentity.Sandbox.FastletPodUID)
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, getSnapshot(t, harness, "snap-a").Status.Phase)
}
