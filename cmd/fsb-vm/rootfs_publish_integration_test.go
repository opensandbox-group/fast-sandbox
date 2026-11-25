package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/retry"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
)

// Requires real OverlayBD tools and an upper copy captured by the external lab.
// The Registry is local to this test; nothing is published to an external repository.
func TestRootfsPackPublishIntegration(t *testing.T) {
	if os.Getenv("FSB_TEST_ROOTFS_PACK") != "1" {
		t.Skip("requires disposable OverlayBD lab")
	}
	source := os.Getenv("FSB_TEST_PACK_SOURCE")
	root := os.Getenv("FSB_TEST_PACK_OUTPUT")
	require.NotEmpty(t, source)
	require.NotEmpty(t, root)
	require.NoError(t, os.Mkdir(root, 0700))
	old := global
	t.Cleanup(func() { global = old })
	global.ublkdSocket = os.Getenv("FSB_TEST_UBLKD_SOCKET")
	if global.ublkdSocket == "" {
		global.ublkdSocket = filepath.Join(root, "no-ublkd.sock")
	}
	tools := os.Getenv("FSB_TEST_OVERLAYBD_TOOLS")
	require.NotEmpty(t, tools)
	global.overlaybdCreate = filepath.Join(tools, "overlaybd-create")
	global.overlaybdCommit = filepath.Join(tools, "overlaybd-commit")
	global.buildWorkDir = filepath.Join(root, "work")
	global.buildOutputDir = filepath.Join(root, "artifacts")
	global.manifestCacheDir = filepath.Join(root, "cache")
	global.credFile = filepath.Join(root, "credentials.json")
	bundle := filepath.Join(root, "bundle")
	require.NoError(t, os.Mkdir(bundle, 0700))
	for _, entry := range []struct{ source, target string }{{"data", ocitemplate.UpperDataName}, {"index", ocitemplate.UpperIndexName}} {
		require.NoError(t, copyFile(filepath.Join(source, entry.source), filepath.Join(bundle, entry.target), 0600))
	}
	kernel := os.Getenv("FSB_TEST_PACK_BOOT_KERNEL")
	seed := os.Getenv("FSB_TEST_PACK_BOOT_DISK")
	var payload []byte
	var desc oci.Descriptor
	if kernel != "" {
		data, index := filepath.Join(root, "raw.data"), filepath.Join(root, "raw.index")
		sealed := filepath.Join(root, "raw.lsmt")
		cmd := exec.CommandContext(t.Context(), filepath.Join(tools, "overlaybd-import-raw"), seed, data, index, sealed)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		mergeConfig, err := json.Marshal(overlaybdMergeConfig{Lowers: []overlaybdMergeLayer{{File: sealed}}})
		require.NoError(t, err)
		configPath := filepath.Join(root, "base-merge.json")
		require.NoError(t, os.WriteFile(configPath, mergeConfig, 0600))
		compressed := filepath.Join(root, "raw.zfile")
		cmd = exec.CommandContext(t.Context(), filepath.Join(tools, "overlaybd-merge"), "--service_config_path", os.Getenv("FSB_TEST_PACK_BOOT_SERVICE_CONFIG"), configPath, compressed)
		output, err = cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		payload, err = os.ReadFile(compressed)
		require.NoError(t, err)
		desc = oci.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromBytes(payload), Size: int64(len(payload))}
	} else {
		baseData, baseIndex, baseLayer := filepath.Join(root, "base.data"), filepath.Join(root, "base.index"), filepath.Join(root, "base.zfile")
		// The source test disk has no remote parent; create an equivalent immutable zero-filled base to verify OCI transfer of the real upper.
		require.NoError(t, createHybridUpper(t.Context(), global.overlaybdCreate, baseData, baseIndex, 1))
		require.NoError(t, commitLayer(t.Context(), baseData, baseIndex, baseLayer, true))
		var err error
		payload, err = os.ReadFile(baseLayer)
		require.NoError(t, err)
		desc = oci.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromBytes(payload), Size: int64(len(payload))}
	}
	server := httptest.NewServer(registrytest.New())
	global.plainHTTP = true
	defer server.Close()
	oldClient := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = oldClient }()
	repository := strings.TrimPrefix(server.URL, "http://") + "/rootfs/base"
	repo, _, err := ocitemplate.Repository(repository+":base", nil, ocitemplate.WithPlainHTTP(true))
	require.NoError(t, err)
	require.NoError(t, repo.Blobs().Push(t.Context(), desc, bytes.NewReader(payload)))
	upper := ocitemplate.RootfsUpper{Repository: repository, VirtualSizeBytes: 1 << 30, Lowers: []oci.Descriptor{desc}, BaseLayers: 1}
	raw, err := json.Marshal(upper)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, ocitemplate.RootfsUpperName), raw, 0600))
	m := rootfsBootManifest()
	m.Machine.MemoryMiB = 256
	m.Machine.RootfsSizeMiB = 1024
	m.Kernel.Version = "6.1.177"
	if kernel != "" {
		global.firecrackerBin = os.Getenv("FSB_TEST_PACK_FIRECRACKER")
		require.NotEmpty(t, global.firecrackerBin)
		m.Compatibility.FirecrackerVersion = artifacts.FirecrackerVersion(global.firecrackerBin)
	}
	raw, err = json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), raw, 0600))
	require.NoError(t, packCapturedBundle(t.Context(), bundle, "rootfs:test", 24, 16, packOptions{rootfsOnly: true, singleImage: true}))
	auth := filepath.Join(root, "auth.json")
	require.NoError(t, os.WriteFile(auth, []byte(`{"auths":{}}`), 0600))
	destination := strings.TrimPrefix(server.URL, "http://") + "/rootfs/published:test"
	command := newPushCommand()
	command.SetArgs([]string{"--artifact-dir", filepath.Join(global.buildOutputDir, "rootfs_test"), "--destination", destination, "--metadata-file", filepath.Join(bundle, "manifest.json"), "--auth-file", auth})
	require.NoError(t, command.Execute())
	target, ref, err := ocitemplate.Repository(destination, nil, ocitemplate.WithPlainHTTP(true))
	require.NoError(t, err)
	resolved, err := ocitemplate.ResolveFrom(t.Context(), ref, target)
	require.NoError(t, err)
	pair, err := ocitemplate.Decode(resolved)
	require.NoError(t, err)
	require.NotNil(t, pair.Metadata)
	require.Empty(t, pair.Snapfiles)
	require.JSONEq(t, string(raw), string(pair.Metadata.Manifest))
	named := templateFile{Template: templateRef{Repo: ref.Registry + "/" + ref.Repository, TemplateID: ref.Reference}}
	require.NoError(t, resolveTemplate(t.Context(), &named, dockerConfig{}))
	require.Equal(t, pair.Rootfs, named.rootfsRef())
	require.Empty(t, named.snapfilesRef())
	// Read the rootfs layers back from the registry before downstream device validation.
	remote, parsed, err := ocitemplate.Repository(pair.Rootfs, nil, ocitemplate.WithPlainHTTP(true))
	require.NoError(t, err)
	md, err := remote.Resolve(t.Context(), parsed.Reference)
	require.NoError(t, err)
	reader, err := remote.Manifests().Fetch(t.Context(), md)
	require.NoError(t, err)
	var manifest oci.Manifest
	require.NoError(t, json.NewDecoder(reader).Decode(&manifest))
	require.NoError(t, reader.Close())
	var paths []string
	for _, layer := range manifest.Layers {
		stream, e := remote.Blobs().Fetch(t.Context(), layer)
		require.NoError(t, e)
		body, e := io.ReadAll(stream)
		require.NoError(t, e)
		require.NoError(t, stream.Close())
		require.Equal(t, layer.Digest, digest.FromBytes(body))
		path := filepath.Join(root, layer.Digest.Encoded()+".zfile")
		require.NoError(t, os.WriteFile(path, body, 0600))
		paths = append(paths, path)
	}
	raw, err = json.Marshal(paths)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "rootfs-layers.json"), raw, 0600))
	t.Log("real upper packed without a snapfiles device; rootfs layers and config metadata published and verified")
	if kernel != "" {
		testRootfsMetadataColdBoot(t, root, kernel, named.Template.Repo, named.Template.TemplateID)
		if old := os.Getenv("FSB_TEST_PACK_LEGACY_CLI"); old != "" {
			testDefaultPackWithLegacyReader(t, root, bundle, kernel, named.Template.Repo, old)
		}
	}
}

