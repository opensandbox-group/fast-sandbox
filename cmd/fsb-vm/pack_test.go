package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/ocitemplate"
)

func TestPackRejectsTruncatedCheckpointBeforeKernelResources(t *testing.T) {
	dir := t.TempDir()
	metadata := testVMManifest()
	raw, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600))
	for _, file := range []string{"rootfs.ext4", "memory.snap", "vmstate.snap"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("short"), 0600))
	}
	err = packCheckpoint(context.Background(), dir, "snapshot:test", defaultMaxRootfsLayers, defaultSquashLayers)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match machine metadata")
}

func TestPackedPairPreservesRootfsParentLayers(t *testing.T) {
	for _, baseLayers := range []int{0, 5} {
		t.Run(fmt.Sprintf("base=%d", baseLayers), func(t *testing.T) {
			layout, _, err := createStagedArtifactLayout(t.TempDir(), "incremental")
			require.NoError(t, err)
			parent := ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("parent"), Size: 100}
			delta := ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("delta"), Size: 20}
			snap := ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("memory"), Size: 50}
			names, err := parseOutput("snapshot:test")
			require.NoError(t, err)
			_, err = writeTemplatePair(layout, names, []ocispec.Descriptor{parent, delta}, snap, "checkpoint", nil, baseLayers)
			require.NoError(t, err)
			var index layoutIndex
			raw, err := os.ReadFile(filepath.Join(layout.root, "manifest/index.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &index))
			require.Len(t, index.Images, 2)
			raw, err = os.ReadFile(layout.manifestPath(index.Images[0].ManifestFile))
			require.NoError(t, err)
			var manifest ocispec.Manifest
			require.NoError(t, json.Unmarshal(raw, &manifest))
			require.Equal(t, []ocispec.Descriptor{parent, delta}, manifest.Layers)
			if baseLayers > 0 {
				require.Equal(t, strconv.Itoa(baseLayers), manifest.Annotations[ocitemplate.AnnotationRootfsBaseLayers])
			} else {
				require.NotContains(t, manifest.Annotations, ocitemplate.AnnotationRootfsBaseLayers)
			}
			raw, err = os.ReadFile(layout.manifestPath(index.Images[1].ManifestFile))
			require.NoError(t, err)
			var snapfilesManifest ocispec.Manifest
			require.NoError(t, json.Unmarshal(raw, &snapfilesManifest))
			require.NotContains(t, snapfilesManifest.Annotations, ocitemplate.AnnotationRootfsBaseLayers, "only the rootfs manifest carries the base boundary")
		})
	}
}

func TestRecoverPackReleasesOnlyOwnedDeviceBeforeRemovingFiles(t *testing.T) {
	for _, failure := range []string{"", "unmount", "list", "detach", "outside", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			bundle := t.TempDir()
			global.buildWorkDir = filepath.Join(bundle, "pack-work")
			require.NoError(t, os.MkdirAll(global.buildWorkDir, 0750))
			work, err := os.MkdirTemp(global.buildWorkDir, "snapshot_test-")
			require.NoError(t, err)
			config := filepath.Join(work, snapfilesConfigName)
			require.NoError(t, os.WriteFile(config, []byte("backing-files"), 0600))
			marker := filepath.Join(bundle, packActiveMarker)
			markerWork := work
			if failure == "outside" {
				markerWork = t.TempDir()
			}
			if failure == "symlink" {
				markerWork = filepath.Join(global.buildWorkDir, "snapshot_link")
				require.NoError(t, os.Symlink(t.TempDir(), markerWork))
			}
			require.NoError(t, writePackResources(marker, markerWork, packPhaseAttached))
			devices := &fakeReleaser{devices: []ublkdDevice{{DevID: 1, Config: config}, {DevID: 2, Config: filepath.Join(bundle, "another.config")}}, delErr: map[int]error{}}
			if failure == "list" {
				devices.listErr = errors.New("list failed")
			}
			if failure == "detach" {
				devices.delErr[1] = errors.New("detach failed")
			}
			unmounted := false
			err = recoverPack(context.Background(), bundle, devices, func(path string) error {
				require.Equal(t, filepath.Join(work, "mnt-snapfiles"), path)
				require.FileExists(t, config)
				require.Empty(t, devices.deleted)
				unmounted = true
				if failure == "unmount" {
					return errors.New("unmount failed")
				}
				return nil
			})
			if failure != "" {
				require.Error(t, err)
				require.FileExists(t, marker)
				require.FileExists(t, config)
				require.Empty(t, devices.deleted)
			} else {
				require.NoError(t, err)
				require.True(t, unmounted)
				require.Equal(t, []int{1}, devices.deleted)
				require.NoDirExists(t, work)
				require.NoFileExists(t, marker)
				require.NoError(t, recoverPack(context.Background(), bundle, devices, func(string) error { t.Fatal("already recovered"); return nil }))
			}
		})
	}
}

