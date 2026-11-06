package reconciler

import (
	"context"
	"errors"
	"fmt"
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

func TestPauseDuringResumePreservesRestoreLineage(t *testing.T) {
	for _, state := range []apiv1alpha2.RuntimeState{"", apiv1alpha2.RuntimePending, apiv1alpha2.RuntimeCreating, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.RuntimeResuming} {
		for _, assigned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/assigned=%t", state, assigned), func(t *testing.T) {
				r, _, f, sb := newControllerHarness(t)
				current := getControllerSandbox(t, r, sb.Name)
				if assigned {
					current = seedReadyControllerAssignment(t, r, current, apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1})
				}
				current.Finalizers = []string{FinalizerName}
				current.Spec.State = apiv1alpha2.SandboxStatePaused
				require.NoError(t, r.Update(context.Background(), current))
				current = getControllerSandbox(t, r, sb.Name)
				current.Status.Runtime.State = state
				current.Status.Runtime.Checkpoint = &apiv1alpha2.CheckpointStatus{CheckpointID: "saved-checkpoint", ManifestRef: "s3://bucket/saved/manifest.json", ArtifactDigest: "saved-digest"}
				require.NoError(t, r.Status().Update(context.Background(), current))

				_, err := r.Reconcile(context.Background(), requestFor(sb.Name))
				require.NoError(t, err)
				require.NotNil(t, f.lastCreateRequest)
				require.NotNil(t, f.lastCreateRequest.Sandbox.Restore, "re-pausing a pending resume must never boot a fresh image")
				require.Equal(t, "s3://bucket/saved/manifest.json", f.lastCreateRequest.Sandbox.Restore.ManifestRef)
				require.Equal(t, "saved-digest", f.lastCreateRequest.Sandbox.Restore.ArtifactDigest)
				for i := 0; i < 4; i++ {
					_, err = r.Reconcile(context.Background(), requestFor(sb.Name))
					require.NoError(t, err)
				}
				current = getControllerSandbox(t, r, sb.Name)
				require.Equal(t, apiv1alpha2.RuntimePaused, current.Status.Runtime.State)
				require.NotNil(t, current.Status.Runtime.Checkpoint)
				require.Empty(t, current.Status.Placement.FastletName)
			})
		}
	}
}

func TestPauseReleaseRecoversAfterAssignmentCleanupWriteFailure(t *testing.T) {
	for _, failProjection := range []bool{true, false} {
		t.Run(fmt.Sprintf("projection=%t", failProjection), func(t *testing.T) {
			r, _, f, sb := newControllerHarness(t)
			current := seedReadyControllerAssignment(t, r, sb, apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1})
			current.Finalizers = []string{FinalizerName}
			current.Spec.State = apiv1alpha2.SandboxStatePaused
			require.NoError(t, r.Update(context.Background(), current))
			current = getControllerSandbox(t, r, sb.Name)
			current.Status.Runtime.State = apiv1alpha2.RuntimePausing
			checkpoint := &apiv1alpha2.CheckpointStatus{CheckpointID: "committed-checkpoint", ManifestRef: "s3://bucket/committed/manifest.json", ArtifactDigest: "committed-digest"}
			current.Status.Runtime.Checkpoint = checkpoint.DeepCopy()
			require.NoError(t, r.Status().Update(context.Background(), current))
			f.runtimes[string(current.UID)] = "running"
			base := r.Client
			injected := errors.New("injected release status failure")
			wrapped := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if s, ok := obj.(*apiv1alpha2.Sandbox); ok && name == "status" && failProjection && s.Status.Placement.FastletName == "" {
						return injected
					}
					return c.SubResource(name).Patch(ctx, obj, patch, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if s, ok := obj.(*apiv1alpha2.Sandbox); ok && name == "status" && !failProjection && s.Status.Runtime.State == apiv1alpha2.RuntimePaused {
						return injected
					}
					return c.SubResource(name).Update(ctx, obj, opts...)
				},
			})
			r.Client = wrapped
			r.Orchestrator.Client = wrapped
			_, err := r.Reconcile(context.Background(), requestFor(sb.Name))
			require.ErrorIs(t, err, injected)
			current = getControllerSandbox(t, r, sb.Name)
			envelope, err := assignment.AssignmentFromAnnotation(current)
			require.NoError(t, err)
			require.Nil(t, envelope, "annotation removal must already be durable")
			require.Equal(t, apiv1alpha2.RuntimePausing, current.Status.Runtime.State)
			if failProjection {
				require.NotEmpty(t, current.Status.Placement.FastletName)
			}
			deleteCalls := f.deleteCall

			// Retry from durable state only, as after a controller restart.
			r.Client = base
			r.Orchestrator.Client = base
			_, err = r.Reconcile(context.Background(), requestFor(sb.Name))
			require.NoError(t, err)
			current = getControllerSandbox(t, r, sb.Name)
			require.Equal(t, apiv1alpha2.RuntimePaused, current.Status.Runtime.State)
			require.Equal(t, checkpoint, current.Status.Runtime.Checkpoint)
			require.Empty(t, current.Status.Placement.FastletName)
			require.Equal(t, int64(2), current.Status.DataPlane.RouteGeneration)
			require.Equal(t, deleteCalls, f.deleteCall, "a removed assignment must not be resolved for another runtime deletion")
			current.Spec.State = apiv1alpha2.SandboxStateRunning
			require.NoError(t, r.Update(context.Background(), current))
			_, err = r.Reconcile(context.Background(), requestFor(sb.Name))
			require.NoError(t, err)
			require.NotNil(t, f.lastCreateRequest.Sandbox.Restore)
			require.Equal(t, checkpoint.ManifestRef, f.lastCreateRequest.Sandbox.Restore.ManifestRef)
		})
	}
}
