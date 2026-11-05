package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func seedDurableTerminalControllerAssignment(t *testing.T) (*SandboxReconciler, *controllerRegistry, *controllerFastlet, *apiv1alpha2.Sandbox) {
	t.Helper()
	reconciler, registry, fastlet, original := newControllerHarness(t)
	current := getControllerSandbox(t, reconciler, original.Name)
	current.Finalizers = []string{FinalizerName}
	require.NoError(t, reconciler.Update(t.Context(), current))
	current = seedReadyControllerAssignment(t, reconciler, getControllerSandbox(t, reconciler, original.Name), apiv1alpha2.PlacementStatus{
		FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1,
	})
	fastlet.runtimes[string(current.UID)] = "failed"
	_, err := reconciler.Reconcile(t.Context(), requestFor(original.Name))
	require.NoError(t, err)
	current = getControllerSandbox(t, reconciler, original.Name)
	require.True(t, terminalRuntimeFailureRecorded(current))
	require.Zero(t, fastlet.deleteCall, "the Failed decision must be durable before acknowledgement")
	return reconciler, registry, fastlet, current
}

func TestTerminalRuntimeFailureAcknowledgementConvergesAfterPodLoss(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		stage := "before-acknowledgement"
		if acknowledged {
			stage = "after-acknowledgement"
		}
		for _, ownerState := range []string{"missing", "replaced", "present"} {
			t.Run(stage+"/"+ownerState, func(t *testing.T) {
				reconciler, registry, fastlet, failed := seedDurableTerminalControllerAssignment(t)
				if acknowledged {
					_, err := reconciler.Reconcile(t.Context(), requestFor(failed.Name))
					require.NoError(t, err)
					require.Empty(t, fastlet.runtimes)
				}
				var pod corev1.Pod
				key := types.NamespacedName{Namespace: failed.Namespace, Name: failed.Status.Placement.FastletName}
				require.NoError(t, reconciler.Get(t.Context(), key, &pod))
				if ownerState != "present" {
					require.NoError(t, reconciler.Delete(t.Context(), &pod))
					if ownerState == "replaced" {
						pod.UID, pod.ResourceVersion = "replacement-pod", ""
						require.NoError(t, reconciler.Create(t.Context(), &pod))
					}
				}
				registry.fastlets = nil
				for range 2 {
					// Every pass uses a new Controller process and the same durable
					// decision; a missing registry entry alone cannot finish cleanup.
					restarted := &SandboxReconciler{Client: reconciler.Client, Scheme: reconciler.Scheme, Orchestrator: reconciler.Orchestrator}
					result, err := restarted.Reconcile(t.Context(), requestFor(failed.Name))
					require.NoError(t, err)
					if ownerState == "present" {
						require.Positive(t, result.RequeueAfter)
					} else {
						require.Zero(t, result.RequeueAfter)
						require.False(t, result.Requeue)
					}
					current := getControllerSandbox(t, restarted, failed.Name)
					require.Equal(t, failed.Status, current.Status)
					require.Equal(t, failed.Annotations[assignment.AnnotationAssignment], current.Annotations[assignment.AnnotationAssignment])
					require.Zero(t, fastlet.ensureCall, "a terminal identity must never be recreated after owner loss")
				}
			})
		}
	}
}

type terminalDeleteRecorder struct {
	*controllerFastlet
	identity fastletapi.SandboxIdentity
}

func (f *terminalDeleteRecorder) DeleteSandbox(ctx context.Context, address string, request *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	f.identity = request.Identity
	return f.controllerFastlet.DeleteSandbox(ctx, address, request)
}