func TestPackRecoveryRefusesActivePack(t *testing.T) {
	previous := global
	t.Cleanup(func() { global = previous })
	global.stateDir = t.TempDir()
	bundle := t.TempDir()
	release, err := lockPackBundle(bundle)
	require.NoError(t, err)
	_, err = lockPackBundle(bundle)
	require.ErrorIs(t, err, errBusy)
	release()
	release, err = lockPackBundle(bundle)
	require.NoError(t, err)
	release()
}

func TestRecoverPackKeepsUncertainAddBackingFiles(t *testing.T) {
	for _, phase := range []string{"prepared", "adding", "attached", "released"} {
		t.Run(phase, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			bundle := t.TempDir()
			global.buildWorkDir = filepath.Join(bundle, "pack-work")
			work := filepath.Join(global.buildWorkDir, "snapshot_test-work")
			require.NoError(t, os.MkdirAll(work, 0750))
			marker := filepath.Join(bundle, packActiveMarker)
			require.NoError(t, writePackResources(marker, work, phase))
			err := recoverPack(context.Background(), bundle, &fakeReleaser{}, func(string) error { return nil })
			if phase == "adding" {
				require.ErrorContains(t, err, "may still be in flight")
				require.DirExists(t, work)
				require.FileExists(t, marker)
			} else {
				require.NoError(t, err)
				require.NoDirExists(t, work)
				require.NoFileExists(t, marker)
			}
		})
	}
}

func TestValidatePackBundlePreservesMetadataAndChecksMachine(t *testing.T) {
	for _, mode := range []string{"valid", "invalid-cpu", "invalid-drive", "short-memory", "short-rootfs"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			machine := testVMManifest()
			if mode == "invalid-cpu" {
				machine.Machine.VCPUCount = 0
			}
			if mode == "invalid-drive" {
				machine.Machine.RootfsDrivePath = "/unrelated"
			}
			raw, err := json.Marshal(machine)
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(raw, &document))
			document["checkpointExtension"] = map[string]string{"keep": "opaque metadata"}
			raw, err = json.MarshalIndent(document, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600))
			for _, name := range []string{"rootfs.ext4", "memory.snap", "vmstate.snap"} {
				file, err := os.Create(filepath.Join(dir, name))
				require.NoError(t, err)
				size := int64(1)
				if name == "rootfs.ext4" || name == "memory.snap" {
					size = 1 << 20
				}
				if (mode == "short-memory" && name == "memory.snap") || (mode == "short-rootfs" && name == "rootfs.ext4") {
					size--
				}
				require.NoError(t, file.Truncate(size))
				require.NoError(t, file.Close())
			}
			metadata, upper, err := validatePackBundle(dir)
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, raw, metadata)
				require.Nil(t, upper)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestPackSnapshotExtrasPreservesOpaqueState(t *testing.T) {
	bundle := t.TempDir()
	payloads := map[string]string{
		"policy.json": `{"v":1,"incarnation":"source","revision":7,"policy":{"defaultAction":"deny"}}`,
		"vault.json":  `{"opaque":"encrypted record"}`,
	}
	for name, content := range payloads {
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte(content), 0600))
	}
	extras, err := packSnapshotExtras(bundle)
	require.NoError(t, err)
	require.Len(t, extras, len(payloads))
	// The exact exported records are packaged, without converting policy into bindings.
	for name, want := range payloads {
		raw, err := os.ReadFile(extras[name])
		require.NoError(t, err)
		require.Equal(t, want, string(raw))
	}
	for _, name := range []string{"policy.json", "vault.json"} {
		require.NoError(t, os.Remove(filepath.Join(bundle, name)))
	}
	extras, err = packSnapshotExtras(bundle)
	require.NoError(t, err)
	require.Equal(t, map[string]string{}, extras, "older stateless bundles remain packable")
}

