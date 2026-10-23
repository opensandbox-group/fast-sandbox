package v1alpha2

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestValidateRuntime(t *testing.T) {
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeKataFc}).ValidateRuntime())
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeKataDragonball}).ValidateRuntime())
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeFirecracker}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{Runtime: RuntimeName("boxlite")}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{Runtime: RuntimeName("unknown")}).ValidateRuntime())
}

func TestValidateSandboxPoolUpdate(t *testing.T) {
	base := SandboxPoolSpec{
		Runtime: RuntimeContainer,
		SandboxResources: NewLegacySandboxResourceProfile(
			resource.MustParse("1"), resource.MustParse("1Gi"), 256,
		),
	}

	same := *base.DeepCopy()
	require.NoError(t, ValidateSandboxPoolUpdate(&base, &same))

	runtimeChanged := *base.DeepCopy()
	runtimeChanged.Runtime = RuntimeGVisor
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &runtimeChanged), ErrRuntimeImmutable)

	resourcesChanged := *base.DeepCopy()
	changedMemory := resource.MustParse("2Gi")
	resourcesChanged.SandboxResources.Memory = &changedMemory
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &resourcesChanged), ErrResourcesImmutable)
}

func TestValidateActionHandlersAndRejectRemoval(t *testing.T) {
	handler := ActionHandler{Name: "egress", TargetHTTPPort: 18080}
	base := SandboxPoolSpec{
		Runtime: RuntimeContainer, ActionHandlers: []ActionHandler{handler},
		SandboxResources: NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 128),
	}
	require.NoError(t, base.ValidateActionHandlers())

	duplicate := base
	duplicate.ActionHandlers = append(duplicate.ActionHandlers, handler)
	require.ErrorIs(t, duplicate.ValidateActionHandlers(), ErrActionHandlersInvalid)

	for _, port := range []int32{5758, 5780, 9093} {
		reserved := base
		reserved.ActionHandlers = append([]ActionHandler(nil), base.ActionHandlers...)
		reserved.ActionHandlers[0].TargetHTTPPort = port
		require.ErrorIs(t, reserved.ValidateActionHandlers(), ErrActionHandlersInvalid)
	}

	removed := base
	removed.ActionHandlers = nil
	// Fill the otherwise required immutable fields equally; Action removal is
	// the only relevant difference.
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &removed), ErrActionHandlersRemoved)
}

func TestGenerationAndAssignmentValidation(t *testing.T) {
	require.Equal(t, int64(1), NextInstanceGeneration(0))
	require.Equal(t, int64(2), NextInstanceGeneration(1))

	placement := &PlacementStatus{
		FastletName: "fastlet-1", FastletPodUID: "pod-uid", Attempt: 1,
	}
	require.NoError(t, placement.Validate())
	placement.Attempt = 0
	require.Error(t, placement.Validate())
}

func TestValidateInfraComponents(t *testing.T) {
	valid := SandboxPoolSpec{InfraComponents: []InfraComponent{{
		Name: "execd",
		Artifact: &InfraArtifact{
			Source: InfraArtifactSource{Image: &InfraArtifactImage{
				Reference: "ghcr.io/opensandbox/execd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			Mappings: []InfraArtifactMapping{{
				SourcePath: "/usr/local/bin/execd", TargetPath: "/.fast/components/execd/execd",
			}},
		},
		Process: InfraProcess{
			Command:     []string{"/.fast/components/execd/execd", "--port", "44772"},
			HealthCheck: InfraHealthCheck{HTTPGet: &InfraHTTPGet{Path: "/ping"}, TimeoutSeconds: 10},
		},
		Endpoint: InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}}}
	require.NoError(t, valid.ValidateInfraComponents())

	duplicatePort := *valid.DeepCopy()
	second := duplicatePort.InfraComponents[0]
	second.Name = "envd"
	second.Artifact.Mappings[0].TargetPath = "/.fast/components/envd/envd"
	duplicatePort.InfraComponents = append(duplicatePort.InfraComponents, second)
	require.ErrorIs(t, duplicatePort.ValidateInfraComponents(), ErrInfraComponentsInvalid)

	escapedTarget := *valid.DeepCopy()
	escapedTarget.InfraComponents[0].Artifact.Mappings[0].TargetPath = "/usr/local/bin/execd"
	require.ErrorIs(t, escapedTarget.ValidateInfraComponents(), ErrInfraComponentsInvalid)

	tokenEnv := *valid.DeepCopy()
	tokenEnv.InfraComponents[0].Process.Env = map[string]string{"FAST_SANDBOX_TOKEN": "bad"}
	require.ErrorIs(t, tokenEnv.ValidateInfraComponents(), ErrInfraComponentsInvalid)
}

