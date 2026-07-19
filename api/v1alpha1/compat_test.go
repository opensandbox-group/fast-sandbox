package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestEffectiveRuntime(t *testing.T) {
	tests := []struct {
		name    string
		spec    SandboxPoolSpec
		want    RuntimeName
		wantErr error
	}{
		{name: "new canonical", spec: SandboxPoolSpec{Runtime: RuntimeKataFc}, want: RuntimeKataFc},
		{name: "legacy", spec: SandboxPoolSpec{RuntimeType: RuntimeGVisor}, want: RuntimeGVisor},
		{name: "legacy default", spec: SandboxPoolSpec{}, want: RuntimeContainer},
		{name: "new and legacy conflict", spec: SandboxPoolSpec{Runtime: RuntimeContainer, RuntimeType: RuntimeContainer}, wantErr: ErrRuntimeFieldConflict},
		{name: "new and handler conflict", spec: SandboxPoolSpec{Runtime: RuntimeContainer, ContainerdRuntimeHandler: "custom"}, wantErr: ErrRuntimeFieldConflict},
		{name: "legacy matching handler", spec: SandboxPoolSpec{RuntimeType: RuntimeGVisor, ContainerdRuntimeHandler: "io.containerd.runsc.v1"}, want: RuntimeGVisor},
		{name: "legacy custom handler", spec: SandboxPoolSpec{RuntimeType: RuntimeGVisor, ContainerdRuntimeHandler: "custom"}, wantErr: ErrLegacyRuntimeOverride},
		{name: "legacy custom runtime class", spec: SandboxPoolSpec{RuntimeType: RuntimeGVisor, RuntimeClassName: "custom"}, wantErr: ErrLegacyRuntimeOverride},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.EffectiveRuntime()
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
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

func TestEffectiveSandboxResources(t *testing.T) {
	defaults, err := (SandboxPoolSpec{}).EffectiveSandboxResources()
	require.NoError(t, err)
	require.Equal(t, "1", defaults.CPU.String())
	require.Equal(t, "512Mi", defaults.Memory.String())
	require.Equal(t, int64(256), defaults.PIDs)

	explicit := SandboxResourceProfile{CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"), PIDs: 128}
	got, err := (SandboxPoolSpec{SandboxResources: explicit}).EffectiveSandboxResources()
	require.NoError(t, err)
	require.Equal(t, explicit.Hash(), got.Hash())

	_, err = (SandboxPoolSpec{SandboxResources: SandboxResourceProfile{CPU: resource.MustParse("1")}}).EffectiveSandboxResources()
	require.ErrorIs(t, err, ErrInvalidSandboxResourceProfile)
	_, err = (SandboxPoolSpec{SandboxResources: SandboxResourceProfile{
		CPU: resource.MustParse("1m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
	}}).EffectiveSandboxResources()
	require.ErrorIs(t, err, ErrInvalidSandboxResourceProfile)
}
