package common

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

func TestEnsureSandboxAssignmentIsIdempotentAndConflicts(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"},
		Spec:       apiv1alpha1.SandboxSpec{Image: "image:v1", PoolRef: "pool-a"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
	desired := apiv1alpha1.SandboxAssignment{
		FastletName: "fastlet-a", FastletPodUID: "pod-uid-a", NodeName: "node-a",
	}

	assigned, err := EnsureSandboxAssignment(context.Background(), k8sClient, key, desired)
	require.NoError(t, err)
	require.Equal(t, int64(1), assigned.Status.Assignment.Attempt)
	require.True(t, assignmentTargetEqual(desired, *assigned.Status.Assignment))
	require.Equal(t, int64(1), assigned.Status.InstanceGeneration)
	require.Equal(t, int64(1), assigned.Status.AssignmentAttempt)

	again, err := EnsureSandboxAssignment(context.Background(), k8sClient, key, desired)
	require.NoError(t, err)
	require.Equal(t, assigned.Status.Assignment, again.Status.Assignment)

	conflicting := desired
	conflicting.FastletName = "fastlet-b"
	conflicting.FastletPodUID = "pod-uid-b"
	_, err = EnsureSandboxAssignment(context.Background(), k8sClient, key, conflicting)
	require.ErrorIs(t, err, ErrAssignmentConflict)
}

func TestEnsureSandboxAssignmentAllocatesAttemptFromDurableHighWaterMark(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"},
		Status:     apiv1alpha1.SandboxStatus{AssignmentAttempt: 7},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	assigned, err := EnsureSandboxAssignment(context.Background(), k8sClient, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, apiv1alpha1.SandboxAssignment{
		FastletName: "fastlet-a", FastletPodUID: "pod-a",
	})
	require.NoError(t, err)
	require.Equal(t, int64(8), assigned.Status.Assignment.Attempt)
	require.Equal(t, int64(8), assigned.Status.AssignmentAttempt)
}

func TestClearSandboxAssignmentRetainsAttemptAndOptionallyAdvancesGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 3}
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"},
		Spec:       apiv1alpha1.SandboxSpec{Image: "image:v1", PoolRef: "pool-a"},
		Status: apiv1alpha1.SandboxStatus{
			Assignment: &assignment, AssignmentAttempt: 3, InstanceGeneration: 2, RouteGeneration: 4,
			RuntimeState: apiv1alpha1.ObservedStateReady, DataPlaneState: apiv1alpha1.ObservedStateReady,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()

	cleared, err := ClearSandboxAssignment(context.Background(), k8sClient, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, assignment, true)
	require.NoError(t, err)
	require.Nil(t, cleared.Status.Assignment)
	require.Equal(t, int64(3), cleared.Status.AssignmentAttempt)
	require.Equal(t, int64(3), cleared.Status.InstanceGeneration)
	require.Equal(t, int64(5), cleared.Status.RouteGeneration)
	require.Equal(t, apiv1alpha1.ObservedStatePending, cleared.Status.RuntimeState)
	require.Equal(t, apiv1alpha1.ObservedStatePending, cleared.Status.DataPlaneState)
}
