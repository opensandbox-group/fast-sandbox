package runtimeenv

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	actionapi "fast-sandbox/internal/protocol/action"
)

func TestResolveDefaultProducesCompleteImmutablePlan(t *testing.T) {
	plan, err := ResolveDefault(runtimecatalog.Builtin(), apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, actionapi.APIVersion, plan.EffectiveActionsAPIVersion())
	require.Equal(t, DefaultEnvironment, plan.Environment)
	require.Equal(t, "k8s.io", plan.Profile.ContainerdNamespace())
	require.Equal(t, "overlayfs", plan.Profile.Containerd.Snapshotter)
	require.Equal(t, "/run/containerd/containerd.sock", plan.Containerd.Socket)
	require.Equal(t, "/var/lib/kubelet", plan.Kubelet.Root)
	require.NotEmpty(t, plan.Profile.ProfileHash)
	require.Equal(t, "sha256:"+plan.Profile.ProfileHash, plan.Revision)
	require.True(t, hasHostPath(plan.Profile, "/run/containerd"))
	require.True(t, hasHostPath(plan.Profile, "/var/lib/containerd"))

	payload, err := plan.Marshal()
	require.NoError(t, err)
	decoded, err := DecodePlan(bytes.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, plan, decoded)
}

func TestLegacyRuntimePlanWithoutActionsVersionRemainsV1(t *testing.T) {
	plan, err := ResolveDefault(runtimecatalog.Builtin(), apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	plan.ActionsAPIVersion = ""
	require.NoError(t, stampRevision(&plan))
	payload, err := plan.Marshal()
	require.NoError(t, err)
	decoded, err := DecodePlan(bytes.NewReader(payload))
	require.NoError(t, err)
	require.Empty(t, decoded.ActionsAPIVersion)
	require.Equal(t, actionapi.APIVersion, decoded.EffectiveActionsAPIVersion())
}

func TestActionsAPIVersionComesFromTrustedRuntimeEnvironmentConfig(t *testing.T) {
	config, err := Parse([]byte(`
version: v1alpha2
actionsApiVersion: sandbox.fast.io/actions/v2
environments:
  default:
    runtimes:
      firecracker: {}
`))
	require.NoError(t, err)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.Equal(t, actionapi.APIVersionV2, plan.EffectiveActionsAPIVersion())

	payload, err := plan.Marshal()
	require.NoError(t, err)
	decoded, err := DecodePlan(bytes.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, plan, decoded)

	_, err = Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeContainer)
	require.ErrorContains(t, err, "supported only by runtime firecracker")
}

func TestActionsAPIVersionRejectsUnknownTrustedConfigValue(t *testing.T) {
	_, err := Parse([]byte(`
version: v1alpha2
actionsApiVersion: sandbox.fast.io/actions/v3
environments:
  default:
    runtimes:
      container: {}
`))
	require.ErrorContains(t, err, "unsupported Actions apiVersion")
}

func TestResolveKataFirecrackerUsesRuntimeSnapshotterOverride(t *testing.T) {
	plan, err := ResolveDefault(runtimecatalog.Builtin(), apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	require.Equal(t, DefaultEnvironment, plan.Environment)
	require.Equal(t, "blockfile", plan.Containerd.Snapshotter)
	require.Equal(t, "blockfile", plan.Profile.Containerd.Snapshotter)
	require.Equal(t, "/opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml", plan.Profile.Containerd.ConfigPath)
}

func TestParseMovesRuntimeToConfiguredEnvironment(t *testing.T) {
	config, err := Parse([]byte(`
version: v1alpha2
environments:
  secure:
    nodeSelector:
      example.com/secure-runtime: "true"
    containerd:
      namespace: tenant-runtime
      root: /srv/containerd/root
    kubelet:
      root: /srv/kubelet
    runtimes:
      container:
        handler: io.containerd.custom.v2
`))
	require.NoError(t, err)
	_, stillDefault := config.Environments[DefaultEnvironment].Runtimes[apiv1alpha2.RuntimeContainer]
	require.False(t, stillDefault)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, "secure", plan.Environment)
	require.Equal(t, "tenant-runtime", plan.Profile.Containerd.Namespace)
	require.Equal(t, "io.containerd.custom.v2", plan.Profile.Containerd.Handler)
	require.Equal(t, "/srv/kubelet", plan.Kubelet.Root)
	require.Equal(t, "true", plan.Profile.Deployment.NodeSelector["example.com/secure-runtime"])
	require.True(t, hasHostPath(plan.Profile, "/srv/containerd/root"))
}

func TestParseRejectsInvalidPlatformPaths(t *testing.T) {
	_, err := Parse([]byte(`
version: v1alpha2
environments:
  invalid:
    containerd:
      root: relative/root
    runtimes:
      container: {}
`))
	require.ErrorContains(t, err, "containerd.root must be an absolute path")
}

func TestParseRequiresExplicitConfigVersion(t *testing.T) {
	_, err := Parse([]byte(`
environments:
  default:
    runtimes:
      container: {}
`))
	require.ErrorContains(t, err, "config version is required")
}

func TestParseRejectsUnknownConfigVersion(t *testing.T) {
	_, err := Parse([]byte(`
version: v9
environments:
  default:
    runtimes:
      container: {}
`))
	require.ErrorContains(t, err, `unsupported runtime environment config version "v9"`)
}