func testDefaultPackWithLegacyReader(t *testing.T, root, bundle, kernel, repo, cli string) {
	t.Helper()
	pack := newPackCommand()
	pack.SetArgs([]string{"--bundle-dir", bundle, "--output", "rootfs:legacy", "--rootfs-only"})
	require.NoError(t, pack.ExecuteContext(t.Context()))
	staged := filepath.Join(global.buildOutputDir, "rootfs_legacy")
	raw, err := os.ReadFile(filepath.Join(staged, "manifest", "index.json"))
	require.NoError(t, err)
	var index layoutIndex
	require.NoError(t, json.Unmarshal(raw, &index))
	require.Len(t, index.Images, 2)
	raw, err = os.ReadFile(filepath.Join(bundle, inDiskManifest))
	require.NoError(t, err)
	ref, _, err := ocitemplate.Push(t.Context(), staged, repo+":legacy", raw, nil, ocitemplate.WithPlainHTTP(true))
	require.NoError(t, err)
	current := templateFile{Template: templateRef{Repo: repo, TemplateID: "legacy"}}
	require.NoError(t, resolveTemplate(t.Context(), &current, dockerConfig{}))
	require.Nil(t, current.RootfsMetadata)
	require.Equal(t, ref, ocitemplate.Prefix+current.snapfilesRef())

	const id = "legacy-reader"
	stateDir, runtimeDir := filepath.Join(root, "state"), filepath.Join(root, "run")
	input := filepath.Join(root, "legacy-reader.json")
	raw, err = json.Marshal(templateFile{SandboxID: id, Template: templateRef{Reference: ref}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input, raw, 0600))
	flags := []string{"--plain-http", "--ublkd-socket", global.ublkdSocket, "--cred-file", global.credFile,
		"--state-dir", stateDir, "--runtime-dir", runtimeDir, "--firecracker", global.firecrackerBin,
		"--overlaybd-create", global.overlaybdCreate}
	t.Cleanup(func() {
		if pathExists(filepath.Join(stateDir, id)) {
			output, err := exec.Command(cli, append(flags, "delete", id)...).CombinedOutput()
			require.NoError(t, err, string(output))
		}
	})
	output, err := exec.CommandContext(t.Context(), cli, append(flags, "create", "-f", input, "--kernel", kernel)...).CombinedOutput()
	require.NoError(t, err, string(output))
	t.Logf("old CLI accepted and started the default two-image RootfsOnly artifact: %s", output)
	serial := filepath.Join(runtimeDir, id, serialLogName)
	if marker := os.Getenv("FSB_TEST_PACK_BOOT_MARKER"); marker != "" {
		require.NoError(t, waitSerialFor(serial, marker, 25*time.Second))
	}
}

