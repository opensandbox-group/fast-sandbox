package firecracker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/auth"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestCheckpointDestination(t *testing.T) {
	dest, err := checkpointDestination("r.example/opensandbox-snapshot/checkpoints", "abc")
	require.NoError(t, err)
	require.Equal(t, "r.example/opensandbox-snapshot/checkpoints:abc", dest)
	for _, value := range []string{"", "r.example/ns/r:mutable", "r.example/ns/r@sha256:abc", "http://r.example/ns/r"} {
		_, err = checkpointDestination(value, "abc")
		require.Error(t, err, value)
	}
}
func TestOCIResumeRejectsAnotherSandboxAndTemplateUse(t *testing.T) {
	ref := "oci://r.example/ns/r@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	config := &fastletapi.RuntimeSandboxConfig{}
	config.Identity.Namespace = "test"
	config.Identity.SandboxUID = "owner"
	config.Desired.Restore = &fastletapi.RestoreSpec{ManifestRef: ref, ArtifactDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	raw, err := json.Marshal(map[string]any{"platform": map[string]any{"checkpoint": map[string]string{"sandboxUID": "owner", "namespace": "test"}}})
	require.NoError(t, err)
	var metadata ociRestoreMetadata
	require.NoError(t, json.Unmarshal(raw, &metadata))
	require.NoError(t, validateOCIResumeOwner(metadata, config))
	config.Identity.SandboxUID = "other"
	require.ErrorIs(t, validateOCIResumeOwner(metadata, config), ErrInvalidConfig)
	config.Identity.SandboxUID = "owner"
	config.Desired.Restore = nil
	require.ErrorIs(t, validateOCIResumeOwner(metadata, config), ErrInvalidConfig)
}
func TestOCICheckpointRestoreReferenceBoundToDigest(t *testing.T) {
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := "oci://r.example/ns/r@sha256:" + sum
	spec := fastletapi.SandboxDesiredSpec{Restore: &fastletapi.RestoreSpec{ManifestRef: ref, ArtifactDigest: sum}}
	got, err := restoreReference(spec)
	require.NoError(t, err)
	require.Equal(t, ref, got)
	spec.Restore.ArtifactDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err = restoreReference(spec)
	require.ErrorIs(t, err, ErrInvalidConfig)
	spec.Restore.ManifestRef = "oci://r.example/ns/r:tag"
	_, err = restoreReference(spec)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestPrepareOCISnapshotKeepsNativeFlowIndependentOfRegistry(t *testing.T) {
	dir := t.TempDir()
	state := &SandboxState{}
	state.Config.Identity.SandboxUID = "sandbox-1"
	state.Config.Identity.Namespace = "test"
	state.Phase = PhaseRunning
	state.Config.Desired.Image = "native:golden"
	require.NoError(t, saveState(dir, state))
	plan := dumpPlan{sandboxDir: dir, snapshotID: "checkpoint-id"}
	enabled, err := prepareOCISnapshot(plan)
	require.NoError(t, err)
	require.False(t, enabled)
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "true")
	state.Config.Desired.Image = "oci://registry.example/templates/alpine:build"
	require.NoError(t, saveState(dir, state))
	t.Setenv(ociSnapshotRepositoryEnv, "")
	destination, err := checkpointDestinationForSource(state.Config.Desired.Image, plan.snapshotID)
	require.NoError(t, err)
	require.Equal(t, "registry.example/templates/alpine:checkpoint-id", destination)
	enabled, err = prepareOCISnapshot(plan)
	require.NoError(t, err)
	require.True(t, enabled, "default OCI pause must not require an extra environment variable")
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots/checkpoints")
	destination, err = checkpointDestinationForSource(state.Config.Desired.Image, plan.snapshotID)
	require.NoError(t, err)
	require.Equal(t, "registry.example/snapshots/checkpoints:checkpoint-id", destination)
	enabled, err = prepareOCISnapshot(plan)
	require.NoError(t, err)
	require.True(t, enabled)
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots/checkpoints:mutable")
	_, err = prepareOCISnapshot(plan)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestDisabledOCIRejectsLiveSnapshotBeforeDump(t *testing.T) {
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "false")
	fixture := newSnapshotDriverFixture(t)
	fixture.sandboxSpec.Desired.Image = "oci://r.example/ns/template:base"
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	_, err := fixture.driver.CreateSnapshot(t.Context(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "live-snapshot", Kind: fastletapi.SnapshotKindTemplate,
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "FAST_SANDBOX_OCI_ENABLED=true")
	require.Empty(t, fixture.server.snapshotDumps)
	require.True(t, fixture.server.running)
}

func TestCheckpointCredentialsSeparateParentReadAndDestinationWrite(t *testing.T) {
	installOCIRegistryConfig(t,
		registryconfig.Credential{Host: "r.example", RepositoryPrefix: "templates", Username: "parent-reader", Password: "read-secret"},
		registryconfig.Credential{Host: "other.example", RepositoryPrefix: "templates", Username: "other-reader", Password: "other-secret"},
		registryconfig.Credential{Host: "r.example", RepositoryPrefix: "checkpoints", IdentityToken: "checkpoint-read-token", WriteUsername: "writer", WritePassword: "write-secret"},
	)
	credentials, err := checkpointCredentials("r.example/checkpoints/sandbox:pause")
	require.NoError(t, err)
	for _, test := range []struct {
		reference string
		want      auth.Credential
	}{
		{"r.example/checkpoints/sandbox:pause_rootfs", auth.Credential{Username: "writer", Password: "write-secret"}},
		{"r.example/templates/base:rootfs", auth.Credential{Username: "parent-reader", Password: "read-secret"}},
		{"other.example/templates/base:rootfs", auth.Credential{Username: "other-reader", Password: "other-secret"}},
		{"public.example/base:rootfs", auth.EmptyCredential},
	} {
		got, err := credentials(context.Background(), test.reference)
		require.NoError(t, err)
		require.Equal(t, test.want, got, test.reference)
	}
	_, err = checkpointCredentials("missing.example/checkpoints/sandbox:pause")
	require.ErrorContains(t, err, "no registry credential")
}

func TestOCITokenOnlyCredentialsFailBeforePacking(t *testing.T) {
	installOCIRegistryConfig(t, registryconfig.Credential{Host: "r.example", IdentityToken: "private-token"})
	_, err := checkpointCredentials("r.example/checkpoints/sandbox:pause")
	require.ErrorContains(t, err, "identitytoken-only credentials are not supported")
}
