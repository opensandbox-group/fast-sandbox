package reconciler

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestResolvedFastletProxyResourcesUsesDefaults(t *testing.T) {
	resources, err := (&SandboxPoolReconciler{}).resolvedFastletProxyResources()
	require.NoError(t, err)
	require.Equal(t, "50m", resources.Requests.Cpu().String())
	require.Equal(t, "64Mi", resources.Requests.Memory().String())
	require.Equal(t, "250m", resources.Limits.Cpu().String())
	require.Equal(t, "256Mi", resources.Limits.Memory().String())
}

func TestResolvedFastletProxyResourcesRejectsIncompleteConfiguration(t *testing.T) {
	reconciler := &SandboxPoolReconciler{FastletProxyResources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
	}}

	_, err := reconciler.resolvedFastletProxyResources()
	require.ErrorContains(t, err, "fastlet proxy request memory must be greater than zero")
}
