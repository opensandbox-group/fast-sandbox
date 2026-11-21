package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// checkpointTestDigest is a fixed artifact digest so the checkpoint reference
// (and therefore the local cache key) is stable across subtests.
const checkpointTestDigest = "abababababababababababababababababababababababababababababababab"

// TestValidateCheckpointResumeOwner covers the ownership boundary of
// checkpoint artifact sets: an instance-private checkpoint restores only
// under the Sandbox identity recorded in its commit-point manifest, and an
// owner-carrying set is never usable as a fresh boot image.
func TestValidateCheckpointResumeOwner(t *testing.T) {
	resume := &fastletapi.RuntimeSandboxConfig{
		Identity: fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", Namespace: "tenant-a"},
		Desired:  fastletapi.SandboxDesiredSpec{Restore: &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/publish/abababababababab/manifest.json", ArtifactDigest: "abababababababababababababababababababababababababababababababab"}},
	}
	freshBoot := *resume
	freshBoot.Desired.Restore = nil

	cases := []struct {
		name       string
		owner      *artifacts.CheckpointOwner
		config     *fastletapi.RuntimeSandboxConfig
		wantErrMsg string
	}{
		{name: "matching owner resumes", owner: &artifacts.CheckpointOwner{SandboxUID: "sandbox-a", Namespace: "tenant-a"}, config: resume},
		{name: "no owner and no restore is a template boot", owner: nil, config: &freshBoot},
		{name: "cross-tenant sandbox uid is rejected", owner: &artifacts.CheckpointOwner{SandboxUID: "sandbox-b", Namespace: "tenant-a"}, config: resume, wantErrMsg: "checkpoint belongs to a different sandbox"},
		{name: "cross-tenant namespace is rejected", owner: &artifacts.CheckpointOwner{SandboxUID: "sandbox-a", Namespace: "tenant-b"}, config: resume, wantErrMsg: "checkpoint belongs to a different sandbox"},
		{name: "checkpoint as a fresh boot image is rejected", owner: &artifacts.CheckpointOwner{SandboxUID: "sandbox-a", Namespace: "tenant-a"}, config: &freshBoot, wantErrMsg: "cannot boot a fresh sandbox"},
		{name: "resume without an owner is not a checkpoint", owner: nil, config: resume, wantErrMsg: "resume reference is not a checkpoint"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			document := artifacts.VMManifest{}
			if test.owner != nil {
				document.Platform = &artifacts.VMPlatform{Checkpoint: test.owner}
			}
			err := validateCheckpointResumeOwner(document, test.config)
			if test.wantErrMsg == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, test.wantErrMsg)
		})
	}
	// A nil Platform is the template manifest shape.
	require.NoError(t, validateCheckpointResumeOwner(artifacts.VMManifest{}, &freshBoot))
	require.ErrorIs(t, validateCheckpointResumeOwner(artifacts.VMManifest{}, resume), ErrInvalidConfig)
}

// ownerManifest returns a valid manifest document that records the given
// checkpoint owner.
func ownerManifest(sandboxUID, namespace string) artifacts.VMManifest {
	document := newTestVMManifest()
	document.Platform = &artifacts.VMPlatform{
		Checkpoint: &artifacts.CheckpointOwner{SandboxUID: sandboxUID, Namespace: namespace},
	}
	return document
}

// seedCheckpointCache commits a complete checkpoint artifact set into the
// node-local cache under the canonical checkpoint reference of digest.
func seedCheckpointCache(t *testing.T, fixture *driverFixture, digest string, document artifacts.VMManifest) string {
	t.Helper()
	reference := artifacts.CheckpointReference(digest)
	seedArtifactSet(t, fixture, reference, document)
	return reference
}

// seedArtifactSet writes a complete native artifact set (rootfs, vmstate,
// memory, commit-point manifest) under the cache key of reference.
func seedArtifactSet(t *testing.T, fixture *driverFixture, reference string, document artifacts.VMManifest) {
	t.Helper()
	dir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(reference))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate-snapshot-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory-snapshot-data"), 0o640))
	payload, err := artifacts.MarshalVMManifest(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, snapshotManifestName), payload, 0o640))
}

func resumeSpec(spec fastletapi.RuntimeSandboxConfig, digest string) fastletapi.RuntimeSandboxConfig {
	spec.Desired.Restore = &fastletapi.RestoreSpec{
		ManifestRef:    "s3://bucket/publish/abababababababab/manifest.json",
		ArtifactDigest: digest,
	}
	return spec
}

// TestEnsureSandboxResumeCheckpointOwnership covers the native artifact-store
// resume path: the checkpoint manifest binds the set to its producing Sandbox
// and the restore verifies the binding before any memory is loaded.
func TestEnsureSandboxResumeCheckpointOwnership(t *testing.T) {
	t.Run("own checkpoint resumes", func(t *testing.T) {
		fixture := newDriverFixture(t)
		seedCheckpointCache(t, fixture, checkpointTestDigest, ownerManifest("sandbox-1", "tenant-a"))
		require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

		spec := resumeSpec(fixture.sandboxSpec, checkpointTestDigest)
		metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&spec))
		require.NoError(t, err)
		require.Equal(t, string(PhaseRunning), metadata.Phase)

		loads := fixture.server.recordedSnapshotLoads()
		require.Len(t, loads, 1)
		require.Equal(t,
			filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(artifacts.CheckpointReference(checkpointTestDigest)), memorySnapshotName),
			loads[0].MemBackend.BackendPath)
	})

	t.Run("another tenant's checkpoint is rejected before any memory load", func(t *testing.T) {
		fixture := newDriverFixture(t)
		seedCheckpointCache(t, fixture, checkpointTestDigest, ownerManifest("sandbox-2", "tenant-b"))
		require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

		spec := resumeSpec(fixture.sandboxSpec, checkpointTestDigest)
		_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&spec))
		require.ErrorIs(t, err, ErrInvalidConfig)
		require.ErrorContains(t, err, "checkpoint belongs to a different sandbox")
		require.Empty(t, fixture.launcher.started)
		require.Empty(t, fixture.server.recordedSnapshotLoads())
		require.Equal(t, 0, fixture.manager.Snapshot().Bound)
	})

	t.Run("checkpoint set cannot boot a fresh sandbox", func(t *testing.T) {
		fixture := newDriverFixture(t)
		seedArtifactSet(t, fixture, fixture.sandboxSpec.Desired.Image, ownerManifest("sandbox-1", "tenant-a"))
		require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

		_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
		require.ErrorIs(t, err, ErrInvalidConfig)
		require.ErrorContains(t, err, "cannot boot a fresh sandbox")
		require.Empty(t, fixture.launcher.started)
	})
}
