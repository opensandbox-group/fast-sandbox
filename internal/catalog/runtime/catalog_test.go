package runtime

import (
	"errors"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestBuiltinCatalogProfiles(t *testing.T) {
	catalog := Builtin()
	expectedNames := []apiv1alpha2.RuntimeName{
		apiv1alpha2.RuntimeContainer,
		apiv1alpha2.RuntimeFirecracker,
		apiv1alpha2.RuntimeGVisor,
		apiv1alpha2.RuntimeKataClh,
		apiv1alpha2.RuntimeKataDragonball,
		apiv1alpha2.RuntimeKataFc,
		apiv1alpha2.RuntimeKataQemu,
	}
	for name := range builtinExtensions {
		expectedNames = append(expectedNames, name)
	}
	sort.Slice(expectedNames, func(i, j int) bool { return expectedNames[i] < expectedNames[j] })
	require.Equal(t, expectedNames, catalog.Names())

	for _, name := range catalog.Names() {
		profile, err := catalog.Resolve(name)
		require.NoError(t, err)
		require.Equal(t, name, profile.Name)
		require.NotEmpty(t, profile.ProfileHash)
		hash, err := ProfileHash(profile)
		require.NoError(t, err)
		require.Equal(t, profile.ProfileHash, hash)
	}

	kata, err := catalog.Resolve(apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	require.Equal(t, DriverKindContainerd, kata.Driver)
	require.True(t, kata.Deployment.RequiresKVM)
	require.Contains(t, kata.Containerd.ConfigPath, "configuration-fc.toml")
	require.Equal(t, CapabilityConfigured, kata.Capabilities.DefaultState)
	require.Equal(t, ResidualProcessFirecracker, kata.ResidualProcess)
	dragonball, err := catalog.Resolve(apiv1alpha2.RuntimeKataDragonball)
	require.NoError(t, err)
	require.Equal(t, "/opt/kata/runtime-rs/bin/containerd-shim-kata-v2", dragonball.Containerd.RuntimePath)
	require.Contains(t, dragonball.Containerd.ConfigPath, "configuration-dragonball.toml")

	firecracker, err := catalog.Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.Equal(t, DriverKindFirecracker, firecracker.Driver)
	require.Equal(t, CapabilityConfigured, firecracker.Capabilities.DefaultState)
	require.Equal(t, ResidualProcessFirecracker, firecracker.ResidualProcess)
	require.True(t, firecracker.Deployment.RequiresKVM)
	require.True(t, hasHostPath(firecracker.Deployment.HostPaths, "/dev/kvm"))
	require.True(t, hasHostPath(firecracker.Deployment.HostPaths, "/dev/net/tun"))
	require.True(t, hasHostPath(firecracker.Deployment.HostPaths, "/usr/local/bin/firecracker"))
	require.Equal(t, "/usr/local/bin/firecracker", firecracker.Firecracker.BinaryPath)
	// No node-side guest kernel: snapshots bake their own and restore
	// never boots one (#84); direct boot pins an operator kernelPath.
	require.Empty(t, firecracker.Firecracker.KernelPath)
	require.False(t, hasHostPath(firecracker.Deployment.HostPaths, "/opt/fast-sandbox/firecracker/vmlinux.bin"))
	require.Equal(t, int32(1), firecracker.Firecracker.DefaultVCPUs)
	require.Equal(t, "512Mi", firecracker.Firecracker.DefaultMemory)

	for _, name := range []apiv1alpha2.RuntimeName{apiv1alpha2.RuntimeKataQemu, apiv1alpha2.RuntimeKataClh, apiv1alpha2.RuntimeKataDragonball} {
		kataProfile, resolveErr := catalog.Resolve(name)
		require.NoError(t, resolveErr)
		require.Contains(t, kataProfile.InfraDeliveryModes, InfraDeliveryBindMount)
		require.Equal(t, ResidualProcessNone, kataProfile.ResidualProcess)
	}

	gvisor, err := catalog.Resolve(apiv1alpha2.RuntimeGVisor)
	require.NoError(t, err)
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/usr/local/bin/runsc"))
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/usr/local/bin/containerd-shim-runsc-v1"))
	require.True(t, hasHostPath(gvisor.Deployment.HostPaths, "/etc/containerd/runsc.toml"))
	require.True(t, hostPath(gvisor.Deployment.HostPaths, "/etc/containerd/runsc.toml").ReadOnly)

	container, err := catalog.Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, DefaultContainerdNamespace, container.ContainerdNamespace())
	require.Equal(t, corev1.MountPropagationBidirectional, hostPath(container.Deployment.HostPaths, "/run/fast-sandbox/netns").MountPropagation)
	require.Equal(t, "/run/netns", hostPath(container.Deployment.HostPaths, "/run/fast-sandbox/netns").MountPath)

}

func TestRuntimeProfilesUsingFastletNetworkHaveRequiredMounts(t *testing.T) {
	catalog := Builtin()
	for _, name := range []apiv1alpha2.RuntimeName{
		apiv1alpha2.RuntimeContainer,
		apiv1alpha2.RuntimeGVisor,
		apiv1alpha2.RuntimeKataQemu,
		apiv1alpha2.RuntimeKataClh,
		apiv1alpha2.RuntimeKataDragonball,
		apiv1alpha2.RuntimeKataFc,
		apiv1alpha2.RuntimeFirecracker,
	} {
		profile, err := catalog.Resolve(name)
		require.NoError(t, err)
		require.True(t, profile.UsesFastletNetNS(), "%s must use a Fastlet-owned netns", name)
		require.True(t, hasHostPath(profile.Deployment.HostPaths, "/run/fast-sandbox/netns"), "%s is missing the named-netns mount", name)
		require.True(t, hasHostPath(profile.Deployment.HostPaths, "/run/fast-sandbox/network"), "%s is missing the network-state mount", name)
	}
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
	require.Equal(t, apiv1alpha2.RuntimeContainer, profile.Name)

	_, err = catalog.Resolve("kata-firecracker")
	require.True(t, errors.Is(err, ErrRuntimeNotFound))
}

func TestResolveReturnsIndependentProfile(t *testing.T) {
	catalog := Builtin()
	first, err := catalog.Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	first.Containerd.Handler = "mutated"
	first.Deployment.HostPaths[0].HostPath = "mutated"

	second, err := catalog.Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, "io.containerd.runc.v2", second.Containerd.Handler)
	require.Equal(t, "/run/fast-sandbox/netns", second.Deployment.HostPaths[0].HostPath)
}

func TestBuiltinCatalogIncludesRegisteredExtension(t *testing.T) {
	previous := builtinExtensions
	builtinExtensions = make(map[apiv1alpha2.RuntimeName]RuntimeDefinition)
	t.Cleanup(func() { builtinExtensions = previous })

	name := apiv1alpha2.RuntimeName("extension-test")
	registerBuiltinDefinition(RuntimeDefinition{
		Name:       name,
		Version:    "v1",
		Driver:     DriverKindContainerd,
		Containerd: &ContainerdConfig{Namespace: DefaultContainerdNamespace, Handler: "io.containerd.extension.v2"},
	})

	profile, err := Builtin().Resolve(name)
	require.NoError(t, err)
	require.Equal(t, "io.containerd.extension.v2", profile.Containerd.Handler)
	require.NotEmpty(t, profile.ProfileHash)
}