func TestParseRejectsLegacyEnvironmentSnapshotterField(t *testing.T) {
	_, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    containerd:
      snapshotter: overlayfs
    runtimes:
      container: {}
`))
	require.ErrorContains(t, err, `unknown field "snapshotter"`)
}

func TestRuntimeBindingOverridesEnvironmentDefaultSnapshotter(t *testing.T) {
	config, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    containerd:
      defaultSnapshotter: overlayfs
    runtimes:
      kata-fc:
        snapshotter: blockfile
`))
	require.NoError(t, err)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	require.Equal(t, "blockfile", plan.Profile.Containerd.Snapshotter)
	require.Equal(t, "blockfile", plan.Containerd.Snapshotter)
}

func TestRuntimeBindingOverridesFastletOverhead(t *testing.T) {
	config, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    runtimes:
      container:
        fastletOverhead:
          cpu: 350m
          memory: 384Mi
`))
	require.NoError(t, err)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	cpu := plan.Profile.Deployment.Overhead[corev1.ResourceCPU]
	memory := plan.Profile.Deployment.Overhead[corev1.ResourceMemory]
	require.Equal(t, "350m", cpu.String())
	require.Equal(t, "384Mi", memory.String())
}

func TestParseRejectsIncompleteFastletOverhead(t *testing.T) {
	_, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    runtimes:
      container:
        fastletOverhead:
          cpu: 100m
`))
	require.ErrorContains(t, err, "requires non-negative cpu and memory")
}

func TestFirecrackerBindingOverridesPathsAndHostPaths(t *testing.T) {
	config, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    containerd:
      defaultSnapshotter: overlayfs
    runtimes:
      firecracker:
        firecracker:
          binaryPath: /opt/fast-sandbox/firecracker/firecracker
          jailerPath: /opt/fast-sandbox/firecracker/jailer
          kernelPath: /opt/fast-sandbox/firecracker/vmlinux.bin
          rootfsPath: /srv/fast-sandbox/firecracker/rootfs
          stateRoot: /srv/fast-sandbox/firecracker
`))
	require.NoError(t, err)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.NotNil(t, plan.Profile.Firecracker)
	require.Equal(t, "/opt/fast-sandbox/firecracker/firecracker", plan.Profile.Firecracker.BinaryPath)
	require.Equal(t, "/opt/fast-sandbox/firecracker/jailer", plan.Profile.Firecracker.JailerPath)
	require.Equal(t, "/opt/fast-sandbox/firecracker/vmlinux.bin", plan.Profile.Firecracker.KernelPath)
	// An operator-pinned kernelPath is mounted into the fastlet pod.
	require.True(t, hasHostPath(plan.Profile, "/opt/fast-sandbox/firecracker/vmlinux.bin"))
	require.Equal(t, "/srv/fast-sandbox/firecracker/rootfs", plan.Profile.Firecracker.RootfsPath)
	require.Equal(t, "/srv/fast-sandbox/firecracker", plan.Profile.Firecracker.StateRoot)
	// The Deployment.HostPaths follow the overridden paths, not the builtin
	// defaults; the jailer requirement appears because the override set it.
	require.False(t, hasHostPath(plan.Profile, "/usr/local/bin/firecracker"))
	require.True(t, hasHostPath(plan.Profile, "/opt/fast-sandbox/firecracker/firecracker"))
	require.True(t, hasHostPath(plan.Profile, "/opt/fast-sandbox/firecracker/jailer"))
	require.True(t, hasHostPath(plan.Profile, "/srv/fast-sandbox/firecracker"))
	require.False(t, hasHostPath(plan.Profile, "/srv/fast-sandbox/firecracker/overlaybd-cache"))
	require.False(t, hasHostPath(plan.Profile, "/var/lib/fast-sandbox/firecracker/overlaybd-cache"))
}

func TestFirecrackerBindingKeepsBuiltinDefaultsWhenEmpty(t *testing.T) {
	config, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    containerd:
      defaultSnapshotter: overlayfs
    runtimes:
      firecracker:
        firecracker:
          jailerPath: /usr/local/bin/jailer
`))
	require.NoError(t, err)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.Equal(t, "/usr/local/bin/firecracker", plan.Profile.Firecracker.BinaryPath)
	require.Equal(t, "/usr/local/bin/jailer", plan.Profile.Firecracker.JailerPath)
	// The builtin profile carries no node-side kernel (#84): an empty
	// kernelPath mounts nothing.
	require.Empty(t, plan.Profile.Firecracker.KernelPath)
	require.False(t, hasHostPath(plan.Profile, "/opt/fast-sandbox/firecracker/vmlinux.bin"))
	require.True(t, hasHostPath(plan.Profile, "/usr/local/bin/firecracker"))
	require.True(t, hasHostPath(plan.Profile, "/usr/local/bin/jailer"))
}

func TestFirecrackerBindingRejectsRelativePaths(t *testing.T) {
	_, err := Parse([]byte(`
version: v1alpha2
environments:
  default:
    containerd:
      defaultSnapshotter: overlayfs
    runtimes:
      firecracker:
        firecracker:
          jailerPath: relative/jailer
`))
	require.ErrorContains(t, err, "firecracker.jailerPath must be absolute")
}

func TestDecodePlanRejectsMutation(t *testing.T) {
	plan, err := ResolveDefault(runtimecatalog.Builtin(), apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	plan.Containerd.Namespace = "other"
	payload, err := plan.Marshal()
	require.NoError(t, err)
	_, err = DecodePlan(bytes.NewReader(payload))
	require.ErrorContains(t, err, "does not match payload")
}

func hasHostPath(profile runtimecatalog.RuntimeProfile, hostPath string) bool {
	for _, requirement := range profile.Deployment.HostPaths {
		if requirement.HostPath == hostPath {
			return true
		}
	}
	return false
}
