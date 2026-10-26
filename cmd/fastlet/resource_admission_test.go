package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastletsettings"
)

func TestResourceAdmissionObserverMemoryLimitMatchesObservedCgroupScope(t *testing.T) {
	settings := &fastletsettings.ResourceAdmissionSettings{
		RuntimeEnvelope: apiv1alpha2.SandboxComputeResources{Memory: resource.MustParse("1Gi")},
		PodMemoryBudget: resource.MustParse("3Gi"),
	}

	containerLimit, err := resourceAdmissionObserverMemoryLimit(settings, false)
	require.NoError(t, err)
	containerBudget := resource.MustParse("1Gi")
	require.Equal(t, containerBudget.Value(), containerLimit)

	podLimit, err := resourceAdmissionObserverMemoryLimit(settings, true)
	require.NoError(t, err)
	podBudget := resource.MustParse("3Gi")
	require.Equal(t, podBudget.Value(), podLimit)
}
