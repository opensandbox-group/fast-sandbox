package janitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
	boxlitestate "fast-sandbox/internal/runtime/boxlite/state"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestBoxLiteBackendScansFencedRecordsAndCleansAfterLockRelease(t *testing.T) {
	root := t.TempDir()
	home, recordPath := writeBoxLiteState(t, root, "pod-uid-a", "sandbox-uid-a")
	backend := NewBoxLiteBackend(root)

	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 1)
	resource := resources[0]
	require.Equal(t, BackendBoxLite, resource.Backend)
	require.Equal(t, "pod-uid-a", resource.FastletPodUID)
	require.Equal(t, "sandbox-uid-a", resource.SandboxUID)
	require.Equal(t, "sandbox-a", resource.SandboxName)
	require.Equal(t, "default", resource.SandboxNamespace)
	require.Equal(t, int64(2), resource.InstanceGeneration)
	require.Equal(t, int64(3), resource.AssignmentAttempt)
	require.Equal(t, filepath.Join(boxlitestate.SafeSegment("pod-uid-a"), filepath.Base(recordPath)), resource.ResourceID)

	lock, err := os.OpenFile(filepath.Join(home, boxlitestate.RuntimeLockFileName), os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	require.ErrorContains(t, backend.Cleanup(context.Background(), resource), "still owns state lock")
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_UN))
	require.NoError(t, lock.Close())

	require.NoError(t, backend.Cleanup(context.Background(), resource))
	_, err = os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, backend.Cleanup(context.Background(), resource), "cleanup must be idempotent")
}

func TestBoxLiteBackendFailsClosedOnRecordFenceChange(t *testing.T) {
	root := t.TempDir()
	_, recordPath := writeBoxLiteState(t, root, "pod-uid-a", "sandbox-uid-a")
	backend := NewBoxLiteBackend(root)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, resources, 1)

	record := readTestBoxLiteRecord(t, recordPath)
	record.Request.Sandbox.AssignmentAttempt++
	writeTestJSON(t, recordPath, record)
	require.ErrorContains(t, backend.Cleanup(context.Background(), resources[0]), "identity changed")
}

func TestBoxLiteBackendScansEmptyHomeAsPodOwnedResource(t *testing.T) {
	root := t.TempDir()
	podUID := "pod-uid-empty"
	createdAt := time.Now().Add(-time.Hour).Unix()
	home := boxlitestate.HomeDirectory(root, podUID)
	require.NoError(t, os.MkdirAll(home, 0700))
	writeTestJSON(t, filepath.Join(home, boxlitestate.OwnerFileName), boxlitestate.OwnerRecord{
		Version: boxlitestate.Version, FastletPodUID: podUID, CreatedAt: createdAt,
	})
	backend := NewBoxLiteBackend(root)
	resources, err := backend.Scan(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ResourceIdentity{{
		Backend: BackendBoxLite, ResourceID: boxlitestate.SafeSegment(podUID), FastletPodUID: podUID,
		CreatedAt: time.Unix(createdAt, 0),
	}}, resources)
	require.NoError(t, backend.Cleanup(context.Background(), resources[0]))
}

func writeBoxLiteState(t *testing.T, root, podUID, sandboxUID string) (string, string) {
	t.Helper()
	createdAt := time.Now().Add(-time.Hour).Unix()
	home := boxlitestate.HomeDirectory(root, podUID)
	metadataRoot := filepath.Join(home, boxlitestate.MetadataDirectoryName)
	bundleRoot := filepath.Join(home, boxlitestate.BundleDirectoryName, boxlitestate.SafeSegment(sandboxUID), "spec-hash")
	require.NoError(t, os.MkdirAll(metadataRoot, 0700))
	require.NoError(t, os.MkdirAll(bundleRoot, 0700))
	writeTestJSON(t, filepath.Join(home, boxlitestate.OwnerFileName), boxlitestate.OwnerRecord{
		Version: boxlitestate.Version, FastletPodUID: podUID, CreatedAt: createdAt,
	})
	record := boxlitestate.SandboxRecord{
		Version: boxlitestate.Version, Namespace: "default", SpecHash: "spec-hash", HostPort: 21000,
		CreatedAt: createdAt, BundleRoot: bundleRoot,
		Request: boxliteprotocol.EnsureRequest{Namespace: "default", TunnelGuestPort: 19090, Sandbox: fastletapi.SandboxSpec{
			SandboxID: sandboxUID, ClaimUID: sandboxUID, ClaimName: "sandbox-a", ClaimNamespace: "default",
			FastletPodUID: podUID, InstanceGeneration: 2, AssignmentAttempt: 3, RouteGeneration: 4,
		}},
	}
	recordPath := filepath.Join(metadataRoot, boxlitestate.RecordFileName(sandboxUID))
	writeTestJSON(t, recordPath, record)
	return home, recordPath
}

func readTestBoxLiteRecord(t *testing.T, path string) boxlitestate.SandboxRecord {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	var record boxlitestate.SandboxRecord
	require.NoError(t, json.NewDecoder(file).Decode(&record))
	return record
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(file).Encode(value))
	require.NoError(t, file.Close())
}
