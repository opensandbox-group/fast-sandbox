package runtimecatalog

import (
	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const builtinProfileVersion = "v1"

func builtinProfiles() map[apiv1alpha1.RuntimeName]RuntimeProfile {
	containerdPaths := []HostPathRequirement{
		{Name: "containerd-run", HostPath: "/run/containerd", MountPath: "/run/containerd", Type: corev1.HostPathDirectory},
		{Name: "containerd-root", HostPath: "/var/lib/containerd", MountPath: "/var/lib/containerd", Type: corev1.HostPathDirectory},
	}
	linuxNetworkPaths := []HostPathRequirement{
		{Name: "fast-sandbox-netns", HostPath: "/run/fast-sandbox/netns", MountPath: "/run/netns", Type: corev1.HostPathDirectoryOrCreate, MountPropagation: corev1.MountPropagationBidirectional},
		{Name: "fast-sandbox-network", HostPath: "/run/fast-sandbox/network", MountPath: "/run/fast-sandbox/network", Type: corev1.HostPathDirectoryOrCreate},
	}
	containerPaths := append(append([]HostPathRequirement{}, containerdPaths...), linuxNetworkPaths...)
	gvisorPaths := append([]HostPathRequirement{}, containerPaths...)
	gvisorPaths = append(gvisorPaths,
		HostPathRequirement{Name: "gvisor-runsc", HostPath: "/usr/local/bin/runsc", MountPath: "/usr/local/bin/runsc", Type: corev1.HostPathFile, ReadOnly: true},
		HostPathRequirement{Name: "gvisor-shim", HostPath: "/usr/local/bin/containerd-shim-runsc-v1", MountPath: "/usr/local/bin/containerd-shim-runsc-v1", Type: corev1.HostPathFile, ReadOnly: true},
		HostPathRequirement{Name: "gvisor-config", HostPath: "/etc/containerd/runsc.toml", MountPath: "/etc/containerd/runsc.toml", Type: corev1.HostPathFile, ReadOnly: true},
	)
	kataPaths := append(append([]HostPathRequirement{}, containerdPaths...), linuxNetworkPaths...)
	kataPaths = append(kataPaths,
		HostPathRequirement{Name: "dev-kvm", HostPath: "/dev/kvm", MountPath: "/dev/kvm", Type: corev1.HostPathCharDev},
		HostPathRequirement{Name: "kata-runtime", HostPath: "/opt/kata", MountPath: "/opt/kata", Type: corev1.HostPathDirectory, ReadOnly: true},
	)

	return map[apiv1alpha1.RuntimeName]RuntimeProfile{
		apiv1alpha1.RuntimeContainer: {
			Name: apiv1alpha1.RuntimeContainer, Version: builtinProfileVersion, Driver: DriverKindContainerd,
			Containerd:         &ContainerdConfig{Handler: "io.containerd.runc.v2"},
			Deployment:         DeploymentRequirements{Privileged: true, HostPaths: containerPaths, Overhead: overhead("100m", "128Mi")},
			Capabilities:       Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
			NetworkMode:        NetworkModeLinuxNetNS,
			InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryBindMount, InfraDeliveryImageLayer, InfraDeliveryPreinstalled},
		},
		apiv1alpha1.RuntimeGVisor: {
			Name: apiv1alpha1.RuntimeGVisor, Version: builtinProfileVersion, Driver: DriverKindContainerd,
			Containerd:         &ContainerdConfig{Handler: "io.containerd.runsc.v1", ConfigPath: "/etc/containerd/runsc.toml", OptionsType: "io.containerd.runsc.v1.options", NeedsTTY: true},
			Deployment:         DeploymentRequirements{Privileged: true, HostPaths: gvisorPaths, Overhead: overhead("200m", "256Mi")},
			Capabilities:       Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
			NetworkMode:        NetworkModeLinuxNetNS,
			InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryBindMount, InfraDeliveryImageLayer},
		},
		apiv1alpha1.RuntimeKataQemu: kataProfile(apiv1alpha1.RuntimeKataQemu, "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml", kataPaths),
		apiv1alpha1.RuntimeKataClh:  kataProfile(apiv1alpha1.RuntimeKataClh, "/opt/kata/share/defaults/kata-containers/configuration-clh.toml", kataPaths),
		apiv1alpha1.RuntimeKataFc:   unavailableKataProfile(apiv1alpha1.RuntimeKataFc, "/opt/kata/share/defaults/kata-containers/configuration-fc.toml", kataPaths, "KataFirecrackerNotValidated"),
		apiv1alpha1.RuntimeBoxLite: {
			Name: apiv1alpha1.RuntimeBoxLite, Version: builtinProfileVersion, Driver: DriverKindBoxLite,
			BoxLite: &BoxLiteConfig{
				StateRoot: "/var/lib/fast-sandbox/boxlite", BinaryPath: "/usr/local/bin/boxlite", ProxyBinary: "gvproxy",
				ControlSocket: "/run/fast-sandbox/boxlite/runtime.sock", ProtocolVersion: "v1", TunnelGuestPort: 19090,
				DefaultVCPUs: 1, DefaultMemory: "1Gi",
			},
			Deployment: DeploymentRequirements{
				Privileged: true, RequiresKVM: true, Sidecar: "boxlite-runtime", ResourceOwner: "boxlite-runtime", Overhead: overhead("200m", "256Mi"),
				HostPaths: []HostPathRequirement{
					{Name: "dev-kvm", HostPath: "/dev/kvm", MountPath: "/dev/kvm", Type: corev1.HostPathCharDev},
					{Name: "boxlite-state", HostPath: "/var/lib/fast-sandbox/boxlite", MountPath: "/var/lib/fast-sandbox/boxlite", Type: corev1.HostPathDirectoryOrCreate},
				},
			},
			Capabilities:       Capabilities{DefaultState: CapabilityUnsupported, SupportsNetwork: true, SupportsRecovery: true, Reason: "BoxLiteResourceEnforcementIncomplete"},
			NetworkMode:        NetworkModeBoxLite,
			InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryTemplateBake, InfraDeliveryPreinstalled, InfraDeliveryArtifactVolume},
		},
	}
}

func unavailableKataProfile(name apiv1alpha1.RuntimeName, configPath string, paths []HostPathRequirement, reason string) RuntimeProfile {
	profile := kataProfile(name, configPath, paths)
	profile.Capabilities.DefaultState = CapabilityDegraded
	profile.Capabilities.Reason = reason
	return profile
}

func kataProfile(name apiv1alpha1.RuntimeName, configPath string, paths []HostPathRequirement) RuntimeProfile {
	return RuntimeProfile{
		Name: name, Version: builtinProfileVersion, Driver: DriverKindContainerd,
		Containerd:         &ContainerdConfig{Handler: "io.containerd.kata.v2", RuntimePath: "/opt/kata/bin/containerd-shim-kata-v2", ConfigPath: configPath},
		Deployment:         DeploymentRequirements{Privileged: true, RequiresKVM: true, HostPaths: paths, Overhead: overhead("250m", "256Mi")},
		Capabilities:       Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
		NetworkMode:        NetworkModeKata,
		InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryTemplateBake, InfraDeliveryPreinstalled, InfraDeliveryGuestCopy},
	}
}

func overhead(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	}
}
