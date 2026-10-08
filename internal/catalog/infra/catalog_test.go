package infra

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

func TestCompileEmptyPlanPreservesCanonicalRevisionAcrossJSON(t *testing.T) {
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeContainer)
	if err != nil {
		t.Fatalf("resolve container runtime profile: %v", err)
	}
	plan, err := Compile(nil, runtimeProfile)
	if err != nil {
		t.Fatalf("Compile(nil) error = %v", err)
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded Plan
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.Components == nil {
		t.Fatalf("decoded Components = nil, want canonical empty list; JSON=%s", payload)
	}
	revision, err := Revision(decoded.Components)
	if err != nil {
		t.Fatalf("Revision() error = %v", err)
	}
	if revision != plan.Revision {
		t.Fatalf("round-trip revision = %q, want %q; JSON=%s", revision, plan.Revision, payload)
	}
}

func TestCompileNormalizesInlineComponentsForEachSupportedRuntime(t *testing.T) {
	component := componentForTest("execd", "44772")
	for _, runtimeName := range []apiv1alpha2.RuntimeName{
		apiv1alpha2.RuntimeContainer,
		apiv1alpha2.RuntimeGVisor,
		apiv1alpha2.RuntimeKataQemu,
		apiv1alpha2.RuntimeKataClh,
		apiv1alpha2.RuntimeKataFc,
		apiv1alpha2.RuntimeKataDragonball,
	} {
		runtimeProfile, err := runtimecatalog.Builtin().Resolve(runtimeName)
		require.NoError(t, err)

		plan, err := Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
		require.NoError(t, err, "runtime %s", runtimeName)
		require.NotEmpty(t, plan.Revision)
		require.Len(t, plan.Components, 1)
		require.Equal(t, "execd", plan.Components[0].Name)
		require.Equal(t, SourceOCIImage, plan.Components[0].Artifact.Source.Type)
		require.Equal(t, RestartOnFailure, plan.Components[0].Process.RestartPolicy)
		require.Equal(t, ProbeHTTP, plan.Components[0].Process.Readiness.Type)
		require.Equal(t, 10*time.Second, plan.Components[0].Process.Readiness.Timeout)
		require.Equal(t, uint32(44772), plan.Components[0].Endpoint.Port)
	}
}

func TestCompileRejectsRuntimeWithoutDeliveryMode(t *testing.T) {
	_, err := Compile([]apiv1alpha2.InfraComponent{componentForTest("execd", "44772")}, runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeContainer,
	})
	require.ErrorIs(t, err, ErrRuntimeUnsupported)
}

func TestCompileRejectsInvalidInlineComponent(t *testing.T) {
	component := componentForTest("execd", "44772")
	component.Artifact.Mappings[0].TargetPath = "/usr/local/bin/execd"

	_, err := Compile([]apiv1alpha2.InfraComponent{component}, runtimecatalog.RuntimeProfile{
		Name:               apiv1alpha2.RuntimeContainer,
		InfraDeliveryModes: []runtimecatalog.InfraDeliveryMode{runtimecatalog.InfraDeliveryBindMount},
	})
	require.ErrorIs(t, err, ErrComponentsInvalid)
}

func TestRevisionIncludesImmutableArtifactAndProcessContract(t *testing.T) {
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	component := componentForTest("execd", "44772")
	first, err := Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)

	component.Process.Env = map[string]string{"MODE": "test"}
	second, err := Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)
	require.NotEqual(t, first.Revision, second.Revision)
}

func TestCompileHostProcessComponentSkipsArtifactAndPinsDelivery(t *testing.T) {
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	component := apiv1alpha2.InfraComponent{
		Name:     "egress",
		Delivery: apiv1alpha2.InfraDeliveryHostProcess,
		Process: apiv1alpha2.InfraProcess{
			Command:     []string{"/bin/egress"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{TCPConnect: &apiv1alpha2.InfraTCPConnect{}, TimeoutSeconds: 5},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 18080},
	}
	plan, err := Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)
	require.Len(t, plan.Components, 1)
	compiled := plan.Components[0]
	require.Equal(t, "egress", compiled.Name)
	require.Equal(t, runtimecatalog.InfraDeliveryHostProcess, compiled.Delivery)
	require.Empty(t, compiled.Artifact.Source.Type)
	require.Empty(t, compiled.Artifact.Mappings)
	require.Equal(t, ProbeTCP, compiled.Process.Readiness.Type)
	require.Equal(t, 5*time.Second, compiled.Process.Readiness.Timeout)
	require.Equal(t, uint32(18080), compiled.Endpoint.Port)
}

func TestCompileRejectsHostProcessDeliveryWithoutRuntimeSupport(t *testing.T) {
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	component := apiv1alpha2.InfraComponent{
		Name:     "egress",
		Delivery: apiv1alpha2.InfraDeliveryHostProcess,
		Process: apiv1alpha2.InfraProcess{
			Command:     []string{"/bin/egress"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{TCPConnect: &apiv1alpha2.InfraTCPConnect{}},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 18080},
	}
	_, err = Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.ErrorIs(t, err, ErrRuntimeUnsupported)
}

func componentForTest(name, port string) apiv1alpha2.InfraComponent {
	return apiv1alpha2.InfraComponent{
		Name: name,
		Artifact: &apiv1alpha2.InfraArtifact{
			Source: apiv1alpha2.InfraArtifactSource{Image: &apiv1alpha2.InfraArtifactImage{
				Reference: "registry.example/component@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
			Mappings: []apiv1alpha2.InfraArtifactMapping{{
				SourcePath: "/execd",
				TargetPath: "/.fast/components/" + name + "/execd",
			}},
		},
		Process: apiv1alpha2.InfraProcess{
			Command: []string{"/.fast/components/" + name + "/execd"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{
				HTTPGet: &apiv1alpha2.InfraHTTPGet{Path: "/ping"},
			},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}
}