func TestTerminalRuntimeFailureAcknowledgementPreservesResolvedIdentity(t *testing.T) {
	reconciler, _, fastlet, failed := seedDurableTerminalControllerAssignment(t)
	old, err := assignment.AssignmentFromAnnotation(failed)
	require.NoError(t, err)
	current := failed.DeepCopy()
	next := *old
	next.InstanceGeneration++
	next.Attempt++
	next.RouteGeneration++
	next.RuntimeInstanceID = "replacement-runtime"
	require.NoError(t, assignment.SetAssignmentAnnotation(current, next))
	require.NoError(t, reconciler.Update(t.Context(), current))
	recorder := &terminalDeleteRecorder{controllerFastlet: fastlet}
	reconciler.Orchestrator.FastletClient = recorder

	// The next assignment wins after Reconcile has read the old Failed status.
	// A terminal acknowledgement must not project and delete that new identity.
	_, err = reconciler.reconcileTerminalRuntimeFailure(t.Context(), reconciler.Orchestrator, failed)
	require.NoError(t, err)
	require.Equal(t, old.InstanceGeneration, recorder.identity.InstanceGeneration)
	require.Equal(t, old.Attempt, recorder.identity.AssignmentAttempt)
	require.Equal(t, old.RuntimeInstanceID, recorder.identity.RuntimeInstanceID)
	current = getControllerSandbox(t, reconciler, failed.Name)
	require.Equal(t, failed.Status, current.Status)
	actual, err := assignment.AssignmentFromAnnotation(current)
	require.NoError(t, err)
	require.Equal(t, next, *actual)
}

func TestTerminalRuntimeFailureDoesNotReservePoolCapacity(t *testing.T) {
	reconciler, _, fastlet, failed := seedDurableTerminalControllerAssignment(t)
	_, err := reconciler.Reconcile(t.Context(), requestFor(failed.Name))
	require.NoError(t, err)
	require.Empty(t, fastlet.runtimes)
	sandboxes := []apiv1alpha2.Sandbox{*getControllerSandbox(t, reconciler, failed.Name)}
	identity := "fastlet-a/pod-a"
	now := time.Now()
	require.Zero(t, activeAssignmentsByPod(sandboxes, "pool-a")[identity])
	require.Zero(t, awaitingAdmissionByFastlet(sandboxes, nil, now)[identity])
	active, pending := poolSlotDemandCounts(sandboxes, "pool-a", map[string]struct{}{identity: {}})
	require.Zero(t, active)
	require.Zero(t, pending)

	pool := resourceAwareScalingPool()
	pool.Spec.Capacity.PoolMin = 0
	pool.Spec.Capacity.BufferMin, pool.Spec.Capacity.BufferMax = 0, 0
	pods := []corev1.Pod{*fastletPod("fastlet-a", "pod-a", "10.0.0.1")}
	info := resourceAwareControllerFastlet("fastlet-a", "pod-a", 0, fastletapi.AdmissionStateOpen, now)
	info.ResourceReceivedAt = now
	plan := (&SandboxPoolReconciler{}).planResourceAwareScaleWithDemand(
		pool, pods, sandboxes, nil, resourceAwareCommitmentCapacity(pool), map[string]placement.FastletInfo{identity: info}, nil, now,
	)
	require.Zero(t, plan.desiredPods)
	require.Equal(t, []string{identity}, plan.drainCandidates)
	current := getControllerSandbox(t, reconciler, failed.Name)
	require.Equal(t, failed.Status, current.Status)
	require.Equal(t, failed.Annotations[assignment.AnnotationAssignment], current.Annotations[assignment.AnnotationAssignment])
}

