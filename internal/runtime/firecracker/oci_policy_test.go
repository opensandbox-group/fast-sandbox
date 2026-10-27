package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestOCISnapshotManifestCarriesOpaquePolicy(t *testing.T) {
	for _, kind := range []fastletapi.SnapshotKind{fastletapi.SnapshotKindCheckpoint, fastletapi.SnapshotKindTemplate} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := newSnapshotDriverFixture(t)
			id := seedRunningSandbox(t, fixture, PhaseRunning)
			sandboxDir := filepath.Join(fixture.stateRoot, sandboxStateDir, id)
			state, err := loadState(sandboxDir)
			require.NoError(t, err)
			state.Config.Desired.Image = "oci://r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			require.NoError(t, saveState(sandboxDir, state))
			home := filepath.Join(fixture.stateRoot, "oci/sandboxes", id)
			require.NoError(t, os.MkdirAll(filepath.Join(home, "mnt"), 0750))
			metadata := preparedMetadata(t)
			metadata["platform"] = map[string]any{"checkpoint": map[string]string{"sandboxUID": "previous", "namespace": "old"}, "future": map[string]bool{"keep": true}, "stateFiles": map[string]any{"policy.json": map[string]any{"sha256": artifacts.SHA256Of([]byte("stale")), "sizeBytes": 1}}}
			raw, err := json.Marshal(metadata)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(home, "mnt", snapshotManifestName), raw, 0600))
			policy := []byte(`{"v":1,"incarnation":"source-fence","attachmentId":"source-route","revision":7,"policy":{"defaultAction":"deny"},"opaque":"preserve-me"}`)
			vault := []byte(`{"encrypted":"opaque-vault-record"}`)
			mover := &fakeTemplateCapableMover{fakeStateMover: &fakeStateMover{exportData: map[runtimecontract.StateKind][]byte{runtimecontract.StateKindPolicy: policy, runtimecontract.StateKindVault: vault}}}
			fixture.driver.SetStateMover(mover)
			staging := t.TempDir()
			for _, name := range []string{vmstateSnapshotName, memorySnapshotName} {
				require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte("dump-"+name), 0600))
			}
			upper := ocitemplate.RootfsUpper{Repository: "r.example/templates/base", VirtualSizeBytes: 1 << 30, Lowers: []oci.Descriptor{{Digest: digest.FromString("parent"), Size: 123, MediaType: ocitemplate.OverlayBDLayerMediaType}}}
			raw, err = json.Marshal(upper)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(staging, ocitemplate.RootfsUpperName), raw, 0600))
			if kind == fastletapi.SnapshotKindCheckpoint {
				require.NoError(t, fixture.driver.stageCheckpointVault(context.Background(), id, staging))
				require.NoError(t, fixture.driver.stageCheckpointPolicy(context.Background(), id, staging))
			} else {
				require.NoError(t, fixture.driver.stageTemplateState(context.Background(), id, staging))
				exports, _ := mover.snapshotTemplate()
				require.Len(t, exports, 2, "template snapshots use seal-free state export")
			}
			_, err = assembleSnapshotManifest(fixture.stateRoot, fixture.driver.ociRoot(), staging, sandboxDir, kind)
			require.NoError(t, err)
			raw, err = os.ReadFile(filepath.Join(staging, snapshotManifestName))
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(raw, &document))
			platform := document["platform"].(map[string]any)
			require.Equal(t, map[string]any{"keep": true}, platform["future"])
			if kind == fastletapi.SnapshotKindCheckpoint {
				require.Equal(t, map[string]any{"sandboxUID": state.Config.Identity.SandboxUID, "namespace": state.Config.Identity.Namespace}, platform["checkpoint"])
			} else {
				require.NotContains(t, platform, "checkpoint")
			}
			files := document["platform"].(map[string]any)["stateFiles"].(map[string]any)
			require.NotContains(t, files, publishedRootfsName, "OCI rootfs stays in the other image")
			require.Equal(t, map[string]any{"sha256": artifacts.SHA256Of(policy), "sizeBytes": float64(len(policy))}, files[policyBundleName])
			require.Contains(t, files, vaultBundleName)
			require.NotContains(t, document, "egressVault")
			require.NotContains(t, string(raw), "preserve-me", "the manifest only checksums the opaque policy record")
			require.NotContains(t, string(raw), "source-fence")
			require.NotContains(t, document, "actionBindings", "binding seeds come from the create request")
			saved, err := os.ReadFile(filepath.Join(staging, policyBundleName))
			require.NoError(t, err)
			require.Equal(t, policy, saved)
			require.NoFileExists(t, filepath.Join(staging, "SHA256SUMS"))
		})
	}
}

