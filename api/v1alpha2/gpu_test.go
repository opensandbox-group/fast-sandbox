package v1alpha2

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestGPURequirementsValidationAndProfileIdentity(t *testing.T) {
	memory := resource.MustParse("30Gi")
	zero := resource.MustParse("0")
	negative := resource.MustParse("-1Gi")
	for _, test := range []struct {
		name         string
		count        int32
		requirements *GPUDeviceRequirements
		wantError    string
	}{
		{name: "unconstrained", count: 1},
		{name: "model", count: 1, requirements: &GPUDeviceRequirements{Model: "Tesla-V100S-PCIE-32GB"}},
		{name: "memory", count: 1, requirements: &GPUDeviceRequirements{MinimumMemory: &memory}},
		{name: "both", count: 1, requirements: &GPUDeviceRequirements{Model: "Tesla-V100S-PCIE-32GB", MinimumMemory: &memory}},
		{name: "CPU", requirements: &GPUDeviceRequirements{Model: "Tesla-V100S-PCIE-32GB"}, wantError: "positive gpus"},
		{name: "empty", count: 1, requirements: &GPUDeviceRequirements{}, wantError: "specify model or minimumMemory"},
		{name: "invalid model", count: 1, requirements: &GPUDeviceRequirements{Model: "Tesla V100S"}, wantError: "label value"},
		{name: "long model", count: 1, requirements: &GPUDeviceRequirements{Model: strings.Repeat("a", 64)}, wantError: "label value"},
		{name: "zero memory", count: 1, requirements: &GPUDeviceRequirements{MinimumMemory: &zero}, wantError: "greater than zero"},
		{name: "negative memory", count: 1, requirements: &GPUDeviceRequirements{MinimumMemory: &negative}, wantError: "greater than zero"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 128)
			profile.GPUs, profile.GPURequirements = test.count, test.requirements
			pool := SandboxPoolSpec{Runtime: RuntimeContainer, MaxSandboxesPerPod: 1, SandboxResources: profile,
				FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet"}}}}}
			if test.wantError != "" {
				require.ErrorContains(t, ValidateSandboxResourceProfile(profile), test.wantError)
				require.ErrorContains(t, pool.ValidateGPUContract(), test.wantError)
				return
			}
			require.NoError(t, ValidateSandboxResourceProfile(profile))
			require.NoError(t, pool.ValidateGPUContract())
		})
	}
	profile := NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 128)
	profile.GPUs = 1
	previous := profile.Hash()
	profile.GPURequirements = &GPUDeviceRequirements{Model: "Tesla-V100S-PCIE-32GB", MinimumMemory: &memory}
	require.NotEqual(t, previous, profile.Hash())
	cloned := profile.DeepCopy()
	cloned.GPURequirements.MinimumMemory.Set(1)
	require.NotEqual(t, cloned.Hash(), profile.Hash())
	require.Equal(t, "30Gi", profile.GPURequirements.MinimumMemory.String())
}

func TestGPUProfileHashKeepsCPUCompatibility(t *testing.T) {
	profile := NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 128)
	require.Equal(t, "653b07c8ce4443538d905f9a5fe3289ddcf1af81911ac2fd2c98a1d755420ec2", profile.Hash(), "historical CPU-only hash from main@4fe01ba")
	original := profile.Hash()
	profile.GPUs = 1
	require.NotEqual(t, original, profile.Hash())
	profile.GPUs = 0
	require.Equal(t, original, profile.Hash())
	profile.GPUs = -1
	require.Error(t, ValidateSandboxResourceProfile(profile))
	profile.GPUs = 65
	require.NoError(t, ValidateSandboxResourceProfile(profile))
	aware := NewResourceAwareSandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), resource.MustParse("2"), resource.MustParse("2"), 128)
	require.Equal(t, "54e18a490abe98acede0aa58f66272eaf0f44e9137f84580d0a3cb9669809f44", aware.Hash(), "historical ResourceAware hash from main@4fe01ba")
	original = aware.Hash()
	aware.GPUs = 1
	require.NotEqual(t, original, aware.Hash())
	aware.GPUs = 0
	require.Equal(t, original, aware.Hash())
}

func TestGPUPoolRejectsExplicitGPUInInitContainer(t *testing.T) {
	pool := SandboxPoolSpec{
		Runtime: RuntimeContainer, MaxSandboxesPerPod: 1,
		SandboxResources: SandboxResourceProfile{GPUs: 1},
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "fastlet"}},
			InitContainers: []corev1.Container{{Name: "init"}},
		}},
	}
	initContainer := &pool.FastletTemplate.Spec.InitContainers[0]
	require.NoError(t, pool.ValidateGPUContract())
	requirements := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
	initContainer.Resources.Requests = requirements
	require.ErrorContains(t, pool.ValidateGPUContract(), "do not set it on container \"init\"")
	initContainer.Resources = corev1.ResourceRequirements{Limits: requirements}
	require.ErrorContains(t, pool.ValidateGPUContract(), "do not set it on container \"init\"")
}