func TestPackSnapshotExtrasRejectsUnreadableState(t *testing.T) {
	for _, name := range []string{"policy.json", "vault.json"} {
		t.Run(name, func(t *testing.T) {
			bundle := t.TempDir()
			require.NoError(t, os.Symlink(name, filepath.Join(bundle, name)))
			_, err := packSnapshotExtras(bundle)
			require.ErrorContains(t, err, name, "a state-file stat failure must not silently publish a stateless snapshot")
		})
	}
}

func TestReleasedPackMissingWorkIsIdempotentButOtherPhasesAreRetained(t *testing.T) {
	for _, mode := range []string{"released", "adding", "attached", "prepared", "outside", "dangling-link"} {
		t.Run(mode, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			bundle := t.TempDir()
			global.buildWorkDir = t.TempDir()
			work := filepath.Join(global.buildWorkDir, "snapshot_missing")
			phase := mode
			if mode == "outside" || mode == "dangling-link" {
				phase = packPhaseReleased
			}
			if mode == "outside" {
				work = filepath.Join(t.TempDir(), "snapshot_missing")
			}
			if mode == "dangling-link" {
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), work))
			}
			marker := filepath.Join(bundle, packActiveMarker)
			require.NoError(t, writePackResources(marker, work, phase))
			err := recoverPack(context.Background(), bundle, &fakeReleaser{}, func(string) error { t.Fatal("must not unmount absent work"); return nil })
			if mode == "released" {
				require.NoError(t, err)
				require.NoFileExists(t, marker)
				require.NoError(t, recoverPack(context.Background(), bundle, &fakeReleaser{}, nil))
			} else {
				require.Error(t, err)
				require.FileExists(t, marker)
			}
		})
	}
}

func TestPackRecoveryRejectsInvalidStateBeforeCleanup(t *testing.T) {
	for _, mode := range []string{"invalid-json", "missing-phase"} {
		t.Run(mode, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			global.buildWorkDir = t.TempDir()
			work := filepath.Join(global.buildWorkDir, "snapshot_test")
			require.NoError(t, os.MkdirAll(work, 0750))
			marker := filepath.Join(t.TempDir(), packActiveMarker)
			raw := `{"work":` + strconv.Quote(work) + `}`
			if mode == "invalid-json" {
				raw += "invalid"
			}
			require.NoError(t, os.WriteFile(marker, []byte(raw), 0600))
			err := recoverPack(context.Background(), filepath.Dir(marker), nil, func(string) error {
				t.Fatal("invalid state must not start cleanup")
				return nil
			})
			require.Error(t, err)
			require.DirExists(t, work)
			require.FileExists(t, marker)
		})
	}
}

type fenceTransport func(*http.Request) (*http.Response, error)

