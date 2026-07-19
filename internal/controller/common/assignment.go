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
	if err := desired.Validate(); err != nil {
		return nil, fmt.Errorf("invalid assignment: %w", err)
	}

	var result *apiv1alpha1.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha1.Sandbox
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		if current.Status.Assignment != nil {
			if assignmentsEqual(*current.Status.Assignment, desired) {
				result = current.DeepCopy()
				return nil
			}
			return ErrAssignmentConflict
		}

		generation := current.Status.InstanceGeneration
		if generation < apiv1alpha1.InitialInstanceGeneration {
			generation = apiv1alpha1.InitialInstanceGeneration
		}
		patchBody, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": current.ResourceVersion},
			"status": map[string]any{
				"assignment":         desired,
				"instanceGeneration": generation,
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

func assignmentsEqual(a, b apiv1alpha1.SandboxAssignment) bool {
	return a.FastletName == b.FastletName &&
		a.FastletPodUID == b.FastletPodUID &&
		a.NodeName == b.NodeName &&
		a.Attempt == b.Attempt
}
