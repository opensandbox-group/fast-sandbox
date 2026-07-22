package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestValidateRuntime(t *testing.T) {
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeKataFc}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{Runtime: RuntimeName("unknown")}).ValidateRuntime())
}

func TestValidateSandboxPoolUpdate(t *testing.T) {
	base := SandboxPoolSpec{
		Runtime: RuntimeContainer,
		SandboxResources: SandboxResourceProfile{
			CPU:    resource.MustParse("1"),
			Memory: resource.MustParse("1Gi"),
			PIDs:   256,
		},
	}

	same := *base.DeepCopy()
	require.NoError(t, ValidateSandboxPoolUpdate(&base, &same))

	runtimeChanged := *base.DeepCopy()
	runtimeChanged.Runtime = RuntimeGVisor
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &runtimeChanged), ErrRuntimeImmutable)

	resourcesChanged := *base.DeepCopy()
	resourcesChanged.SandboxResources.Memory = resource.MustParse("2Gi")
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &resourcesChanged), ErrResourcesImmutable)
}

func TestGenerationAndAssignmentValidation(t *testing.T) {
	require.Equal(t, int64(1), NextInstanceGeneration(0))
	require.Equal(t, int64(2), NextInstanceGeneration(1))

	assignment := &SandboxAssignment{FastletName: "fastlet-1", FastletPodUID: "pod-uid", Attempt: 1}
	require.NoError(t, assignment.Validate())
	assignment.Attempt = 0
	require.Error(t, assignment.Validate())
}

func TestSandboxResourceProfileHashIsCanonical(t *testing.T) {
	a := SandboxResourceProfile{CPU: resource.MustParse("1"), Memory: resource.MustParse("1024Mi"), PIDs: 256}
	b := SandboxResourceProfile{CPU: resource.MustParse("1000m"), Memory: resource.MustParse("1Gi"), PIDs: 256}
	require.Equal(t, a.Hash(), b.Hash())
	b.PIDs++
	require.NotEqual(t, a.Hash(), b.Hash())
}

func TestValidateSandboxResourceProfile(t *testing.T) {
	valid := SandboxResourceProfile{CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"), PIDs: 128}
	require.NoError(t, ValidateSandboxResourceProfile(valid))
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{}), ErrInvalidSandboxResourceProfile)
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{
		CPU: resource.MustParse("1"),
	}), ErrInvalidSandboxResourceProfile)
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{
		CPU: resource.MustParse("1m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
	}), ErrInvalidSandboxResourceProfile)
}
