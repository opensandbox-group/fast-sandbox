package infra

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

func TestPrepareAndRecoverInstanceUsesFencedPrivateConfig(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	profile, err := infracatalog.Builtin().Resolve("test-infra")
	require.NoError(t, err)
	manager, err := NewManagerWithConfig(ManagerConfig{
		Catalog: infracatalog.Builtin(), RuntimeProfile: runtimeProfile, ProfileName: profile.Name,
		ExpectedProfileHash: profile.ProfileHash, Store: store, Resolver: NewPlatformResolver(nil), SandboxInitPath: sandboxInit,
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &api.SandboxSpec{
		SandboxID: "uid-a", InstanceGeneration: 2, AssignmentAttempt: 3,
		InfraProfile: profile.Name, InfraProfileHash: profile.ProfileHash,
	}
	instance, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, instance.WrapperRequired)
	require.Len(t, instance.Services, 1)
	require.NotEmpty(t, instance.UpstreamHeaders[UpstreamTokenHeader])
	require.FileExists(t, instance.ConfigPodPath)
	info, err := os.Stat(instance.ConfigPodPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0400), info.Mode().Perm())

	recovered, err := manager.RecoverInstance(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, instance.UpstreamHeaders, recovered.UpstreamHeaders)
	stale := *spec
	stale.AssignmentAttempt++
	_, err = manager.RecoverInstance(context.Background(), &stale)
	require.Error(t, err)

	next := *spec
	next.InstanceGeneration++
	nextInstance, err := manager.PrepareInstance(context.Background(), &next)
	require.NoError(t, err)
	require.NotEqual(t, instance.UpstreamHeaders[UpstreamTokenHeader], nextInstance.UpstreamHeaders[UpstreamTokenHeader], "reset generation must fence the old Infra credential")
}
