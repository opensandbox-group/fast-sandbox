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