func testRootfsMetadataColdBoot(t *testing.T, root, kernel, repo, templateID string) {
	t.Helper()
	global.stateDir, global.runtimeDir = filepath.Join(root, "state"), filepath.Join(root, "run")
	const id = "metadata"
	path := filepath.Join(root, "input.json")
	raw, err := json.Marshal(templateFile{SandboxID: id, Template: templateRef{Repo: repo, TemplateID: templateID}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	layout := newSandboxLayout(id)
	defer func() {
		if pathExists(layout.home) {
			require.NoError(t, runDelete(context.Background(), id))
		}
	}()
	require.NoError(t, runCreate(t.Context(), &createOptions{templateFile: path, kernel: kernel}))
	state, err := readState(layout.home)
	require.NoError(t, err)
	require.NotEmpty(t, state.RootfsMetadataSHA256)
	require.NotNil(t, state.Rootfs)
	require.Nil(t, state.Snapfiles)
	require.Empty(t, state.SharedSnapfiles)
	require.False(t, isMounted(layout.mountPoint))
	devices, err := newUblkdClient(global.ublkdSocket).List(t.Context())
	require.NoError(t, err)
	require.Len(t, devices, 1, "the disposable daemon must only own the rootfs device")
	require.Equal(t, state.Rootfs.DevID, devices[0].DevID)
	defer func() {
		if t.Failed() {
			serial, _ := os.ReadFile(layout.serialLog)
			t.Logf("guest serial output: %s", serial)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if marker := os.Getenv("FSB_TEST_PACK_BOOT_MARKER"); marker != "" {
		require.NoError(t, waitSerialFor(layout.serialLog, marker, 25*time.Second))
		return
	}
	require.Eventually(t, func() bool {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err := probeRootfsHelper(probe, filepath.Join(layout.runtime, "guest-control.sock"), 1234)
		return err == nil
	}, 25*time.Second, 300*time.Millisecond, "guest helper did not respond after cold boot")
}
