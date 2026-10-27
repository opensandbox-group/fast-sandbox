package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestDisabledOCIRejectsCreateResumeAndWarmPull(t *testing.T) {
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "false")
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const image = "oci://r.example/ns/template@sha256:" + digest
	for _, cached := range []bool{false, true} {
		for _, resume := range []bool{false, true} {
			fixture := newDriverFixture(t)
			fixture.sandboxSpec.Desired.Image = image
			restore := &fastletapi.RestoreSpec{ManifestRef: image, ArtifactDigest: digest}
			if resume {
				fixture.sandboxSpec.Desired.Image = "native:template"
				fixture.sandboxSpec.Desired.Restore = restore
			}
			if cached {
				file := cachedManifestPath(fixture.stateRoot, image)
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0750))
				require.NoError(t, os.WriteFile(file, []byte(`{"oci":{"rootfs":"r.example/ns/t@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapfiles":"r.example/ns/t@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`), 0600))
				require.NoError(t, verifyRestorableImage(fixture.stateRoot, image))
			}
			_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, "FAST_SANDBOX_OCI_ENABLED=true")
			if resume {
				_, err = fixture.driver.DeliverCheckpoint(context.Background(), restore)
			} else {
				_, err = fixture.driver.DeliverImage(context.Background(), image)
			}
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, "FAST_SANDBOX_OCI_ENABLED=true")
			err = fixture.driver.PullImage(context.Background(), image)
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, "FAST_SANDBOX_OCI_ENABLED=true")
			require.Empty(t, fixture.driver.imageDeliveries)
			require.Empty(t, fixture.launcher.started)
			require.Zero(t, fixture.manager.Snapshot().Bound)
		}
	}
}

func TestOCISupportDefaultsAndNativeIndependence(t *testing.T) {
	for _, enabled := range []string{"", "false", "true", "invalid"} {
		t.Run(enabled, func(t *testing.T) {
			t.Setenv("FAST_SANDBOX_OCI_ENABLED", enabled)
			err := validateOCISupport("oci://r.example/ns/template:build")
			if enabled != "true" {
				require.ErrorIs(t, err, ErrInvalidConfig)
			} else {
				require.NoError(t, err)
			}
			fixture := newDriverFixture(t)
			fixture.prepareCachedImage(t, fixture.sandboxSpec.Desired.Image)
			status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Desired.Image)
			require.NoError(t, err)
			require.Equal(t, runtimecontract.ImageDelivered, status)
			require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Desired.Image))
			require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
			_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.NoError(t, err)
			require.Len(t, fixture.launcher.started, 1)
		})
	}
}

func TestOCIDeliveryNeedsNeitherAgentNorOuterCache(t *testing.T) {
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "true")
	root := t.TempDir()
	image := "oci://r.example/ns/template@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, verifyRestorableImage(root, image))
	fixture := newDriverFixture(t)
	agent := &fakeAgentClient{}
	fixture.driver.newAgentClient = func(string) (AgentClient, error) {
		t.Fatal("OCI delivery must not contact the node agent")
		return agent, nil
	}
	fixture.driver.agentSocket = "/absent-agent.sock"
	require.NoError(t, fixture.driver.PullImage(context.Background(), image))
	status, err := fixture.driver.DeliverImage(context.Background(), image)
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivered, status)
	status, err = fixture.driver.DeliverCheckpoint(context.Background(), &fastletapi.RestoreSpec{ManifestRef: image, ArtifactDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	require.NoError(t, err)
	require.Equal(t, runtimecontract.ImageDelivered, status)
	fixture.driver.releaseAgentSandbox(context.Background(), "sandbox-1", image)
	require.NoDirExists(t, filepath.Join(fixture.stateRoot, imageCacheDir))
}

func TestEnsureSandboxRejectsInvalidCLIMetadataBeforeAcquiringSlot(t *testing.T) {
	for _, payload := range []string{`{broken`, `{}`, `{"oci":{"rootfs":"r.example/ns/t:mutable"}}`} {
		t.Run(payload, func(t *testing.T) {
			fixture := newDriverFixture(t)
			require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
			fixture.sandboxSpec.Desired.Image = "oci://r.example/ns/t@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			installPreparedOCI(t, fixture, payload)
			_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.Error(t, err)
			require.Empty(t, fixture.launcher.started)
			require.Equal(t, 0, fixture.manager.Snapshot().Bound)
			require.NoDirExists(t, filepath.Join(fixture.stateRoot, "oci/sandboxes/sandbox-1"))
		})
	}
}

func TestOCIBackingPathsArePrivateToFastletPod(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.driver.SetFastletPodUID("pod-a")
	first := fixture.driver.fsbArgs()
	fixture.driver.SetFastletPodUID("pod-b")
	second := fixture.driver.fsbArgs()
	require.Contains(t, first, filepath.Join(fixture.stateRoot, "oci/pods/pod-a/sandboxes"))
	require.Contains(t, second, filepath.Join(fixture.stateRoot, "oci/pods/pod-b/sandboxes"))
	require.NotContains(t, second, filepath.Join(fixture.stateRoot, "oci/pods/pod-a/sandboxes"))
	require.Contains(t, first, "--reject-credential-conflicts")
	cache := filepath.Join(fixture.stateRoot, "oci/manifest-cache")
	require.Contains(t, first, cache)
	require.Contains(t, second, cache)
}

func TestOCIPackRecoveryDoesNotShareReplacementPodStaging(t *testing.T) {
	root := t.TempDir()
	require.NotEqual(t, snapshotStagingPath(root, "old-pod", "same-snapshot", true), snapshotStagingPath(root, "new-pod", "same-snapshot", true))
	require.Equal(t, snapshotStagingPath(root, "old-pod", "same-snapshot", false), snapshotStagingPath(root, "new-pod", "same-snapshot", false))
}
