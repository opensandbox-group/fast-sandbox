package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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

// waitForMetadataPublication blocks until every payload write started by the
// driver has settled, so tests observe the record after provisioning returns.
func (d *Driver) waitForMetadataPublication() {
	d.metadataPublication.Wait()
}

// writeVMMetadataRecordForTest publishes a record through the production
// primitives (compose, create, write) in one synchronous step.
func writeVMMetadataRecordForTest(t *testing.T, directory string, metadata VMMetadata) {
	t.Helper()
	payload, err := composeVMMetadata(metadata)
	require.NoError(t, err)
	file, err := createVMMetadataRecord(directory)
	require.NoError(t, err)
	require.NoError(t, writeVMMetadataPayload(file, payload))
}

// readVMMetadataForTest reads and decodes the record after provisioning.
func readVMMetadataForTest(t *testing.T, path string) VMMetadata {
	t.Helper()
	payload, err := os.ReadFile(path)
	require.NoError(t, err, "the discovery record must exist after provisioning returns")
	var decoded VMMetadata
	require.NoError(t, json.Unmarshal(payload, &decoded))
	return decoded
}

// holdMetadataPayload blocks the payload write inside the driver's publisher:
// entered closes once a write is in flight (and blocked), release unblocks
// it. The gate passes through exactly once, so later publications — a
// replacement VM's own publisher — are never delayed.
func holdMetadataPayload(entered, release chan struct{}) func(*os.File, []byte) error {
	var gate sync.Mutex
	held := false
	return func(file *os.File, payload []byte) error {
		gate.Lock()
		first := !held
		held = true
		gate.Unlock()
		if first {
			close(entered)
			<-release
		}
		return writeVMMetadataPayload(file, payload)
	}
}

func TestWriteVMMetadataRecordSchema(t *testing.T) {
	directory := t.TempDir()
	writeVMMetadataRecordForTest(t, directory, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID,
		Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 2,
	})
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

func TestWriteVMMetadataRecordOmitsZeroPlacementAttempt(t *testing.T) {
	directory := t.TempDir()
	writeVMMetadataRecordForTest(t, directory, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID, Namespace: "team-a", Name: "sbx-7d9f2c",
	})
	payload, err := os.ReadFile(filepath.Join(directory, vmMetadataName))
	require.NoError(t, err)
	require.NotContains(t, string(payload), "placementAttempt")
}

func TestWriteVMMetadataRecordIsWriteOnce(t *testing.T) {
	directory := t.TempDir()
	first := VMMetadata{Version: vmMetadataVersion, UID: longSandboxUID, Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 1}
	writeVMMetadataRecordForTest(t, directory, first)
	// A stale publisher's create hits EEXIST and is treated as already
	// published: the record is never rewritten.
	_, err := createVMMetadataRecord(directory)
	require.True(t, os.IsExist(err))
	payload, err := os.ReadFile(filepath.Join(directory, vmMetadataName))
	require.NoError(t, err)
	var decoded VMMetadata
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, first, decoded)
}

func TestVMMetadataRejectsInvalid(t *testing.T) {
	_, err := composeVMMetadata(VMMetadata{})
	require.ErrorIs(t, err, ErrInvalidConfig)
	_, err = createVMMetadataRecord("")
	require.ErrorIs(t, err, ErrInvalidConfig)
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
	fixture.driver.waitForMetadataPublication()

	truncatedID := truncatedSandboxID(longSandboxUID)
	require.Len(t, truncatedID, 32)
	path := filepath.Join(jailerRoot(filepath.Join(fixture.stateRoot, jailerChrootBaseDir), "firecracker", truncatedID), vmMetadataName)
	require.Equal(t, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID,
		Namespace: "team-a", Name: "sbx-7d9f2c", PlacementAttempt: 1,
	}, readVMMetadataForTest(t, path))

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
	fixture.driver.waitForMetadataPublication()

	directory, err := sandboxDir(fixture.stateRoot, fixture.sandboxSpec.Identity.SandboxUID)
	require.NoError(t, err)
	path := filepath.Join(directory, vmMetadataName)
	require.Equal(t, VMMetadata{
		Version: vmMetadataVersion, UID: "sandbox-1", Namespace: "tenant-a",
		Name: "sandbox-1", PlacementAttempt: 1,
	}, readVMMetadataForTest(t, path))
}

func TestEnsureSandboxDoesNotWaitForMetadataPayload(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareBareCachedImage(t, fixture.sandboxSpec.Desired.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	entered := make(chan struct{})
	release := make(chan struct{})
	fixture.driver.writeVMMetadataPayload = holdMetadataPayload(entered, release)

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	// The publisher is provably blocked mid-write while EnsureSandbox has
	// already returned: provisioning never waits for the payload.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the payload write never started")
	}

	directory, err := sandboxDir(fixture.stateRoot, fixture.sandboxSpec.Identity.SandboxUID)
	require.NoError(t, err)
	payload, err := os.ReadFile(filepath.Join(directory, vmMetadataName))
	require.NoError(t, err)
	require.Empty(t, payload, "the record is created before the payload lands")

	close(release)
	fixture.driver.waitForMetadataPublication()
	require.Equal(t, VMMetadata{
		Version: vmMetadataVersion, UID: "sandbox-1", Namespace: "tenant-a",
		Name: "sandbox-1", PlacementAttempt: 1,
	}, readVMMetadataForTest(t, filepath.Join(directory, vmMetadataName)))
}

func TestStaleMetadataPublisherCannotWriteReplacementRecord(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.enableJailer()
	fixture.sandboxSpec.Identity.SandboxUID = longSandboxUID
	fixture.sandboxSpec.Identity.Namespace = "team-a"
	fixture.sandboxSpec.Identity.Name = "sbx-7d9f2c"
	fixture.prepareBareCachedImage(t, fixture.sandboxSpec.Desired.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	entered := make(chan struct{})
	release := make(chan struct{})
	fixture.driver.writeVMMetadataPayload = holdMetadataPayload(entered, release)

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	<-entered // the first publisher is blocked mid-write

	// The replacement reuses the same truncated jail directory name: the
	// stale publisher's handle must keep pointing at the removed jail, never
	// at the recreated one.
	replacement := fixture.sandboxSpec
	replacement.Identity.RuntimeInstanceID = "runtime-2"
	replacement.Identity.AssignmentAttempt = 2
	_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&replacement))
	require.NoError(t, err)

	// Only now unblock the stale publisher and let every write settle.
	close(release)
	fixture.driver.waitForMetadataPublication()

	truncatedID := truncatedSandboxID(longSandboxUID)
	path := filepath.Join(jailerRoot(filepath.Join(fixture.stateRoot, jailerChrootBaseDir), "firecracker", truncatedID), vmMetadataName)
	require.Equal(t, VMMetadata{
		Version: vmMetadataVersion, UID: longSandboxUID, Namespace: "team-a",
		Name: "sbx-7d9f2c", PlacementAttempt: 2,
	}, readVMMetadataForTest(t, path), "the stale publisher must not write the replacement's record")
}
