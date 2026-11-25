package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
)

// assembleSnapshotManifest is the manifest assembly without earlier proofs,
// as the assembly tests drive it directly.
func assembleSnapshotManifest(stateRoot, ociRoot, staging, sandboxDir string, kind fastletapi.SnapshotKind) (int64, error) {
	size, _, err := assembleSnapshotManifestWith(stateRoot, ociRoot, staging, sandboxDir, kind, nil)
	return size, err
}

// countStagedHashes wraps the staged-file hash and returns the base names
// it read, one entry per read.
func countStagedHashes(t *testing.T) func() []string {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []string
	)
	original := hashStagedFile
	hashStagedFile = func(path string) (string, int64, agentpull.VerifiedFile, error) {
		mu.Lock()
		calls = append(calls, filepath.Base(path))
		mu.Unlock()
		return original(path)
	}
	t.Cleanup(func() { hashStagedFile = original })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func proveFile(t *testing.T, path string) agentpull.VerifiedFile {
	t.Helper()
	_, _, proof, err := agentpull.HashAndTrackFile(path)
	require.NoError(t, err)
	return proof
}

func TestHashStagedFilesUsesKnownDigests(t *testing.T) {
	dir := t.TempDir()
	for name, payload := range map[string]string{"a": "alpha", "b": "beta"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(payload), 0o640))
	}
	known := map[string]agentpull.VerifiedFile{"a": proveFile(t, filepath.Join(dir, "a"))}
	hashed := countStagedHashes(t)

	cache, verified, err := hashStagedFiles(dir, []string{"a", "b"}, known)
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, hashed(), "a file with a valid proof is not read again")
	for _, name := range []string{"a", "b"} {
		want, err := artifacts.SHA256File(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, want, cache[filepath.Join(dir, name)])
		require.Equal(t, want, verified[name].SHA256(), "every hashed file carries its proof")
	}

	_, _, err = hashStagedFiles(dir, []string{"missing"}, nil)
	require.Error(t, err)
}

// A proof of a file that changed since it was hashed is not trusted.
func TestHashStagedFilesRehashesStaleProof(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	require.NoError(t, os.WriteFile(path, []byte("alpha"), 0o640))
	known := map[string]agentpull.VerifiedFile{"a": proveFile(t, path)}
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("omega"), 0o640))
	hashed := countStagedHashes(t)

	cache, _, err := hashStagedFiles(dir, []string{"a"}, known)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, hashed())
	want, err := artifacts.SHA256File(path)
	require.NoError(t, err)
	require.Equal(t, want, cache[path])
}

// An unknown name before known ones starts a hashing goroutine first; the
// known digests must not be written to the maps concurrently with it (run
// with -race).
func TestHashStagedFilesUnknownBeforeKnown(t *testing.T) {
	dir := t.TempDir()
	var names []string
	known := map[string]agentpull.VerifiedFile{}
	for i := range 16 {
		name := "f" + string(rune('a'+i))
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, 4096), 0o640))
		names = append(names, name)
		if i%2 == 1 {
			known[name] = proveFile(t, path)
		}
	}
	for range 20 {
		cache, verified, err := hashStagedFiles(dir, names, known)
		require.NoError(t, err)
		require.Len(t, cache, len(names))
		require.Len(t, verified, len(names))
		for _, name := range names {
			want, err := artifacts.SHA256File(filepath.Join(dir, name))
			require.NoError(t, err)
			require.Equal(t, want, cache[filepath.Join(dir, name)])
		}
	}
}

