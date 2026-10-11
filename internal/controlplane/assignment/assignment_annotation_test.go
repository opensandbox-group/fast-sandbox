package assignment

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func testAssignmentEnvelope() AssignmentEnvelope {
	return AssignmentEnvelope{
		Version: AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a", NodeName: "node-a",
		Attempt: 1, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-a",
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
}

func TestEffectiveAssignmentUsesAnnotationBeforeStatusProjection(t *testing.T) {
	sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"}}
	want := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, want))

	got, err := EffectiveAssignment(sandbox)
	require.NoError(t, err)
	require.Equal(t, want, *got)
}

func TestEffectiveAssignmentFailsClosedOnProjectionMismatch(t *testing.T) {
	sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"}}
	envelope := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, envelope))
	wrong := envelope.StatusPlacement()
	wrong.FastletPodUID = "pod-b"
	sandbox.Status = apiv1alpha2.SandboxStatus{Placement: wrong,
		Runtime:   apiv1alpha2.RuntimeStatus{Generation: envelope.InstanceGeneration},
		DataPlane: apiv1alpha2.DataPlaneStatus{RouteGeneration: envelope.RouteGeneration}}

	_, err := EffectiveAssignment(sandbox)
	require.ErrorIs(t, err, ErrAssignmentProjectionConflict)
}

func TestProjectAssignmentToStatusAndCASReassignment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	first := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, first))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha2.Sandbox{}).WithObjects(sandbox).Build()

	projected, err := ProjectAssignmentToStatus(context.Background(), k8sClient, types.NamespacedName{Namespace: "default", Name: "sandbox-a"})
	require.NoError(t, err)
	require.Equal(t, first.StatusPlacement(), projected.Status.Placement)
	require.Equal(t, first.InstanceGeneration, projected.Status.Runtime.Generation)

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
	placement := testAssignmentEnvelope().StatusPlacement()
	sandbox := &apiv1alpha2.Sandbox{Status: apiv1alpha2.SandboxStatus{Placement: placement}}
	_, err := EffectiveAssignment(sandbox)
	require.ErrorIs(t, err, ErrAssignmentAnnotationMissing)
}

func TestCASAssignmentIgnoresUnrelatedMetadataRace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	first := testAssignmentEnvelope()
	require.NoError(t, SetAssignmentAnnotation(sandbox, first))
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha2.Sandbox{}).WithObjects(sandbox).Build()
	k8sClient := &metadataRaceClient{Client: base}

	second := first
	second.FastletName, second.FastletPodUID, second.NodeName = "fastlet-b", "pod-b", "node-b"
	second.Attempt, second.RouteGeneration, second.RuntimeInstanceID = 2, 2, "runtime-b"
	updated, err := CASAssignmentAnnotation(context.Background(), k8sClient, types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, first, second)
	require.NoError(t, err)
	require.Equal(t, []string{"sandbox.fast.io/cleanup"}, updated.Finalizers)
	envelope, err := AssignmentFromAnnotation(updated)
	require.NoError(t, err)
	require.Equal(t, second, *envelope)
}

type metadataRaceClient struct {
	client.Client
	injected bool
}

func (c *metadataRaceClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if !c.injected {
		c.injected = true
		var current apiv1alpha2.Sandbox
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), &current); err != nil {
			return err
		}
		current.Finalizers = append(current.Finalizers, "sandbox.fast.io/cleanup")
		if err := c.Client.Update(ctx, &current); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func TestCASStopsOnChangedAnnotationBeforeLaggingProjection(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	first := testAssignmentEnvelope()
	second := first
	second.Attempt, second.RouteGeneration, second.RuntimeInstanceID = 2, 2, "runtime-b"
	sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"}, Status: apiv1alpha2.SandboxStatus{Placement: first.StatusPlacement(), Runtime: apiv1alpha2.RuntimeStatus{Generation: 1}, DataPlane: apiv1alpha2.DataPlaneStatus{RouteGeneration: 1}}}
	require.NoError(t, SetAssignmentAnnotation(sandbox, second))
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&apiv1alpha2.Sandbox{}).WithObjects(sandbox).Build()
	_, err := CASAssignmentAnnotation(context.Background(), k8s, client.ObjectKeyFromObject(sandbox), first, second)
	require.ErrorIs(t, err, ErrAssignmentAnnotationChanged)
	_, err = CASAssignmentAnnotation(context.Background(), k8s, client.ObjectKeyFromObject(sandbox), second, first)
	require.ErrorIs(t, err, ErrAssignmentProjectionConflict)
}

func TestCASAssignmentRejectsMissingAuthoritativeAnnotation(t *testing.T) {
	for _, projected := range []bool{false, true} {
		name := "unassigned"
		wantErr := ErrAssignmentAnnotationChanged
		sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"}}
		if projected {
			name = "status only"
			wantErr = ErrAssignmentAnnotationMissing
			sandbox.Status.Placement = testAssignmentEnvelope().StatusPlacement()
		}
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, apiv1alpha2.AddToScheme(scheme))
			k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
			expected := testAssignmentEnvelope()
			next := expected
			next.Attempt++
			next.RouteGeneration++
			next.RuntimeInstanceID = "next-runtime"
			key := client.ObjectKeyFromObject(sandbox)
			_, err := CASAssignmentAnnotation(context.Background(), k8s, key, expected, next)
			require.ErrorIs(t, err, wantErr)
			var current apiv1alpha2.Sandbox
			require.NoError(t, k8s.Get(context.Background(), key, &current))
			require.Empty(t, current.Annotations[AnnotationAssignment])
		})
	}
}
