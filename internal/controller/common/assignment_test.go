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
		FastletName: "fastlet-a", FastletPodUID: "pod-uid-a", NodeName: "node-a", Attempt: 1,
	}

	assigned, err := EnsureSandboxAssignment(context.Background(), k8sClient, key, desired)
	require.NoError(t, err)
	require.Equal(t, &desired, assigned.Status.Assignment)
	require.Equal(t, int64(1), assigned.Status.InstanceGeneration)
	require.Equal(t, desired.FastletName, assigned.Status.AssignedFastlet)

	again, err := EnsureSandboxAssignment(context.Background(), k8sClient, key, desired)
	require.NoError(t, err)
	require.Equal(t, assigned.Status.Assignment, again.Status.Assignment)

	conflicting := desired
	conflicting.FastletName = "fastlet-b"
	conflicting.FastletPodUID = "pod-uid-b"
	conflicting.Attempt = 2
	_, err = EnsureSandboxAssignment(context.Background(), k8sClient, key, conflicting)
	require.ErrorIs(t, err, ErrAssignmentConflict)
}
