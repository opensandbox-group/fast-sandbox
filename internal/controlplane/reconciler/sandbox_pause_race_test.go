package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
)

func TestImmediatePauseAdvancesUnprojectedRuntime(t *testing.T) {
	for _, state := range []apiv1alpha2.RuntimeState{"", apiv1alpha2.RuntimePending, apiv1alpha2.RuntimeCreating, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.RuntimeResuming} {
		t.Run(string(state), func(t *testing.T) {
			r, _, f, sb := newControllerHarness(t)
			current := seedReadyControllerAssignment(t, r, sb, apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1})
			f.runtimes[string(current.UID)] = "running"
			current.Spec.State = apiv1alpha2.SandboxStatePaused
			require.NoError(t, r.Update(context.Background(), current))
			current = getControllerSandbox(t, r, sb.Name)
			current.Status.Runtime.State = state
			require.NoError(t, r.Status().Update(context.Background(), current))
			for i := 0; i < 6; i++ {
				_, err := r.Reconcile(context.Background(), requestFor(sb.Name))
				require.NoError(t, err)
			}
			current = getControllerSandbox(t, r, sb.Name)
			require.Equal(t, apiv1alpha2.RuntimePaused, current.Status.Runtime.State)
			require.NotNil(t, current.Status.Runtime.Checkpoint)
			require.Empty(t, current.Status.Placement.FastletName)
			require.Zero(t, f.ensureCall, "pause must observe the existing Fast-Path runtime")
		})
	}
}

func TestPausedPublicationFollowsAssignmentRelease(t *testing.T) {
	r, _, f, sb := newControllerHarness(t)
	current := seedReadyControllerAssignment(t, r, sb, apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1})
	f.runtimes[string(current.UID)] = "running"
	current.Spec.State = apiv1alpha2.SandboxStatePaused
	require.NoError(t, r.Update(context.Background(), current))
	base := r.Client
	blockRelease := true
	published := false
	wrapped := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*apiv1alpha2.Sandbox); ok && blockRelease {
				return errors.New("injected assignment cleanup failure")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			s, ok := obj.(*apiv1alpha2.Sandbox)
			if ok && name == "status" && s.Status.Runtime.State == apiv1alpha2.RuntimePaused && !published {
				var durable apiv1alpha2.Sandbox
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(s), &durable))
				active, err := assignment.AssignmentFromAnnotation(&durable)
				require.NoError(t, err)
				require.Nil(t, active, "PAUSED must not be visible with the deleted runtime assignment")
				require.Empty(t, durable.Status.Placement.FastletName)
				published = true
			}
			return c.SubResource(name).Update(ctx, obj, opts...)
		},
	})
	r.Client = wrapped
	r.Orchestrator.Client = wrapped
	var releaseErr error
	for i := 0; i < 5; i++ {
		_, releaseErr = r.Reconcile(context.Background(), requestFor(sb.Name))
		if releaseErr != nil {
			break
		}
	}
	require.ErrorContains(t, releaseErr, "injected assignment cleanup failure")
	require.False(t, published)
	current = getControllerSandbox(t, r, sb.Name)
	require.Equal(t, apiv1alpha2.RuntimePausing, current.Status.Runtime.State)
	require.NotNil(t, current.Status.Runtime.Checkpoint)
	blockRelease = false
	_, err := r.Reconcile(context.Background(), requestFor(sb.Name))
	require.NoError(t, err)
	require.True(t, published)
	current = getControllerSandbox(t, r, sb.Name)
	current.Spec.State = apiv1alpha2.SandboxStateRunning
	require.NoError(t, r.Update(context.Background(), current))
	_, err = r.Reconcile(context.Background(), requestFor(sb.Name))
	require.NoError(t, err)
	require.NotNil(t, f.lastCreateRequest.Sandbox.Restore)
	require.NotEqual(t, "runtime-a", f.lastCreateRequest.Identity.RuntimeInstanceID)
	require.Equal(t, apiv1alpha2.RuntimeReady, getControllerSandbox(t, r, sb.Name).Status.Runtime.State)
}

func TestResumeDuringCommittedPauseCompletesRelease(t *testing.T) {
	r, _, f, sb := newControllerHarness(t)
	current := seedReadyControllerAssignment(t, r, sb, apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1})
	current.Finalizers = []string{FinalizerName}
	current.Spec.State = apiv1alpha2.SandboxStateRunning
	require.NoError(t, r.Update(context.Background(), current))
	current = getControllerSandbox(t, r, sb.Name)
	current.Status.Runtime.State = apiv1alpha2.RuntimePausing
	current.Status.Runtime.Checkpoint = &apiv1alpha2.CheckpointStatus{CheckpointID: "checkpoint-1", ManifestRef: "s3://bucket/checkpoint/manifest.json", ArtifactDigest: "deadbeef"}
	require.NoError(t, r.Status().Update(context.Background(), current))
	// The old runtime has already been deleted, but its assignment remains.
	_, err := r.Reconcile(context.Background(), requestFor(sb.Name))
	require.NoError(t, err)
	require.Zero(t, f.ensureCall, "resume must finish release before ensuring a runtime")
	current = getControllerSandbox(t, r, sb.Name)
	require.Empty(t, current.Status.Placement.FastletName)
	require.NotNil(t, current.Status.Runtime.Checkpoint)
	_, err = r.Reconcile(context.Background(), requestFor(sb.Name))
	require.NoError(t, err)
	require.NotNil(t, f.lastCreateRequest.Sandbox.Restore)
	require.NotEqual(t, "runtime-a", f.lastCreateRequest.Identity.RuntimeInstanceID)
	require.Equal(t, apiv1alpha2.RuntimeReady, getControllerSandbox(t, r, sb.Name).Status.Runtime.State)
}
