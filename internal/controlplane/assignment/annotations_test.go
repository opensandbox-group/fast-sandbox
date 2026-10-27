package assignment

import (
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestProjectResolvedResourceAnnotationsUsesCanonicalMapping(t *testing.T) {
	resources := &fastletapi.ResolvedSandboxResources{
		Requests: fastletapi.SandboxResourceVector{CPU: "500m", Memory: "2Gi"},
		Limits:   fastletapi.SandboxRuntimeLimits{CPU: "2", Memory: "3Gi", PIDs: 128},
	}
	sandbox := &apiv1alpha2.Sandbox{}
	ProjectResolvedResourceAnnotations(sandbox, "policy-a", resources)

	require.Equal(t, ResolvedResourceAnnotations("policy-a", resources), sandbox.Annotations)
	require.Equal(t, "policy-a", sandbox.Annotations[AnnotationResourcePolicyHash])
	require.Equal(t, "500m", sandbox.Annotations[AnnotationResolvedRequestCPU])
	require.Equal(t, "2Gi", sandbox.Annotations[AnnotationResolvedRequestMemory])
	require.Equal(t, "2", sandbox.Annotations[AnnotationResolvedLimitCPU])
	require.Equal(t, "3Gi", sandbox.Annotations[AnnotationResolvedLimitMemory])
}

func TestResolvedResourceAnnotationsOmitLegacyResources(t *testing.T) {
	require.Nil(t, ResolvedResourceAnnotations("", nil))
	sandbox := &apiv1alpha2.Sandbox{}
	ProjectResolvedResourceAnnotations(sandbox, "", nil)
	require.Nil(t, sandbox.Annotations)
}
