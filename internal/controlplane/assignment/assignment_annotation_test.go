package assignment

import (
	"context"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testAssignmentEnvelope() AssignmentEnvelope {
	return AssignmentEnvelope{
		Version: AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a", NodeName: "node-a",
		Attempt: 1, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-a",
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraProfileHash: "infra-hash",
	}
}

func TestEffectiveAssignmentUsesAnnotationBeforeStatusProjection(t *testing.T) {
	sandbox := &apiv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"}}
	want := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, want))

	got, err := EffectiveAssignment(sandbox)
	require.NoError(t, err)
	require.Equal(t, want, *got)
}

func TestEffectiveAssignmentFailsClosedOnProjectionMismatch(t *testing.T) {
	sandbox := &apiv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"}}
	envelope := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, envelope))
	wrong := envelope.StatusAssignment()
	wrong.FastletPodUID = "pod-b"
	sandbox.Status = apiv1alpha1.SandboxStatus{
		Assignment: &wrong, AssignmentAttempt: envelope.Attempt,
		InstanceGeneration: envelope.InstanceGeneration, RouteGeneration: envelope.RouteGeneration,
	}

	_, err := EffectiveAssignment(sandbox)
	require.ErrorIs(t, err, ErrAssignmentProjectionConflict)
}

func TestProjectAssignmentToStatusAndCASReassignment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec:       apiv1alpha1.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	first := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, first))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha1.Sandbox{}).WithObjects(sandbox).Build()

	projected, err := ProjectAssignmentToStatus(context.Background(), k8sClient, types.NamespacedName{Namespace: "default", Name: "sandbox-a"})
	require.NoError(t, err)
	require.Equal(t, first.StatusAssignment(), *projected.Status.Assignment)
	require.Equal(t, first.InstanceGeneration, projected.Status.InstanceGeneration)

	second := first
	second.FastletName, second.FastletPodUID, second.NodeName = "fastlet-b", "pod-b", "node-b"
	second.Attempt, second.RouteGeneration, second.RuntimeInstanceID = 2, 2, "runtime-b"
	updated, err := CASAssignmentAnnotation(context.Background(), k8sClient, types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, first, second)
	require.NoError(t, err)

	_, err = EffectiveAssignment(updated)
	require.ErrorIs(t, err, ErrAssignmentProjectionConflict, "status must fail closed until the new annotation is projected")

	projected, err = ProjectAssignmentToStatus(context.Background(), k8sClient, types.NamespacedName{Namespace: "default", Name: "sandbox-a"})
	require.NoError(t, err)
	effective, err := EffectiveAssignment(projected)
	require.NoError(t, err)
	require.Equal(t, second, *effective)
}

func TestEffectiveAssignmentRejectsStatusOnlyPlacement(t *testing.T) {
	assignment := testAssignmentEnvelope().StatusAssignment()
	sandbox := &apiv1alpha1.Sandbox{Status: apiv1alpha1.SandboxStatus{Assignment: &assignment}}
	_, err := EffectiveAssignment(sandbox)
	require.ErrorIs(t, err, ErrAssignmentAnnotationMissing)
}
