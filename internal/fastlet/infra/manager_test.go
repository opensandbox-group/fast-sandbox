package infra

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

func TestManagerPreparesProfileAndSupervisorOnce(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pod"), 0755))
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
	plan, err := manager.Plan()
	require.NoError(t, err)
	require.NotNil(t, plan.Supervisor)
	require.Len(t, plan.Components, 1)
	require.NotNil(t, plan.Components[0].Artifact)
	require.NoError(t, manager.Prepare(context.Background()))
	require.Len(t, manager.ArtifactReferences(), 2)
}

func TestManagerRetriesTransientArtifactPreparationFailure(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	resolver := &transientResolver{delegate: NewPlatformResolver(nil)}
	manager, err := NewManagerWithConfig(ManagerConfig{
		Catalog: infracatalog.Builtin(), RuntimeProfile: runtimeProfile, ProfileName: "test-infra",
		Store: store, Resolver: resolver, SandboxInitPath: sandboxInit,
	})
	require.NoError(t, err)
	require.Error(t, manager.Prepare(context.Background()))
	require.NoError(t, manager.Prepare(context.Background()))
	_, err = manager.Plan()
	require.NoError(t, err)
	require.Equal(t, 2, resolver.calls)
}

func TestManagerStagesArtifactVolumeForBoxLiteSidecar(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	sandboxTunnel := filepath.Join(root, "sandbox-tunnel")
	require.NoError(t, os.WriteFile(sandboxTunnel, []byte("sandbox-tunnel"), 0555))
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeBoxLite)
	require.NoError(t, err)
	catalog, err := infracatalog.New([]infracatalog.Profile{{
		Name: "boxlite-artifact-volume", Version: "v1", Configured: true,
		AllowedRuntimes: []apiv1alpha1.RuntimeName{apiv1alpha1.RuntimeBoxLite},
		Components: []infracatalog.Component{{
			Name: "helper", Artifact: infracatalog.Artifact{
				SourceType: infracatalog.SourceEmbedded, Reference: "embedded://test-infra/v1",
				Digest: infracatalog.TestInfraDigest(), Executable: true,
			},
			ContainerPath: "/.fast/infra/helper",
			DeliveryModes: []runtimecatalog.InfraDeliveryMode{runtimecatalog.InfraDeliveryArtifactVolume},
			Activation: infracatalog.Activation{
				Mode: infracatalog.ActivationComponentBootstrap, Command: "/.fast/infra/helper",
			},
			Required: true,
		}},
	}})
	require.NoError(t, err)
	manager, err := NewManagerWithConfig(ManagerConfig{
		Catalog: catalog, RuntimeProfile: runtimeProfile, ProfileName: "boxlite-artifact-volume",
		Store: store, Resolver: NewPlatformResolver(nil), SandboxInitPath: sandboxInit, SandboxTunnelPath: sandboxTunnel,
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare(context.Background()))
	plan, err := manager.Plan()
	require.NoError(t, err)
	require.Len(t, plan.Components, 1)
	require.Equal(t, runtimecatalog.InfraDeliveryArtifactVolume, plan.Components[0].Plan.DeliveryMode)
	require.NotNil(t, plan.Components[0].Artifact)
	require.FileExists(t, plan.Components[0].Artifact.PodPath)
	require.NotNil(t, plan.Supervisor)
	require.NotNil(t, plan.Tunnel)
	require.FileExists(t, plan.Tunnel.PodPath)
	require.Len(t, manager.ArtifactReferences(), 3)
}

type transientResolver struct {
	delegate ArtifactResolver
	calls    int
}

func (r *transientResolver) Open(ctx context.Context, artifact infracatalog.Artifact) (io.ReadCloser, error) {
	r.calls++
	if r.calls == 1 {
		return nil, errors.New("temporary registry failure")
	}
	return r.delegate.Open(ctx, artifact)
}