func TestTerminalRuntimeFailurePoolCountersRetainUnconfirmedAssignments(t *testing.T) {
	_, _, _, failed := seedDurableTerminalControllerAssignment(t)
	for _, testCase := range []struct {
		name   string
		change func(*apiv1alpha2.Sandbox)
	}{
		{name: "ready", change: func(s *apiv1alpha2.Sandbox) { s.Status.Runtime.State = apiv1alpha2.RuntimeReady }},
		{name: "another-generation", change: func(s *apiv1alpha2.Sandbox) { s.Status.Runtime.Generation++ }},
		{name: "another-attempt", change: func(s *apiv1alpha2.Sandbox) { s.Status.Placement.Attempt++ }},
		{name: "another-pod", change: func(s *apiv1alpha2.Sandbox) { s.Status.Placement.FastletPodUID = "replacement-pod" }},
		{name: "no-durable-assignment", change: func(s *apiv1alpha2.Sandbox) { delete(s.Annotations, assignment.AnnotationAssignment) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sandbox := failed.DeepCopy()
			testCase.change(sandbox)
			identity := sandbox.Status.Placement.FastletName + "/" + string(sandbox.Status.Placement.FastletPodUID)
			sandboxes := []apiv1alpha2.Sandbox{*sandbox}
			require.Equal(t, 1, activeAssignmentsByPod(sandboxes, "pool-a")[identity])
			require.Equal(t, 1, awaitingAdmissionByFastlet(sandboxes, nil, time.Now())[identity])
			active, pending := poolSlotDemandCounts(sandboxes, "pool-a", map[string]struct{}{identity: {}})
			require.Equal(t, int32(1), active)
			require.Zero(t, pending)
		})
	}
}

func TestTerminalRuntimeFailureDrainWaitsForResourceCleanup(t *testing.T) {
	_, _, _, failed := seedDurableTerminalControllerAssignment(t)
	for _, testCase := range []struct {
		name      string
		used      int
		committed string
		inFlight  int
		wantGone  bool
	}{
		{name: "cleanup-complete", committed: "0", wantGone: true},
		{name: "deleting-instance", used: 1, committed: "0"},
		{name: "committed-resources", committed: "1"},
		{name: "in-flight-admission", committed: "0", inFlight: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sandboxes := []apiv1alpha2.Sandbox{*failed}
			reconciler, k8sClient, drainer, pool := newDrainHarness(t, sandboxes)
			pool.Spec.Runtime = apiv1alpha2.RuntimeFirecracker
			pool.Spec.SandboxResources = resourceAwareScalingPool().Spec.SandboxResources
			pod := getFastletPod(t, k8sClient, "fastlet-a")
			pod.Annotations = map[string]string{
				placement.AnnotationDraining: "true", placement.AnnotationDrainID: "drain-attempt",
				placement.AnnotationDrainReason:    placement.DrainReasonPlannedUpgrade,
				placement.AnnotationDrainStartedAt: time.Now().Add(-24 * time.Hour).Format(time.RFC3339Nano),
			}
			require.NoError(t, k8sClient.Update(t.Context(), pod))
			drainer.respond = func(request *fastletapi.SetDrainingRequest) *fastletapi.SetDrainingResponse {
				return &fastletapi.SetDrainingResponse{
					Draining: true, DrainID: request.DrainID, FastletPodUID: request.FastletPodUID, RuntimeReady: true,
					Admission: &fastletapi.AdmissionStatus{
						Used: testCase.used, Deleting: testCase.used,
						Resources: &fastletapi.ResourceAdmissionStatus{Version: "v2", Epoch: "epoch", Committed: fastletapi.SandboxResourceVector{CPU: testCase.committed, Memory: "0"}},
					},
				}
			}
			observed := map[string]placement.FastletInfo{podIdentity(pod): {InFlightSlots: testCase.inFlight}}
			_, _, err := reconciler.reconcileDraining(t.Context(), pool, []corev1.Pod{*pod}, sandboxes, 0, "", true, nil, observed)
			require.NoError(t, err)
			err = k8sClient.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})
			if testCase.wantGone {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err, "a durable failure is not evidence of physical resource cleanup")
			}
			var current apiv1alpha2.Sandbox
			require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKeyFromObject(failed), &current))
			require.Equal(t, failed.Status, current.Status)
			require.Equal(t, failed.Annotations[assignment.AnnotationAssignment], current.Annotations[assignment.AnnotationAssignment])
		})
	}
}
