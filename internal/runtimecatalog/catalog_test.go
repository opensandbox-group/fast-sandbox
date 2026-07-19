package runtimecatalog

import (
	"errors"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/stretchr/testify/require"
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

	kata, err := catalog.Resolve(apiv1alpha1.RuntimeKataFc)
	require.NoError(t, err)
	require.Equal(t, DriverKindContainerd, kata.Driver)
	require.True(t, kata.Deployment.RequiresKVM)
	require.Contains(t, kata.Containerd.ConfigPath, "configuration-fc.toml")

	gvisor, err := catalog.Resolve(apiv1alpha1.RuntimeGVisor)
	require.NoError(t, err)
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/usr/local/bin/runsc"))
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/usr/local/bin/containerd-shim-runsc-v1"))
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
