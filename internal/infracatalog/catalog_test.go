package infracatalog

import (
	"errors"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

func TestBuiltinCompileAndFailClosedProfiles(t *testing.T) {
	catalog := Builtin()
	container, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)

	minimal, err := catalog.Compile("", container)
	require.NoError(t, err)
	require.Equal(t, "minimal", minimal.ProfileName)
	require.Empty(t, minimal.Components)

	testInfra, err := catalog.Compile("test-infra", container)
	require.NoError(t, err)
	require.Len(t, testInfra.Components, 1)
	require.Equal(t, runtimecatalog.InfraDeliveryBindMount, testInfra.Components[0].DeliveryMode)

	_, err = catalog.Compile("opensandbox-execd", container)
	require.ErrorIs(t, err, ErrProfileUnconfigured)

	_, err = catalog.Compile("e2b-envd", container)
	require.ErrorIs(t, err, ErrRuntimeUnsupported)
}

func TestProfileValidationRejectsServiceConflictsAndCycles(t *testing.T) {
	profile := Profile{
		Name: "bad", Version: "v1", Configured: true,
		Components: []Component{
			componentForTest("a", "b", "service-a", 8080),
			componentForTest("b", "a", "service-b", 8080),
		},
	}
	err := Validate(profile)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrProfileInvalid))

	profile.Components[1].Services[0].Port = 8081
	err = Validate(profile)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

func TestProfileHashIncludesImmutableArtifactDigest(t *testing.T) {
	profile := Profile{Name: "p", Version: "v1", Configured: true, Components: []Component{componentForTest("a", "", "svc", 8080)}}
	first, err := ProfileHash(profile)
	require.NoError(t, err)
	profile.Components[0].Artifact.Digest = "sha256:" + "b" + profile.Components[0].Artifact.Digest[len("sha256:")+1:]
	second, err := ProfileHash(profile)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
}

func componentForTest(name, dependency, service string, port uint32) Component {
	component := Component{
		Name:          name,
		Artifact:      Artifact{SourceType: SourceStatic, Reference: "file:///platform/component", Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		ContainerPath: "/.fast/infra/" + name,
		DeliveryModes: []runtimecatalog.InfraDeliveryMode{runtimecatalog.InfraDeliveryBindMount},
		Activation:    Activation{Mode: ActivationEntrypointSupervisor, Command: "/.fast/infra/" + name, RestartPolicy: RestartNever},
		InstanceInit:  InstanceInit{Mode: InitNone}, Required: true,
		Services: []Service{{Name: service, Transport: "http", Port: port, Readiness: ReadinessProbe{Type: ProbeHTTP, Path: "/health"}}},
	}
	if dependency != "" {
		component.DependsOn = []string{dependency}
	}
	return component
}