func (f fenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fenceReply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestPackRecoveryFencesLateAddBeforeReleasingFiles(t *testing.T) {
	for _, mode := range []string{"never-sent", "late-add", "disconnect", "unknown-reply", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			global.buildWorkDir = t.TempDir()
			bundle := t.TempDir()
			work := filepath.Join(global.buildWorkDir, "snapshot_test")
			require.NoError(t, os.MkdirAll(work, 0750))
			config := filepath.Join(work, snapfilesConfigName)
			require.NoError(t, os.WriteFile(config, []byte(`{"valid":"original"}`), 0600))
			backing := filepath.Join(work, "backing")
			require.NoError(t, os.WriteFile(backing, []byte("must retain until Del"), 0600))
			marker := filepath.Join(bundle, packActiveMarker)
			require.NoError(t, writePackResources(marker, work, packPhaseAdding))
			calls, deleted := 0, false
			c := &ublkdClient{httpClient: &http.Client{Transport: fenceTransport(func(req *http.Request) (*http.Response, error) {
				require.FileExists(t, backing)
				switch req.URL.Path {
				case "/v1/add":
					var input map[string]string
					require.NoError(t, json.NewDecoder(req.Body).Decode(&input))
					require.Equal(t, config, input["config"])
					raw, err := os.ReadFile(config)
					require.NoError(t, err)
					require.False(t, json.Valid(raw), "future Adds must be disabled first")
					calls++
					if mode == "disconnect" {
						return nil, errors.New("lost response")
					}
					if mode == "unknown-reply" {
						return fenceReply(500, `{"error":"unrecognized daemon"}`), nil
					}
					if mode == "timeout" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					if mode == "late-add" && calls == 1 {
						return fenceReply(409, `{"error":"an add of this image is already in progress"}`), nil
					}
					return fenceReply(500, `{"error":"device failed to start (see daemon log)"}`), nil
				case "/v1/list":
					require.Greater(t, calls, 0, "List alone cannot fence an Add")
					if mode == "late-add" && !deleted {
						return fenceReply(200, `{"ok":true,"devices":[{"dev_id":42,"config":`+strconv.Quote(config)+`}]}`), nil
					}
					return fenceReply(200, `{"ok":true,"devices":[]}`), nil
				case "/v1/del":
					var input map[string]int
					require.NoError(t, json.NewDecoder(req.Body).Decode(&input))
					require.Equal(t, 42, input["dev_id"])
					deleted = true
					return fenceReply(200, `{"ok":true}`), nil
				default:
					t.Fatal(req.URL.Path)
					return nil, nil
				}
			})}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "timeout" {
				ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer cancel()
			}
			err := recoverPack(ctx, bundle, c, func(string) error { return nil })
			if mode == "never-sent" || mode == "late-add" {
				require.NoError(t, err)
				require.NoDirExists(t, work)
				require.NoFileExists(t, marker)
				if mode == "late-add" {
					require.True(t, deleted)
					require.Equal(t, 2, calls)
				}
			} else {
				require.Error(t, err)
				require.FileExists(t, backing)
				require.FileExists(t, marker)
			}
		})
	}
}

// squashTestStubs installs commit/merge stubs: every commit is dirty, the
// merge validates the packaged tool's argument shape and concatenates the
// layer files named by the merge config into the output.
func squashTestStubs(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "overlaybd-commit"), []byte("#!/bin/sh\nhead -c 128 /dev/urandom > \"$5\"\n"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "overlaybd-merge"), []byte(`#!/bin/sh
if [ "$#" -ne 2 ] || [ ! -f "$1" ]; then
  echo "usage: overlaybd-merge <config> <out>" >&2
  exit 2
fi
: > "$2"
for layer in $(grep -o '"file":"[^"]*"' "$1" | sed 's/^"file":"//;s/"$//'); do
  cat "$layer" >> "$2" || exit 1
done
`), 0755))
}

func squashTestUpper(layers []ocispec.Descriptor) *ocitemplate.RootfsUpper {
	return &ocitemplate.RootfsUpper{
		Repository:       "127.0.0.1:1/sandbox",
		VirtualSizeBytes: 1 << 30,
		Lowers:           layers,
	}
}

// chainEnv reads FSB_CHAIN_* overrides for the verify script.
func chainEnv(maxLayers, squashCount, cycles int) (int, int, int) {
	for _, entry := range []struct {
		name string
		out  *int
	}{{"FSB_CHAIN_CYCLES", &cycles}, {"FSB_CHAIN_MAX_LAYERS", &maxLayers}, {"FSB_CHAIN_SQUASH_LAYERS", &squashCount}} {
		if raw := os.Getenv(entry.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err == nil && value > 0 {
				*entry.out = value
			}
		}
	}
	return maxLayers, squashCount, cycles
}

