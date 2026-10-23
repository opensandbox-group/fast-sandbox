package infra

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/sandbox/supervisor"
)

func TestPrepareAndRecoverInstanceUsesFencedPrivateConfigWithoutComponentToken(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeContainer)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.RuntimeSandboxConfig{
		Desired:  fastletapi.SandboxDesiredSpec{InfraRevision: manager.Revision()},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 2, AssignmentAttempt: 3},
	}
	instance, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, instance.WrapperRequired)
	require.Len(t, instance.Services, 1)
	require.Len(t, instance.Mounts, 4, "sandbox-init, two artifact mappings, and private config")
	require.FileExists(t, instance.ConfigPodPath)

	configFile, err := os.Open(instance.ConfigPodPath)
	require.NoError(t, err)
	var initConfig supervisor.Config
	require.NoError(t, json.NewDecoder(configFile).Decode(&initConfig))
	require.NoError(t, configFile.Close())
	require.Len(t, initConfig.Components, 1)
	require.Equal(t, "production", initConfig.Components[0].Env["MODE"])
	require.NotContains(t, initConfig.Components[0].Env, "EXECD_ACCESS_TOKEN")
	require.NotContains(t, initConfig.Components[0].Env, "FAST_SANDBOX_INTERNAL_TOKEN")
	info, err := os.Stat(instance.ConfigPodPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0400), info.Mode().Perm())

	recovered, err := manager.RecoverInstance(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, instance, recovered)
	stale := *spec
	stale.Identity.AssignmentAttempt++
	_, err = manager.RecoverInstance(context.Background(), &stale)
	require.Error(t, err)
}

func TestPrepareInstanceExcludesHostProcessComponentFromSupervisorConfig(t *testing.T) {
	manager, _ := testHostProcessManager(t)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.RuntimeSandboxConfig{
		Desired:  fastletapi.SandboxDesiredSpec{InfraRevision: manager.Revision()},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	instance, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	require.False(t, instance.WrapperRequired)
	require.Len(t, instance.Services, 1)
	require.True(t, instance.Services[0].HostProcess)
	require.Equal(t, "egress", instance.Services[0].Component)
	require.Equal(t, uint32(18080), instance.Services[0].Port)
	require.Empty(t, instance.Mounts, "host-process-only plans deliver nothing into the guest rootfs")

	configFile, err := os.Open(instance.ConfigPodPath)
	require.NoError(t, err)
	var initConfig supervisor.Config
	require.NoError(t, json.NewDecoder(configFile).Decode(&initConfig))
	require.NoError(t, configFile.Close())
	require.Empty(t, initConfig.Components, "host-process components must not appear in the guest supervisor config")

	recovered, err := manager.RecoverInstance(context.Background(), spec)
	require.NoError(t, err)
	require.Len(t, recovered.Services, 1)
	require.True(t, recovered.Services[0].HostProcess)
}

func TestPrepareInstanceMixedGuestAndHostProcessComponents(t *testing.T) {
	root := t.TempDir()
	podRoot := filepath.Join(root, "pod")
	hostRoot := filepath.Join(root, "host")
	sourcePodRoot := filepath.Join(podRoot, "source")
	sourceHostRoot := filepath.Join(hostRoot, "source")
	require.NoError(t, os.MkdirAll(filepath.Join(sourcePodRoot, "config"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sourcePodRoot, "execd"), []byte("execd"), 0555))
	store, err := NewArtifactStore(podRoot, hostRoot)
	require.NoError(t, err)
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	profile := runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeFirecracker, NetworkMode: runtimecatalog.NetworkModeFirecracker,
		InfraDeliveryModes: []runtimecatalog.InfraDeliveryMode{
			runtimecatalog.InfraDeliveryGuestCopy, runtimecatalog.InfraDeliveryHostProcess,
		},
	}
	execd := apiv1alpha2.InfraComponent{
		Name: "execd",
		Artifact: &apiv1alpha2.InfraArtifact{
			Source: apiv1alpha2.InfraArtifactSource{Image: &apiv1alpha2.InfraArtifactImage{
				Reference: "registry.example/execd@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
			Mappings: []apiv1alpha2.InfraArtifactMapping{{SourcePath: "/execd", TargetPath: "/.fast/components/execd/execd"}},
		},
		Process: apiv1alpha2.InfraProcess{
			Command:     []string{"/.fast/components/execd/execd"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{TCPConnect: &apiv1alpha2.InfraTCPConnect{}},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}
	egress := apiv1alpha2.InfraComponent{
		Name:     "egress",
		Delivery: apiv1alpha2.InfraDeliveryHostProcess,
		Process: apiv1alpha2.InfraProcess{
			Command:     []string{"/bin/egress"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{TCPConnect: &apiv1alpha2.InfraTCPConnect{}},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 18080},
	}
	plan, err := infracatalog.Compile([]apiv1alpha2.InfraComponent{execd, egress}, profile)
	require.NoError(t, err)
	digest := plan.Components[0].Artifact.Source.Digest
	manager, err := NewManagerWithConfig(ManagerConfig{
		Plan: plan, RuntimeProfile: profile, Store: store,
		Resolver: &testResolver{source: PreparedSource{
			Digest: digest, PodRoot: sourcePodRoot, HostRoot: sourceHostRoot,
		}},
		SandboxInitPath: sandboxInit,
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare(context.Background()))

	spec := &fastletapi.RuntimeSandboxConfig{
		Desired:  fastletapi.SandboxDesiredSpec{InfraRevision: manager.Revision()},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	instance, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, instance.WrapperRequired)
	require.Len(t, instance.Services, 2)
	require.False(t, instance.Services[0].HostProcess)
	require.True(t, instance.Services[1].HostProcess)

	configFile, err := os.Open(instance.ConfigPodPath)
	require.NoError(t, err)
	var initConfig supervisor.Config
	require.NoError(t, json.NewDecoder(configFile).Decode(&initConfig))
	require.NoError(t, configFile.Close())
	require.Len(t, initConfig.Components, 1, "only the guest component enters the supervisor config")
	require.Equal(t, "execd", initConfig.Components[0].Name)
}

func TestPrepareInstanceRejectsWrongImmutableRevision(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeContainer)
	require.NoError(t, manager.Prepare(context.Background()))
	_, err := manager.PrepareInstance(context.Background(), &fastletapi.RuntimeSandboxConfig{
		Desired:  fastletapi.SandboxDesiredSpec{InfraRevision: "sha256:stale"},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	})
	require.ErrorContains(t, err, "revision")
}
