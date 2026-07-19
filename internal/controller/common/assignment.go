package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrAssignmentConflict = errors.New("sandbox already has a different assignment")

var ErrAssignmentChanged = errors.New("sandbox assignment changed before it could be cleared")

// EnsureSandboxAssignment establishes the authoritative assignment with a
// resourceVersion precondition. Its merge patch only owns assignment and the
// deprecated compatibility projection, so independent status writers do not
// overwrite runtime/data-plane Conditions.
func EnsureSandboxAssignment(
	ctx context.Context,
	k8sClient client.Client,
	key types.NamespacedName,
	desired apiv1alpha1.SandboxAssignment,
) (*apiv1alpha1.Sandbox, error) {
	validationAssignment := desired
	validationAssignment.Attempt = 1
	if err := validationAssignment.Validate(); err != nil {
		return nil, fmt.Errorf("invalid assignment: %w", err)
	}

	var result *apiv1alpha1.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha1.Sandbox
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		if current.Status.Assignment != nil {
			if assignmentTargetEqual(*current.Status.Assignment, desired) {
				result = current.DeepCopy()
				return nil
			}
			return ErrAssignmentConflict
		}
		nextAssignment := desired
		nextAssignment.Attempt = current.Status.AssignmentAttempt + 1

		generation := current.Status.InstanceGeneration
		if generation < apiv1alpha1.InitialInstanceGeneration {
			generation = apiv1alpha1.InitialInstanceGeneration
		}
		routeGeneration := current.Status.RouteGeneration
		if routeGeneration < 1 {
			routeGeneration = 1
		}
		patchBody, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": current.ResourceVersion},
			"status": map[string]any{
				"assignment":         nextAssignment,
				"assignmentAttempt":  nextAssignment.Attempt,
				"instanceGeneration": generation,
				"routeGeneration":    routeGeneration,
				"assignedFastlet":    desired.FastletName,
				"nodeName":           desired.NodeName,
			},
		})
		if err != nil {
			return err
		}

		patchTarget := &apiv1alpha1.Sandbox{}
		patchTarget.Namespace = key.Namespace
		patchTarget.Name = key.Name
		if err := k8sClient.Status().Patch(ctx, patchTarget, client.RawPatch(types.MergePatchType, patchBody)); err != nil {
			return err
		}
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		result = current.DeepCopy()
		return nil
	})
	if err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf("assignment CAS exhausted: %w", err)
		}
		return nil, err
	}
	return result, nil
}

// ClearSandboxAssignment removes only the expected placement and retains the
// assignment-attempt high-water mark. When advanceInstance is true, the
// runtime and route generations are advanced to fence a lost/reset instance.
func ClearSandboxAssignment(
	ctx context.Context,
	k8sClient client.Client,
	key types.NamespacedName,
	expected apiv1alpha1.SandboxAssignment,
	advanceInstance bool,
) (*apiv1alpha1.Sandbox, error) {
	var result *apiv1alpha1.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha1.Sandbox
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		if current.Status.Assignment == nil {
			result = current.DeepCopy()
			return nil
		}
		if !assignmentsEqual(*current.Status.Assignment, expected) {
			return ErrAssignmentChanged
		}

		generation := current.Status.InstanceGeneration
		if generation < apiv1alpha1.InitialInstanceGeneration {
			generation = apiv1alpha1.InitialInstanceGeneration
		}
		routeGeneration := current.Status.RouteGeneration
		if routeGeneration < 1 {
			routeGeneration = 1
		}
		// Removing an assignment always fences the local data-plane route,
		// including a capacity reschedule that retains the runtime generation.
		routeGeneration++
		if advanceInstance {
			generation = apiv1alpha1.NextInstanceGeneration(generation)
		}
		patchBody, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": current.ResourceVersion},
			"status": map[string]any{
				"assignment":         nil,
				"assignedFastlet":    "",
				"nodeName":           "",
				"sandboxID":          "",
				"endpoints":          []string{},
				"instanceGeneration": generation,
				"routeGeneration":    routeGeneration,
				"runtimeState":       apiv1alpha1.ObservedStatePending,
				"dataPlaneState":     apiv1alpha1.ObservedStatePending,
				"phase":              string(apiv1alpha1.PhasePending),
			},
		})
		if err != nil {
			return err
		}
		patchTarget := &apiv1alpha1.Sandbox{}
		patchTarget.Namespace = key.Namespace
		patchTarget.Name = key.Name
		if err := k8sClient.Status().Patch(ctx, patchTarget, client.RawPatch(types.MergePatchType, patchBody)); err != nil {
			return err
		}
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		result = current.DeepCopy()
		return nil
	})
	return result, err
}

func assignmentsEqual(a, b apiv1alpha1.SandboxAssignment) bool {
	return assignmentTargetEqual(a, b) && a.Attempt == b.Attempt
}

func assignmentTargetEqual(a, b apiv1alpha1.SandboxAssignment) bool {
	return a.FastletName == b.FastletName &&
		a.FastletPodUID == b.FastletPodUID &&
		a.NodeName == b.NodeName
}
