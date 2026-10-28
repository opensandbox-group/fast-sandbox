package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// longSandboxUID is a full 36-character Sandbox CR UID; the jail directory
// name only carries its first 32 characters.
const longSandboxUID = "6f9c1e8a-3b2d-4c5e-8f70-a1b2c3d4e5f6"

// prepareBareCachedImage seeds an image cache without a manifest, which is
// admitted without CPU provenance checks and keeps the test host-independent.
func (f *driverFixture) prepareBareCachedImage(t *testing.T, image string) {
	t.Helper()
	dir := filepath.Join(f.stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate-snapshot-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory-snapshot-data"), 0o640))
}

// awaitVMMetadataForTest polls until the record parses, matching the
// consumer contract of retrying on a short JSON prefix.
func awaitVMMetadataForTest(t *testing.T, path string) VMMetadata {
	t.Helper()
	var decoded VMMetadata
	require.Eventually(t, func() bool {
		payload, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		return json.Unmarshal(payload, &decoded) == nil
	}, 5*time.Second, 10*time.Millisecond, "the asynchronous discovery record must appear after provisioning")
	return decoded
}

func TestWriteVMMetadataFileSchema(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, writeVMMetadataFile(directory, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID,
		Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 2,
	}))
	path := filepath.Join(directory, vmMetadataName)
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	require.Equal(t, float64(1), fields["version"])
	require.Equal(t, longSandboxUID, fields["uid"])
	require.Equal(t, "team-a", fields["namespace"])
	require.Equal(t, "sbx-7d9f2c", fields["name"])
	require.Equal(t, float64(2), fields["placementAttempt"])
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0640), info.Mode().Perm())
}

func TestWriteVMMetadataFileOmitsZeroPlacementAttempt(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, writeVMMetadataFile(directory, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID, Namespace: "team-a", Name: "sbx-7d9f2c",
	}))
	payload, err := os.ReadFile(filepath.Join(directory, vmMetadataName))
	require.NoError(t, err)
	require.NotContains(t, string(payload), "placementAttempt")
}

func TestWriteVMMetadataFileIsWriteOnce(t *testing.T) {
	directory := t.TempDir()
	first := VMMetadata{Version: vmMetadataVersion, UID: longSandboxUID, Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 1}
	require.NoError(t, writeVMMetadataFile(directory, first))
	stale := VMMetadata{Version: vmMetadataVersion, UID: "aaaaaaaa-3b2d-4c5e-8f70-a1b2c3d4e5f6", Namespace: "team-b", Name: "sbx-other", PlacementAttempt: 9}
	require.NoError(t, writeVMMetadataFile(directory, stale))
	payload, err := os.ReadFile(filepath.Join(directory, vmMetadataName))
	require.NoError(t, err)
	var decoded VMMetadata
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, first, decoded)
}

func TestWriteVMMetadataFileRejectsInvalid(t *testing.T) {
	require.ErrorIs(t, writeVMMetadataFile("", VMMetadata{UID: longSandboxUID}), ErrInvalidConfig)
	require.ErrorIs(t, writeVMMetadataFile(t.TempDir(), VMMetadata{}), ErrInvalidConfig)
}

func TestEnsureSandboxWritesVMMetadataInJail(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.enableJailer()
	fixture.sandboxSpec.Identity.SandboxUID = longSandboxUID
	fixture.sandboxSpec.Identity.Namespace = "team-a"
	fixture.sandboxSpec.Identity.Name = "sbx-7d9f2c"
	fixture.prepareBareCachedImage(t, fixture.sandboxSpec.Desired.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	truncatedID := truncatedSandboxID(longSandboxUID)
	require.Len(t, truncatedID, 32)
	path := filepath.Join(jailerRoot(filepath.Join(fixture.stateRoot, jailerChrootBaseDir), "firecracker", truncatedID), vmMetadataName)
	require.Equal(t, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID,
		Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 1,
	}, awaitVMMetadataForTest(t, path))

	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), longSandboxUID))
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr))
}

func TestEnsureSandboxWritesVMMetadataInStateDir(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareBareCachedImage(t, fixture.sandboxSpec.Desired.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	directory, err := sandboxDir(fixture.stateRoot, fixture.sandboxSpec.Identity.SandboxUID)
	require.NoError(t, err)
	path := filepath.Join(directory, vmMetadataName)
	require.Equal(t, VMMetadata{
		Version: vmMetadataVersion, UID: "sandbox-1", Namespace: "tenant-a",
		Name: "sandbox-1", PlacementAttempt: 1,
	}, awaitVMMetadataForTest(t, path))
}