// TestPackRootfsSquashBoundsChainGrowth simulates pause cycles through the
// real packRootfs path: one dirty layer per cycle, blobs carried across
// cycles. The chain must never exceed maxRootfsLayers.
func TestPackRootfsSquashBoundsChainGrowth(t *testing.T) {
	maxLayers, squashCount, cycles := chainEnv(8, 4, 30)
	previous := global
	t.Cleanup(func() { global = previous })
	dir := t.TempDir()
	squashTestStubs(t, dir)
	global.overlaybdCommit = filepath.Join(dir, "overlaybd-commit")
	global.overlaybdMerge = filepath.Join(dir, "overlaybd-merge")
	global.overlaybdServiceConfig = filepath.Join(t.TempDir(), "overlaybd.json")

	ctx := context.Background()
	blobs := map[digest.Digest][]byte{}
	var chain []ocispec.Descriptor
	squashes := 0
	for cycle := 0; cycle < cycles; cycle++ {
		layout, _, err := createStagedArtifactLayout(t.TempDir(), fmt.Sprintf("cycle-%d", cycle))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(layout.blobDir(), 0o750))
		for _, layer := range chain {
			data, ok := blobs[layer.Digest]
			require.True(t, ok, "cycle %d: inherited blob %s must be materializable", cycle, layer.Digest)
			require.NoError(t, os.WriteFile(layout.blobPath(layer.Digest), data, 0o640))
		}
		bundle := t.TempDir()
		for _, name := range []string{ocitemplate.UpperDataName, ocitemplate.UpperIndexName} {
			require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("cycle payload"), 0600))
		}
		layers, err := packRootfs(ctx, bundle, t.TempDir(), layout, squashTestUpper(chain), maxLayers, squashCount)
		require.NoError(t, err)
		require.LessOrEqual(t, len(layers), maxLayers, "cycle %d exceeds the layer bound", cycle)
		if chain != nil && len(chain)+1 >= maxLayers && len(layers) == len(chain)-squashCount+2 {
			squashes++
			require.Equal(t, chain[:len(chain)+1-squashCount], layers[:len(layers)-1], "squash must keep the bottom layers")
		}
		for _, layer := range layers {
			if _, ok := blobs[layer.Digest]; !ok {
				data, err := os.ReadFile(layout.blobPath(layer.Digest))
				require.NoError(t, err)
				require.NotEmpty(t, data, "cycle %d: merged layer %s must retain data", cycle, layer.Digest)
				blobs[layer.Digest] = data
			}
		}
		t.Logf("CHAIN cycle=%d layers=%d", cycle+1, len(layers))
		chain = layers
	}
	require.GreaterOrEqual(t, chain[len(chain)-1].Size, int64(0))
	require.Greater(t, squashes, 0, "the cap must trigger squashes")
}

// TestPackRootfsSquashFallsBackOnMergeFailure publishes the un-squashed
// chain when the merge tool is unavailable.
func TestPackRootfsSquashFallsBackOnMergeFailure(t *testing.T) {
	previous := global
	t.Cleanup(func() { global = previous })
	dir := t.TempDir()
	squashTestStubs(t, dir)
	global.overlaybdCommit = filepath.Join(dir, "overlaybd-commit")
	global.overlaybdMerge = filepath.Join(dir, "missing-merge")
	global.overlaybdServiceConfig = filepath.Join(t.TempDir(), "overlaybd.json")

	chain := []ocispec.Descriptor{
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("l1"), Size: 4},
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("l2"), Size: 4},
	}
	layout, _, err := createStagedArtifactLayout(t.TempDir(), "fallback")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.blobDir(), 0o750))
	for _, layer := range chain {
		require.NoError(t, os.WriteFile(layout.blobPath(layer.Digest), []byte("data"), 0o640))
	}
	bundle := t.TempDir()
	for _, name := range []string{ocitemplate.UpperDataName, ocitemplate.UpperIndexName} {
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("payload"), 0600))
	}
	layers, err := packRootfs(context.Background(), bundle, t.TempDir(), layout, squashTestUpper(chain), 3, 2)
	require.NoError(t, err)
	require.Len(t, layers, len(chain)+1, "un-squashed chain is published when merge fails")
}

