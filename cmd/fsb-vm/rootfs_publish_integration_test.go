package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/retry"

	"fast-sandbox/internal/ocitemplate"
)

// Requires root, ublkd, and real OverlayBD tools; consumes only an upper copy already captured by the external lab.
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
	require.NotEmpty(t, global.ublkdSocket)
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
	baseData, baseIndex, baseLayer := filepath.Join(root, "base.data"), filepath.Join(root, "base.index"), filepath.Join(root, "base.zfile")
	// The source test disk has no remote parent; create an equivalent immutable zero-filled base to verify OCI transfer of the real upper.
	require.NoError(t, createHybridUpper(t.Context(), global.overlaybdCreate, baseData, baseIndex, 1))
	require.NoError(t, commitLayer(t.Context(), baseData, baseIndex, baseLayer, true))
	payload, err := os.ReadFile(baseLayer)
	require.NoError(t, err)
	desc := oci.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromBytes(payload), Size: int64(len(payload))}
	server := httptest.NewTLSServer(registrytest.New())
	defer server.Close()
	oldClient := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = oldClient }()
	repository := strings.TrimPrefix(server.URL, "https://") + "/rootfs/base"
	repo, _, err := ocitemplate.Repository(repository+":base", nil)
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
	raw, err = json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), raw, 0600))
	require.NoError(t, packCapturedBundle(t.Context(), bundle, "rootfs:test", 24, 16, true))
	auth := filepath.Join(root, "auth.json")
	require.NoError(t, os.WriteFile(auth, []byte(`{"auths":{}}`), 0600))
	destination := strings.TrimPrefix(server.URL, "https://") + "/rootfs/published:test"
	command := newPushCommand()
	command.SetArgs([]string{"--artifact-dir", filepath.Join(global.buildOutputDir, "rootfs_test"), "--destination", destination, "--metadata-file", filepath.Join(bundle, "manifest.json"), "--auth-file", auth})
	require.NoError(t, command.Execute())
	target, ref, err := ocitemplate.Repository(destination, nil)
	require.NoError(t, err)
	resolved, err := ocitemplate.ResolveFrom(t.Context(), ref, target)
	require.NoError(t, err)
	pair, err := ocitemplate.Decode(resolved)
	require.NoError(t, err)
	// Read the manifest and every layer back from the registry, verifying digests before downstream mount validation.
	for _, entry := range []struct{ name, ref string }{{"rootfs", pair.Rootfs}, {"snapfiles", pair.Snapfiles}} {
		remote, parsed, e := ocitemplate.Repository(entry.ref, nil)
		require.NoError(t, e)
		md, e := remote.Resolve(t.Context(), parsed.Reference)
		require.NoError(t, e)
		reader, e := remote.Manifests().Fetch(t.Context(), md)
		require.NoError(t, e)
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
		raw, e = json.Marshal(paths)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(root, entry.name+"-layers.json"), raw, 0600))
	}
	t.Log("real upper packed; rootfs and metadata layers published, fetched, and digest verified")
}
