package runtime

import (
	"errors"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuiltinCatalogProfiles(t *testing.T) {
	catalog := Builtin()
	require.Equal(t, []apiv1alpha1.RuntimeName{
		apiv1alpha1.RuntimeBoxLite,
		apiv1alpha1.RuntimeContainer,
		apiv1alpha1.RuntimeGVisor,
		apiv1alpha1.RuntimeKataClh,
		apiv1alpha1.RuntimeKataFc,
		apiv1alpha1.RuntimeKataQemu,
	}, catalog.Names())

	for _, name := range catalog.Names() {
		profile, err := catalog.Resolve(name)
		require.NoError(t, err)
		require.Equal(t, name, profile.Name)
		require.NotEmpty(t, profile.ProfileHash)
		hash, err := ProfileHash(profile)
		require.NoError(t, err)
		require.Equal(t, profile.ProfileHash, hash)
	}

	boxlite, err := catalog.Resolve(apiv1alpha1.RuntimeBoxLite)
	require.NoError(t, err)
	require.Equal(t, DriverKindBoxLite, boxlite.Driver)
	require.Equal(t, CapabilityUnsupported, boxlite.Capabilities.DefaultState)
	require.Equal(t, "BoxLiteResourceEnforcementIncomplete", boxlite.Capabilities.Reason)
	require.Equal(t, "/run/fast-sandbox/boxlite/runtime.sock", boxlite.BoxLite.ControlSocket)
	require.Equal(t, "v1", boxlite.BoxLite.ProtocolVersion)
	require.Equal(t, uint32(19090), boxlite.BoxLite.TunnelGuestPort)
	require.Equal(t, "boxlite-runtime", boxlite.Deployment.Sidecar)
	require.Equal(t, "boxlite-runtime", boxlite.Deployment.ResourceOwner)
	require.True(t, boxlite.Deployment.RequiresKVM)

	kata, err := catalog.Resolve(apiv1alpha1.RuntimeKataFc)
	require.NoError(t, err)
	require.Equal(t, DriverKindContainerd, kata.Driver)
	require.True(t, kata.Deployment.RequiresKVM)
	require.Contains(t, kata.Containerd.ConfigPath, "configuration-fc.toml")
	require.Equal(t, CapabilityDegraded, kata.Capabilities.DefaultState)
	require.Equal(t, "KataFirecrackerNotValidated", kata.Capabilities.Reason)
	for _, name := range []apiv1alpha1.RuntimeName{apiv1alpha1.RuntimeKataQemu, apiv1alpha1.RuntimeKataClh} {
		kataProfile, resolveErr := catalog.Resolve(name)
		require.NoError(t, resolveErr)
		require.Contains(t, kataProfile.InfraDeliveryModes, InfraDeliveryBindMount)
	}

	gvisor, err := catalog.Resolve(apiv1alpha1.RuntimeGVisor)
	require.NoError(t, err)
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/usr/local/bin/runsc"))
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/usr/local/bin/containerd-shim-runsc-v1"))
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/etc/containerd/runsc.toml"))
	require.True(t, hostPath(gvisor.Deployment.HostPaths, "/etc/containerd/runsc.toml").ReadOnly)

	container, err := catalog.Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, corev1.MountPropagationBidirectional, hostPath(container.Deployment.HostPaths, "/run/fast-sandbox/netns").MountPropagation)
	require.Equal(t, "/run/netns", hostPath(container.Deployment.HostPaths, "/run/fast-sandbox/netns").MountPath)
}

func TestRuntimeProfilesUsingFastletNetworkHaveRequiredMounts(t *testing.T) {
	catalog := Builtin()
	for _, name := range []apiv1alpha1.RuntimeName{
		apiv1alpha1.RuntimeContainer,
		apiv1alpha1.RuntimeGVisor,
		apiv1alpha1.RuntimeKataQemu,
		apiv1alpha1.RuntimeKataClh,
		apiv1alpha1.RuntimeKataFc,
	} {
		profile, err := catalog.Resolve(name)
		require.NoError(t, err)
		require.True(t, profile.UsesFastletNetNS(), "%s must use a Fastlet-owned netns", name)
		require.True(t, hasHostPath(profile.Deployment.HostPaths, "/run/fast-sandbox/netns"), "%s is missing the named-netns mount", name)
		require.True(t, hasHostPath(profile.Deployment.HostPaths, "/run/fast-sandbox/network"), "%s is missing the network-state mount", name)
	}

	boxlite, err := catalog.Resolve(apiv1alpha1.RuntimeBoxLite)
	require.NoError(t, err)
	require.False(t, boxlite.UsesFastletNetNS())
}

func hostPath(requirements []HostPathRequirement, path string) HostPathRequirement {
	for _, requirement := range requirements {
		if requirement.HostPath == path {
			return requirement
		}
	}
	return HostPathRequirement{}
}

func hasHostPath(requirements []HostPathRequirement, path string) bool {
	for _, requirement := range requirements {
		if requirement.HostPath == path {
			return true
		}
	}
	return false
}

func TestResolveDefaultsAndRejectsAliases(t *testing.T) {
	catalog := Builtin()
	profile, err := catalog.Resolve("")
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeContainer, profile.Name)

	_, err = catalog.Resolve("kata-firecracker")
	require.True(t, errors.Is(err, ErrRuntimeNotFound))
}

func TestResolveReturnsIndependentProfile(t *testing.T) {
	catalog := Builtin()
	first, err := catalog.Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	first.Containerd.Handler = "mutated"
	first.Deployment.HostPaths[0].HostPath = "mutated"

	second, err := catalog.Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, "io.containerd.runc.v2", second.Containerd.Handler)
	require.Equal(t, "/run/containerd", second.Deployment.HostPaths[0].HostPath)
}