func TestValidateHostProcessInfraComponent(t *testing.T) {
	hostProcess := SandboxPoolSpec{InfraComponents: []InfraComponent{{
		Name:     "egress",
		Delivery: InfraDeliveryHostProcess,
		Process: InfraProcess{
			Command:     []string{"/bin/egress"},
			HealthCheck: InfraHealthCheck{TCPConnect: &InfraTCPConnect{}, TimeoutSeconds: 10},
		},
		Endpoint: InfraEndpoint{Protocol: "HTTP", Port: 18080},
	}}}
	require.NoError(t, hostProcess.ValidateInfraComponents())

	withArtifact := *hostProcess.DeepCopy()
	withArtifact.InfraComponents[0].Artifact = &InfraArtifact{
		Source: InfraArtifactSource{Image: &InfraArtifactImage{
			Reference: "registry.example/egress@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
		Mappings: []InfraArtifactMapping{{SourcePath: "/egress", TargetPath: "/.fast/components/egress/egress"}},
	}
	require.ErrorIs(t, withArtifact.ValidateInfraComponents(), ErrInfraComponentsInvalid)

	unsupportedDelivery := *hostProcess.DeepCopy()
	unsupportedDelivery.InfraComponents[0].Delivery = "bind-mount"
	require.ErrorIs(t, unsupportedDelivery.ValidateInfraComponents(), ErrInfraComponentsInvalid)

	missingArtifact := SandboxPoolSpec{InfraComponents: []InfraComponent{{
		Name: "execd",
		Process: InfraProcess{
			Command:     []string{"/bin/execd"},
			HealthCheck: InfraHealthCheck{TCPConnect: &InfraTCPConnect{}, TimeoutSeconds: 10},
		},
		Endpoint: InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}}}
	require.ErrorIs(t, missingArtifact.ValidateInfraComponents(), ErrInfraComponentsInvalid)
}

func TestSandboxResourceProfileHashIsCanonical(t *testing.T) {
	a := NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1024Mi"), 256)
	b := NewLegacySandboxResourceProfile(resource.MustParse("1000m"), resource.MustParse("1Gi"), 256)
	require.Equal(t, a.Hash(), b.Hash())
	b.PIDs++
	require.NotEqual(t, a.Hash(), b.Hash())

	awareA := NewResourceAwareSandboxResourceProfile(
		resource.MustParse("2"), resource.MustParse("10Gi"), resource.MustParse("2"), resource.MustParse("2"), 256,
	)
	awareB := NewResourceAwareSandboxResourceProfile(
		resource.MustParse("2000m"), resource.MustParse("10240Mi"), resource.MustParse("2.0"), resource.MustParse("2000m"), 256,
	)
	require.Equal(t, awareA.Hash(), awareB.Hash())
	require.NotEqual(t, a.Hash(), awareA.Hash())
}

func TestValidateSandboxResourceProfile(t *testing.T) {
	legacy := NewLegacySandboxResourceProfile(resource.MustParse("500m"), resource.MustParse("256Mi"), 128)
	require.NoError(t, ValidateSandboxResourceProfile(legacy))
	require.Equal(t, SandboxResourceModeLegacySlot, legacy.ResourceMode())

	aware := NewResourceAwareSandboxResourceProfile(
		resource.MustParse("500m"), resource.MustParse("256Mi"), resource.MustParse("2"), resource.MustParse("2"), 128,
	)
	require.NoError(t, ValidateSandboxResourceProfile(aware))
	require.Equal(t, SandboxResourceModeResourceAware, aware.ResourceMode())
	requests, ok := aware.RequestResources()
	require.True(t, ok)
	require.Equal(t, "250m", requests.CPU.String())
	require.Equal(t, "128Mi", requests.Memory.String())

	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{}), ErrInvalidSandboxResourceProfile)
	missingMemory := resource.MustParse("1Gi")
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{Memory: &missingMemory, PIDs: 128}), ErrInvalidSandboxResourceProfile)
	require.ErrorIs(t, ValidateSandboxResourceProfile(NewLegacySandboxResourceProfile(
		resource.MustParse("1m"), resource.MustParse("256Mi"), 128,
	)), ErrInvalidSandboxResourceProfile)

	mixed := legacy
	mixed.Mode = SandboxResourceModeResourceAware
	require.ErrorIs(t, ValidateSandboxResourceProfile(mixed), ErrInvalidSandboxResourceProfile)

	invalidRatio := aware
	invalidRatio.OvercommitRatio = &SandboxOvercommitRatio{CPU: resource.MustParse("0.5"), Memory: resource.MustParse("1")}
	require.ErrorIs(t, ValidateSandboxResourceProfile(invalidRatio), ErrInvalidSandboxResourceProfile)

	rounded := NewResourceAwareSandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("5Gi"), resource.MustParse("3"), resource.MustParse("1.25"), 128,
	)
	roundedRequests, ok := rounded.RequestResources()
	require.True(t, ok)
	require.Equal(t, "334m", roundedRequests.CPU.String())
	require.Equal(t, "4Gi", roundedRequests.Memory.String())
}

func TestValidateResourceContractBranchesByMode(t *testing.T) {
	legacy := SandboxPoolSpec{
		SandboxResources: NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 128),
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "fastlet", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
		}}}},
	}
	require.NoError(t, legacy.ValidateResourceContract(), "LegacySlot retains explicit aggregate resources")

	resourceAware := legacy
	resourceAware.SandboxResources = NewResourceAwareSandboxResourceProfile(
		resource.MustParse("2"), resource.MustParse("5Gi"), resource.MustParse("8"), resource.MustParse("1.25"), 128,
	)
	require.ErrorIs(t, resourceAware.ValidateResourceContract(), ErrRuntimeOwnerResources)

	resourceAware.FastletTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	require.NoError(t, resourceAware.ValidateResourceContract())
}
