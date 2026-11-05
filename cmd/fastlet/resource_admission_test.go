package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastletsettings"
)

func TestBoundedSettingsRequireCgroupCapableDriver(t *testing.T) {
	_, err := configureRuntimeCgroups(nil, &fastletsettings.ResourceAdmissionSettings{Version: fastletsettings.BoundedFirecrackerVersion}, "pod")
	require.ErrorContains(t, err, "cannot enforce")
	manager, err := configureRuntimeCgroups(nil, &fastletsettings.ResourceAdmissionSettings{Version: "v2"}, "pod")
	require.NoError(t, err)
	require.Nil(t, manager)
}

func TestResourceAdmissionObserverMemoryLimitMatchesObservedCgroupScope(t *testing.T) {
	settings := &fastletsettings.ResourceAdmissionSettings{
		SandboxRuntimeBudget: apiv1alpha2.SandboxComputeResources{Memory: resource.MustParse("512Mi")},
		RuntimeEnvelope:      apiv1alpha2.SandboxComputeResources{Memory: resource.MustParse("1Gi")},
		PodMemoryBudget:      resource.MustParse("3Gi"),
	}

	containerLimit, err := resourceAdmissionObserverMemoryLimit(settings, false, false)
	require.NoError(t, err)
	containerBudget := resource.MustParse("1Gi")
	require.Equal(t, containerBudget.Value(), containerLimit)

	podLimit, err := resourceAdmissionObserverMemoryLimit(settings, true, false)
	require.NoError(t, err)
	podBudget := resource.MustParse("3Gi")
	require.Equal(t, podBudget.Value(), podLimit)

	runtimeLimit, err := resourceAdmissionObserverMemoryLimit(settings, false, true)
	require.NoError(t, err)
	runtimeBudget := resource.MustParse("512Mi")
	require.Equal(t, runtimeBudget.Value(), runtimeLimit)
}
