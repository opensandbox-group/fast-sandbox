package reconciler

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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
	overhead corev1.ResourceList,
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

	requests, _ := pool.Spec.SandboxResources.RequestResources()
	limits, _ := pool.Spec.SandboxResources.LimitResources()
	resolved.CommitmentCapacity = scaledComputeResources(requests, int64(pool.Spec.MaxSandboxesPerPod))
	resolved.RuntimeEnvelope = apiv1alpha2.SandboxComputeResources{
		CPU:    envelopeQuantity(overhead, corev1.ResourceCPU, resolved.CommitmentCapacity.CPU, limits.CPU),
		Memory: envelopeQuantity(overhead, corev1.ResourceMemory, resolved.CommitmentCapacity.Memory, limits.Memory),
	}
	return resolved, nil
}

func scaledComputeResources(resources apiv1alpha2.SandboxComputeResources, factor int64) apiv1alpha2.SandboxComputeResources {
	cpu := resources.CPU.DeepCopy()
	cpu.Mul(factor)
	memory := resources.Memory.DeepCopy()
	memory.Mul(factor)
	return apiv1alpha2.SandboxComputeResources{CPU: cpu, Memory: memory}
}

func envelopeQuantity(
	overhead corev1.ResourceList,
	name corev1.ResourceName,
	commitment resource.Quantity,
	limit resource.Quantity,
) resource.Quantity {
	base := commitment.DeepCopy()
	if base.Cmp(limit) < 0 {
		base = limit.DeepCopy()
	}
	if quantity, found := overhead[name]; found {
		base.Add(quantity)
	}
	return base
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
