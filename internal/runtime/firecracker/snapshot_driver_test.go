package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	runtimecontract "fast-sandbox/internal/runtime/contract"

	"github.com/stretchr/testify/require"
)

// seedRunningSandbox writes a durable Running Sandbox state plus its
// instance rootfs, pointing the API socket at the fake VMM.
func seedRunningSandbox(t *testing.T, fixture *driverFixture, phase VMPhase) string {
	t.Helper()
	state := &SandboxState{
		Config:     fixture.sandboxSpec,
		Phase:      phase,
		APIAddress: fixture.server.socket,
		CreatedAt:  1720000000,
	}
	dir := filepath.Join(fixture.stateRoot, sandboxStateDir, state.Config.Identity.SandboxUID)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, instanceRootfsName), []byte("instance-rootfs-data"), 0o640))
	require.NoError(t, saveState(dir, state))
	return state.Config.Identity.SandboxUID
}

// snapshotAgentFake records the publication and inspects the staging set at
// call time (the driver removes staging right after).
type snapshotAgentFake struct {
	fakeAgentClient
	key         string
	manifest    map[string]any
	sums        string
	missingFile string
}

func (f *snapshotAgentFake) PublishImage(_ context.Context, _, key, dir string) (PublishOutcome, error) {
	payload, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return PublishOutcome{}, err
	}
	if err := json.Unmarshal(payload, &f.manifest); err != nil {
		return PublishOutcome{}, err
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return PublishOutcome{}, err
	}
	f.sums, f.key = string(sums), key
	for _, name := range []string{"rootfs.ext4", "vmstate.snap", "memory.snap"} {
		if info, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil || info.Size() == 0 {
			f.missingFile = name
			return PublishOutcome{}, errors.New("incomplete staging set: " + name)
		}
	}
	return PublishOutcome{ManifestRef: "s3://bucket/publish/0123456789abcdef/manifest.json", ArtifactDigest: "digest-" + key}, nil
}

// withSnapshotSpill points the driver at a per-test spill root.
func withSnapshotSpill(t *testing.T) string {
	t.Helper()
	spill := t.TempDir()
	t.Setenv("FAST_SANDBOX_SNAPSHOT_SPILL_DIR", spill)
	return spill
}

func TestCreateSnapshotSpillsDumpOutsideStateRoot(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	spill := withSnapshotSpill(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, "digest-app-v2", result.ArtifactDigest)

	// The dump targeted the spill area (absolute paths in direct mode).
	require.Len(t, fixture.server.snapshotDumps, 1)
	require.Equal(t, filepath.Join(spill, "snap-1", jailedDumpVMStateName), fixture.server.snapshotDumps[0].SnapshotPath)
	require.Equal(t, filepath.Join(spill, "snap-1", jailedDumpMemoryName), fixture.server.snapshotDumps[0].MemFilePath)

	// The relocation landed the artifacts in the publish staging: the
	// agent-side manifest assembly saw all three files.
	require.Len(t, agent.manifest["files"].(map[string]any), 3)

	// The spill area is empty again and no dump leaked into the staging
	// placement the driver itself controls (staging is fully removed after
	// publication by CreateSnapshot).
	entries, readErr := os.ReadDir(spill)
	require.NoError(t, readErr)
	require.Empty(t, entries, "the per-snapshot spill directory is removed")
}

func TestCreateSnapshotRecordsActionBindingsInManifest(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
		ActionBindings: []runtimecontract.SnapshotActionBinding{
			{Handler: "egress", Input: `{"egressPolicy":"deny-all"}`},
		},
	})
	require.NoError(t, err)
	recorded, ok := agent.manifest["actionBindings"].([]any)
	require.True(t, ok, "manifest lacks actionBindings")
	require.Len(t, recorded, 1)
	entry := recorded[0].(map[string]any)
	require.Equal(t, "egress", entry["handler"])
	require.Equal(t, `{"egressPolicy":"deny-all"}`, entry["input"])
}

func TestCreateSnapshotFallsBackWhenSpillUnavailable(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	// A spill root that does not exist: the capacity guard fails and the
	// dump falls back to the staging directory (legacy absolute paths).
	t.Setenv("FAST_SANDBOX_SNAPSHOT_SPILL_DIR", filepath.Join(t.TempDir(), "missing"))
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Len(t, fixture.server.snapshotDumps, 1)
	require.Equal(t, filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1", vmstateSnapshotName),
		fixture.server.snapshotDumps[0].SnapshotPath)
	require.Len(t, agent.manifest["files"].(map[string]any), 3)
}