func TestOCIRestoreImportsStateFromSnapfilesBeforeReady(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, integrity := range []string{"valid", "corrupt", "unlisted"} {
			t.Run(map[bool]string{false: "clone", true: "resume"}[resume]+"/"+integrity, func(t *testing.T) {
				fixture := newDriverFixture(t)
				require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
				image := "oci://r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				fixture.sandboxSpec.Desired.Image = image
				metadata := preparedMetadata(t)
				metadata["platform"] = map[string]any{}
				if resume {
					fixture.sandboxSpec.Desired.Restore = &fastletapi.RestoreSpec{ManifestRef: image, ArtifactDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
					metadata["platform"].(map[string]any)["checkpoint"] = map[string]string{"sandboxUID": "sandbox-1", "namespace": "tenant-a"}
				}
				policy := []byte(`{"v":1,"incarnation":"old","revision":7,"policy":{"defaultAction":"deny"}}`)
				vault := []byte(`{"encrypted":"record"}`)
				marker := map[string]any{"sha256": artifacts.SHA256Of(vault), "sizeBytes": len(vault)}
				files := map[string]any{vaultBundleName: marker}
				if integrity != "unlisted" {
					files[policyBundleName] = map[string]any{"sha256": artifacts.SHA256Of(policy), "sizeBytes": len(policy)}
				}
				metadata["platform"].(map[string]any)["stateFiles"] = files
				raw, err := json.Marshal(metadata)
				require.NoError(t, err)
				installPreparedOCI(t, fixture, string(raw))
				bundle := t.TempDir()
				// Actual snapfiles metadata has no OCI reference envelope; the prepared
				// CLI state above carries that envelope separately.
				delete(metadata, "oci")
				raw, err = json.Marshal(metadata)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(bundle, snapshotManifestName), raw, 0600))
				if integrity == "corrupt" {
					policy = []byte(`{"tampered":true}`)
				}
				require.NoError(t, os.WriteFile(filepath.Join(bundle, policyBundleName), policy, 0600))
				require.NoError(t, os.WriteFile(filepath.Join(bundle, vaultBundleName), vault, 0600))
				t.Setenv("TEST_PREPARE_BUNDLE", bundle)
				mover := &ociStateReader{fakeTemplateCapableMover: &fakeTemplateCapableMover{fakeStateMover: &fakeStateMover{}}, read: func(kind runtimecontract.StateKind, path string) {
					require.True(t, strings.HasPrefix(path, filepath.Join(fixture.stateRoot, sandboxStateDir, "sandbox-1")+string(os.PathSeparator)+"state-import-"), "handler must receive an ordinary shared file outside the private OCI mount")
					saved, err := os.ReadFile(path)
					require.NoError(t, err)
					require.Equal(t, map[runtimecontract.StateKind][]byte{runtimecontract.StateKindVault: vault, runtimecontract.StateKindPolicy: policy}[kind], saved)
					info, err := os.Stat(path)
					require.NoError(t, err)
					require.Equal(t, os.FileMode(0600), info.Mode().Perm())
				}}
				fixture.driver.SetStateMover(mover)
				_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
				require.NoError(t, err, "rejected policy follows native observable fallback without failing restore")
				var imports, other []string
				if resume {
					_, imports = mover.snapshot()
					_, other = mover.snapshotTemplate()
				} else {
					_, imports = mover.snapshotTemplate()
					_, other = mover.snapshot()
				}
				require.Empty(t, other, "clone landing and checkpoint import must remain distinct")
				expected := []string{"vault.json"}
				if integrity == "valid" {
					expected = append(expected, "policy.json")
				}
				names := make([]string, 0, len(imports))
				for _, entry := range imports {
					path := strings.SplitN(entry, ":", 3)[2]
					names = append(names, filepath.Base(path))
					require.NoDirExists(t, filepath.Dir(path), "temporary handler state must be removed after import")
				}
				require.ElementsMatch(t, expected, names, "verified state must be handed to the mover before EnsureSandbox returns")
				require.NoFileExists(t, cachedManifestPath(fixture.stateRoot, image), "OCI restore must not need a second native cache")
			})
		}
	}
}

// Read the state at the actual handler handoff, while its temporary file exists.
type ociStateReader struct {
	*fakeTemplateCapableMover
	read func(runtimecontract.StateKind, string)
}

func (m *ociStateReader) ImportState(ctx context.Context, uid string, kind runtimecontract.StateKind, path string) error {
	m.read(kind, path)
	return m.fakeTemplateCapableMover.ImportState(ctx, uid, kind, path)
}
func (m *ociStateReader) ImportTemplateState(ctx context.Context, uid string, kind runtimecontract.StateKind, path string) error {
	m.read(kind, path)
	return m.fakeTemplateCapableMover.ImportTemplateState(ctx, uid, kind, path)
}

func TestOCIStateHandoffRemovesFilesAfterHandlerFailure(t *testing.T) {
	shared := t.TempDir()
	source := filepath.Join(t.TempDir(), policyBundleName)
	record := []byte(`{"opaque":"record"}`)
	require.NoError(t, os.WriteFile(source, record, 0600))
	called := false
	importRestoreBundle(context.Background(), importPlan{
		sharedDir: shared, path: source, markerPresent: true,
		marker: &fileMarker{SHA256: artifacts.SHA256Of(record), SizeBytes: int64(len(record))}, maxBytes: maxPolicyBundleBytes,
		importIt: func(path string) error {
			called = true
			require.NotEqual(t, source, path)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, record, data)
			return errors.New("handler unavailable")
		},
	})
	require.True(t, called)
	files, err := os.ReadDir(shared)
	require.NoError(t, err)
	require.Empty(t, files, "failed imports must not leave policy or vault state on disk")
}