// TestPackRootfsSquashPreservesBaseLayers clamps the merge tail above the
// inherited template base boundary and drops the blobs the merge absorbed.
func TestPackRootfsSquashPreservesBaseLayers(t *testing.T) {
	previous := global
	t.Cleanup(func() { global = previous })
	dir := t.TempDir()
	squashTestStubs(t, dir)
	global.overlaybdCommit = filepath.Join(dir, "overlaybd-commit")
	global.overlaybdMerge = filepath.Join(dir, "overlaybd-merge")
	global.overlaybdServiceConfig = filepath.Join(t.TempDir(), "overlaybd.json")

	base := []ocispec.Descriptor{
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("base0"), Size: 4},
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("base1"), Size: 4},
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("base2"), Size: 4},
	}
	checkpointLayers := []ocispec.Descriptor{
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("delta0"), Size: 4},
		{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("delta1"), Size: 4},
	}
	chain := append(append([]ocispec.Descriptor{}, base...), checkpointLayers...)
	layout, _, err := createStagedArtifactLayout(t.TempDir(), "bounded-base")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.blobDir(), 0o750))
	for _, layer := range chain {
		// Blob contents are irrelevant to the merge stub, but sizes must
		// match the descriptors or ensureLocalBlob would try to fetch.
		require.NoError(t, os.WriteFile(layout.blobPath(layer.Digest), []byte("data"), 0o640))
	}
	upper := squashTestUpper(chain)
	upper.BaseLayers = len(base)
	bundle := t.TempDir()
	for _, name := range []string{ocitemplate.UpperDataName, ocitemplate.UpperIndexName} {
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("payload"), 0600))
	}
	// 5 inherited layers + the new root reach the bound of 6; the positional
	// tail of 4 would reach into the base, the clamp keeps it to 3.
	layers, err := packRootfs(context.Background(), bundle, t.TempDir(), layout, upper, 6, 4)
	require.NoError(t, err)
	require.Len(t, layers, len(base)+1)
	require.Equal(t, base, layers[:len(base)], "template base layers must survive untouched")
	for _, layer := range base {
		require.FileExists(t, layout.blobPath(layer.Digest), "base blobs stay in the store")
	}
	for _, layer := range checkpointLayers {
		require.NoFileExists(t, layout.blobPath(layer.Digest), "absorbed checkpoint blobs are cleaned up")
	}
	// The merged layer must be derived from the input layers: the stub
	// concatenates the files named by the merge config, bottom-first, so the
	// output is both checkpoint deltas followed by the committed root layer.
	merged, err := os.ReadFile(layout.blobPath(layers[len(layers)-1].Digest))
	require.NoError(t, err)
	require.Len(t, merged, 2*len("data")+128, "the merge must consume every tail layer")
	require.Equal(t, "datadata", string(merged[:2*len("data")]), "tail layers merge bottom-first")
}

// TestPackRootfsSquashRecoversOvershootChain collapses a chain that already
// exceeds the bound (after prior squash misses) back under it in one pack.
func TestPackRootfsSquashRecoversOvershootChain(t *testing.T) {
	maxLayers, squashCount := 24, 16
	previous := global
	t.Cleanup(func() { global = previous })
	dir := t.TempDir()
	squashTestStubs(t, dir)
	global.overlaybdCommit = filepath.Join(dir, "overlaybd-commit")
	global.overlaybdMerge = filepath.Join(dir, "overlaybd-merge")
	global.overlaybdServiceConfig = filepath.Join(t.TempDir(), "overlaybd.json")

	chain := make([]ocispec.Descriptor, 0, 40)
	for index := 0; index < 40; index++ {
		chain = append(chain, ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString(fmt.Sprintf("overshoot-%d", index)), Size: 4})
	}
	layout, _, err := createStagedArtifactLayout(t.TempDir(), "overshoot")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.blobDir(), 0o750))
	for _, layer := range chain {
		require.NoError(t, os.WriteFile(layout.blobPath(layer.Digest), []byte("data"), 0o640))
	}
	bundle := t.TempDir()
	for _, name := range []string{ocitemplate.UpperDataName, ocitemplate.UpperIndexName} {
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("payload"), 0600))
	}
	layers, err := packRootfs(context.Background(), bundle, t.TempDir(), layout, squashTestUpper(chain), maxLayers, squashCount)
	require.NoError(t, err)
	// 40 inherited + the new root: 41 -> 26 -> 11, below the bound in one pack.
	require.Less(t, len(layers), maxLayers)
	require.Equal(t, 11, len(layers))
	require.Equal(t, chain[:10], layers[:10], "repeated squashes keep the surviving bottom layers")
	// Only the surviving chain's blobs remain in the store.
	kept := map[digest.Digest]bool{}
	for _, layer := range layers {
		kept[layer.Digest] = true
	}
	entries, err := os.ReadDir(layout.blobDir())
	require.NoError(t, err)
	require.Len(t, entries, len(layers), "absorbed blobs are cleaned up")
	for _, entry := range entries {
		require.Regexp(t, `^[0-9a-f]{64}$`, entry.Name())
	}
}
