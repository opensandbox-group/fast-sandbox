package reconciler

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultFastletProxyResources returns the backward-compatible resources for
// the platform-owned Fastlet Proxy sidecar.
func DefaultFastletProxyResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// ValidateFastletProxyResources requires explicit positive CPU and memory
// requests and limits so scheduling reservations and cgroup bounds cannot
// silently diverge.
func ValidateFastletProxyResources(resources corev1.ResourceRequirements) error {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, hasRequest := resources.Requests[name]
		if !hasRequest || request.Sign() <= 0 {
			return fmt.Errorf("fastlet proxy request %s must be greater than zero", name)
		}
		limit, hasLimit := resources.Limits[name]
		if !hasLimit || limit.Sign() <= 0 {
			return fmt.Errorf("fastlet proxy limit %s must be greater than zero", name)
		}
		if request.Cmp(limit) > 0 {
			return fmt.Errorf("fastlet proxy request %s=%s exceeds limit %s", name, request.String(), limit.String())
		}
	}
	return nil
}

func (r *SandboxPoolReconciler) resolvedFastletProxyResources() (corev1.ResourceRequirements, error) {
	resources := r.FastletProxyResources
	if len(resources.Requests) == 0 && len(resources.Limits) == 0 && len(resources.Claims) == 0 {
		resources = DefaultFastletProxyResources()
	}
	if err := ValidateFastletProxyResources(resources); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	return *resources.DeepCopy(), nil
}
