package reconciler

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// resolvedFastletResources is the single Controller-owned resource contract
// compiled from a SandboxPool. Consumers may project this value, but must not
// derive another capacity from the Pod template or Kubernetes requests.
type resolvedFastletResources struct {
	Mode               apiv1alpha2.SandboxResourceMode
	StaticAdmission    bool
	MemoryAdmission    bool
	CommitmentCapacity apiv1alpha2.SandboxComputeResources
	RuntimeEnvelope    apiv1alpha2.SandboxComputeResources
}

func compileFastletResources(
	pool *apiv1alpha2.SandboxPool,
	_ corev1.ResourceList,
) (resolvedFastletResources, error) {
	if pool == nil {
		return resolvedFastletResources{}, fmt.Errorf("SandboxPool is required")
	}
	if err := pool.Spec.ValidateResourceContract(); err != nil {
		return resolvedFastletResources{}, err
	}
	mode := pool.Spec.SandboxResources.ResourceMode()
	resolved := resolvedFastletResources{Mode: mode}
	if mode == apiv1alpha2.SandboxResourceModeLegacySlot {
		return resolved, nil
	}
	if mode != apiv1alpha2.SandboxResourceModeResourceAware {
		return resolvedFastletResources{}, apiv1alpha2.ErrInvalidSandboxResourceProfile
	}
	resolved.StaticAdmission = true
	resolved.MemoryAdmission = true
	if pool.Spec.MaxSandboxesPerPod <= 0 {
		return resolvedFastletResources{}, fmt.Errorf("maxSandboxesPerPod must be greater than zero")
	}

	capacity, envelope, err := pool.Spec.SandboxResources.ResolveFastletCapacity(pool.Spec.MaxSandboxesPerPod)
	if err != nil {
		return resolvedFastletResources{}, err
	}
	resolved.CommitmentCapacity = capacity
	resolved.RuntimeEnvelope = envelope
	return resolved, nil
}

func applyResolvedRuntimeOwnerResources(container *corev1.Container, resolved resolvedFastletResources) {
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Requests[corev1.ResourceCPU] = resolved.RuntimeEnvelope.CPU.DeepCopy()
	container.Resources.Requests[corev1.ResourceMemory] = resolved.RuntimeEnvelope.Memory.DeepCopy()
	container.Resources.Limits[corev1.ResourceCPU] = resolved.RuntimeEnvelope.CPU.DeepCopy()
	container.Resources.Limits[corev1.ResourceMemory] = resolved.RuntimeEnvelope.Memory.DeepCopy()
}