func newSnapshotFixture(t *testing.T) (*driverFixture, *snapshotAgentFake) {
	t.Helper()
	fixture := newSnapshotDriverFixture(t)
	agent := &snapshotAgentFake{}
	fixture.driver.agentSocket = "/run/test/agent.sock"
	fixture.driver.newAgentClient = func(string) (AgentClient, error) { return agent, nil }
	return fixture, agent
}

// newSnapshotDriverFixture prepares a driver with a cached image (including
// its manifest facts) and a running fake VMM, without wiring an agent.
func newSnapshotDriverFixture(t *testing.T) *driverFixture {
	t.Helper()
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	fixture.server.running = true
	return fixture
}

func TestCreateSnapshotDumpsPublishesAndResumes(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, "snap-1", result.SnapshotID)
	require.Equal(t, "digest-app-v2", result.ArtifactDigest)
	require.Contains(t, result.ManifestRef, "s3://bucket/publish/")
	require.Greater(t, result.SizeBytes, int64(0))

	// The dump paused and resumed the VM and wrote both snapshot files.
	require.Len(t, fixture.server.snapshotDumps, 1)
	require.Equal(t, "Full", fixture.server.snapshotDumps[0].SnapshotType)
	require.True(t, fixture.server.running, "the VM must be resumed")
	require.Contains(t, fixture.server.recordedCalls(), "PATCH /vm")

	// The published manifest copies the source facts verbatim and describes
	// this dump.
	require.Equal(t, "app-v2", agent.key)
	require.Equal(t, fixture.sandboxSpec.Spec.Image, agent.manifest["sourceImage"])
	require.Equal(t, map[string]any{
		"vcpu": "2", "memory": "1Gi",
	}, agent.manifest["machine"])
	require.Equal(t, map[string]any{
		"iface": "eth0", "mac": "02:00:00:00:00:01", "ip": "172.30.0.3",
		"gateway": "172.30.0.1", "netmask": "255.255.255.0",
	}, agent.manifest["guestNetwork"])
	require.Equal(t, "native", agent.manifest["format"])
	require.Equal(t, map[string]any{"booted": true, "restored": false}, agent.manifest["validation"])
	files := agent.manifest["files"].(map[string]any)
	require.Len(t, files, 3)
	require.Contains(t, agent.sums, "rootfs.ext4")
	require.Contains(t, agent.sums, "memory.snap")

	// Staging is removed after the publication.
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1"))
	require.True(t, os.IsNotExist(statErr))
}

func TestCreateSnapshotResumesAfterFailedDump(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	fixture.server.mu.Lock()
	fixture.server.failSnapshot = true
	fixture.server.mu.Unlock()

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.Error(t, err)
	require.True(t, fixture.server.running, "the VM must resume even after a failed dump")
	require.Empty(t, agent.fakeAgentClient.publishes, "nothing is published on failure")
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1"))
	require.True(t, os.IsNotExist(statErr), "failed staging is discarded")
}

func TestCreateSnapshotRequiresRunningSandbox(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseStopped)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Empty(t, agent.fakeAgentClient.publishes)
}

func TestCreateSnapshotRequiresAgentForPublication(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	fixture.server.running = true
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.True(t, fixture.server.running, "the VM is resumed even when publication is impossible")
}

func TestCreateSnapshotRequiresSourceManifestFacts(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	fixture.server.running = true
	// A hand-seeded cache without machine/guestNetwork facts.
	dir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Spec.Image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schemaVersion":1}`), 0o640))
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "machine")
}

func TestCreateSnapshotRejectsInsufficientStagingBeforePause(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	fixture.driver.snapshotCapacityWait = 0 // no GC self-heal wait
	// An absurd memory size makes the footprint exceed any tmpdir's free
	// space without touching the disk.
	fixture.sandboxSpec.Spec.Memory = "99999Gi"
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, runtimecontract.ErrInsufficientStorage)
	require.Contains(t, err.Error(), "staging needs")
	// The VM was never paused and nothing was published.
	require.True(t, fixture.server.running)
	require.Empty(t, fixture.server.snapshotDumps)
	require.Empty(t, agent.fakeAgentClient.publishes)
}

func TestDeleteSnapshotDiscardsStaging(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	staging := filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1")
	require.NoError(t, os.MkdirAll(staging, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "manifest.json"), []byte("{}"), 0o644))

	require.NoError(t, fixture.driver.DeleteSnapshot(context.Background(), "snap-1"))
	_, statErr := os.Stat(staging)
	require.True(t, os.IsNotExist(statErr))
	// Idempotent for unknown snapshots.
	require.NoError(t, fixture.driver.DeleteSnapshot(context.Background(), "snap-ghost"))
}