// A Full snapshot reads each staged file once for its manifest, and the
// proofs reach the same-node cache commit.
func TestCreateSnapshotHashesEachStagedFileOnce(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	// No reflink clone: the assembly hashes the rootfs too.
	original := cloneRootfs
	cloneRootfs = func(string, string) error { return os.ErrInvalid }
	t.Cleanup(func() { cloneRootfs = original })
	hashed := countStagedHashes(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	calls := hashed()
	require.ElementsMatch(t, []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName}, calls)
	files, ok := agent.manifest["files"].(map[string]any)
	require.True(t, ok)
	for _, name := range calls {
		entry, ok := files[name].(map[string]any)
		require.True(t, ok, name)
		cached := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey("app-v2"), name)
		if name == publishedRootfsName {
			cached = filepath.Join(fixture.stateRoot, imageCacheDir, imageKey("app-v2"), rootfsImageName)
		}
		want, err := artifacts.SHA256File(cached)
		require.NoError(t, err)
		require.Equal(t, want, entry["sha256"], "%s digest", name)
	}
}

// withClonedRootfs stands in for a reflink-capable filesystem: the snapshot
// rootfs "clone" succeeds (as a plain copy), enabling the background hash.
func withClonedRootfs(t *testing.T) {
	t.Helper()
	original := cloneRootfs
	cloneRootfs = copyFile
	t.Cleanup(func() { cloneRootfs = original })
}

// observeRootfsHash wraps the background rootfs hash, failing it when fail
// is set, and returns the paths it was called with.
func observeRootfsHash(t *testing.T, fail bool) func() []string {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []string
	)
	original := hashStagedRootfs
	hashStagedRootfs = func(path string) (string, int64, agentpull.VerifiedFile, error) {
		mu.Lock()
		calls = append(calls, path)
		mu.Unlock()
		if fail {
			return "", 0, agentpull.VerifiedFile{}, os.ErrPermission
		}
		return original(path)
	}
	t.Cleanup(func() { hashStagedRootfs = original })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func manifestRootfsDigest(t *testing.T, agent *snapshotAgentFake) string {
	t.Helper()
	files, ok := agent.manifest["files"].(map[string]any)
	require.True(t, ok, "manifest files section")
	entry, ok := files[publishedRootfsName].(map[string]any)
	require.True(t, ok, "manifest rootfs entry")
	return entry["sha256"].(string)
}

func createTemplateSnapshot(t *testing.T, fixture *driverFixture) string {
	t.Helper()
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	want, err := artifacts.SHA256File(filepath.Join(fixture.stateRoot, sandboxStateDir, sandboxID, instanceRootfsName))
	require.NoError(t, err)
	_, err = fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	return want
}

func TestCreateSnapshotHashesClonedRootfsInBackground(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	withClonedRootfs(t)
	background := observeRootfsHash(t, false)
	assembly := countStagedHashes(t)

	want := createTemplateSnapshot(t, fixture)
	require.Equal(t, []string{filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1", publishedRootfsName)}, background(),
		"the cloned rootfs is hashed by the background hash")
	require.NotContains(t, assembly(), publishedRootfsName, "the assembly reuses the background digest")
	require.Equal(t, want, manifestRootfsDigest(t, agent))
}

func TestCreateSnapshotFallsBackWhenBackgroundRootfsHashFails(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	withClonedRootfs(t)
	background := observeRootfsHash(t, true)
	assembly := countStagedHashes(t)

	want := createTemplateSnapshot(t, fixture)
	require.Len(t, background(), 1)
	require.Contains(t, assembly(), publishedRootfsName, "the manifest assembly hashes the rootfs itself")
	require.Equal(t, want, manifestRootfsDigest(t, agent))
}

// Without a reflink clone the rootfs is copied in the pause window and the
// manifest assembly hashes it, as before.
func TestCreateSnapshotWithoutCloneSkipsBackgroundRootfsHash(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	original := cloneRootfs
	cloneRootfs = func(string, string) error { return os.ErrInvalid }
	t.Cleanup(func() { cloneRootfs = original })
	background := observeRootfsHash(t, false)

	want := createTemplateSnapshot(t, fixture)
	require.Empty(t, background(), "a copied rootfs is not hashed in the background")
	require.Equal(t, want, manifestRootfsDigest(t, agent))
}
