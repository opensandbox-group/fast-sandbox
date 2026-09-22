package hostready

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// serveAssets spins an httptest server acting as the Firecracker release
// host and kernel blob store. The fake tarball carries the release layout
// firecracker-v1.16.1-x86_64/firecracker-v1.16.1-x86_64 (and jailer).
func serveAssets(t *testing.T) *httptest.Server {
	t.Helper()
	var tarball []byte
	{
		buffer := &bytes.Buffer{}
		writer := gzip.NewWriter(buffer)
		archive := tar.NewWriter(writer)
		for _, name := range []string{"firecracker-v1.16.1-x86_64", "jailer-v1.16.1-x86_64"} {
			if err := archive.WriteHeader(&tar.Header{Name: "release-v1.16.1/" + name, Mode: 0o755, Size: int64(len(name))}); err != nil {
				t.Fatal(err)
			}
			if _, err := archive.Write([]byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		tarball = buffer.Bytes()
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case regexp.MustCompile(`firecracker-.*\.tgz$`).MatchString(request.URL.Path):
			writer.Write(tarball)
		case request.URL.Path == "/vmlinux.bin":
			writer.Write([]byte("fake kernel blob"))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAssetEnsureInstallsAndVerifies(t *testing.T) {
	if _, err := assetArch(); err != nil {
		t.Skipf("asset install only runs on supported arch: %v", err)
	}
	server := serveAssets(t)
	dir := filepath.Join(t.TempDir(), "fc")
	config := AssetConfig{
		Dir:         dir,
		FCVersion:   "v1.16.1",
		KernelURL:   server.URL + "/vmlinux.bin",
		ReleaseBase: server.URL,
		VerifyBinary: func(path string) error {
			// The fake payload doubles as a marker: it exists -> ok.
			if _, err := os.Stat(path); err != nil {
				return errors.New("missing")
			}
			return nil
		},
	}
	if err := config.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	for _, name := range []string{"firecracker", "jailer", "vmlinux.bin"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() == 0 {
			t.Fatalf("expected %s installed: err=%v", name, err)
		}
	}
}

func TestAssetEnsureIdempotent(t *testing.T) {
	server := serveAssets(t)
	dir := filepath.Join(t.TempDir(), "fc")
	config := AssetConfig{
		Dir:          dir,
		KernelURL:    server.URL + "/vmlinux.bin",
		ReleaseBase:  server.URL,
		VerifyBinary: func(path string) error { return nil },
	}
	// Pre-create a verifiable layout: Ensure must short-circuit (no
	// network). Point the base at a closed server to prove it.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	closedConfig := config
	closedConfig.ReleaseBase = closed.URL
	closedConfig.KernelURL = closed.URL + "/vmlinux.bin"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vmlinux.bin"), []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := closedConfig.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure must short-circuit on a verified layout: %v", err)
	}
}

func TestAssetEnsureDownloadFailureFails(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	config := AssetConfig{
		Dir:         filepath.Join(t.TempDir(), "fc"),
		KernelURL:   closed.URL + "/vmlinux.bin",
		ReleaseBase: closed.URL,
	}
	if err := config.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure must fail when the release host is unreachable")
	}
}

func TestKernelURLDefaultsToX86Only(t *testing.T) {
	// x86_64 is the only supported architecture: the default kernel URL
	// is the pinned x86_64 blob, and the installer refuses other arches
	// before any download (assetArch, asserted via the Ensure skip).
	require.Equal(t, defaultKernelURL, AssetConfig{}.kernelURL())
	custom := AssetConfig{KernelURL: "https://mirror.example/vmlinux.bin"}
	require.Equal(t, "https://mirror.example/vmlinux.bin", custom.kernelURL())
}

func TestDownloadRejectsOversizedBody(t *testing.T) {
	// A body at/over the cap must error, not silently truncate into an
	// installable-looking blob.
	big := make([]byte, maxAssetBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(big)
	}))
	t.Cleanup(server.Close)
	config := AssetConfig{HTTPClient: server.Client()}
	err := config.download(context.Background(), server.URL+"/huge", filepath.Join(t.TempDir(), "out"))
	require.ErrorContains(t, err, "asset cap")
}

func TestDownloadAcceptsBodiesUnderTheCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("small blob"))
	}))
	t.Cleanup(server.Close)
	config := AssetConfig{HTTPClient: server.Client()}
	out := filepath.Join(t.TempDir(), "out")
	require.NoError(t, config.download(context.Background(), server.URL+"/ok", out))
	payload, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "small blob", string(payload))
}

func TestDefaultHTTPClientCarriesTimeouts(t *testing.T) {
	require.NotSame(t, http.DefaultClient, AssetConfig{}.httpClient())
	transport, ok := defaultHTTPClient.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, defaultTLSHandshakeTimeout, transport.TLSHandshakeTimeout)
	require.Equal(t, defaultResponseHeaderTimeout, transport.ResponseHeaderTimeout)
	require.NotNil(t, transport.DialContext)
	require.True(t, transport.Proxy != nil)
}

func TestDownloadWithDefaultClientStillWorks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("default client blob"))
	}))
	t.Cleanup(server.Close)
	config := AssetConfig{}
	out := filepath.Join(t.TempDir(), "out")
	require.NoError(t, config.download(context.Background(), server.URL+"/ok", out))
	payload, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "default client blob", string(payload))
}

// statOKVerifyBinary accepts any existing file as a "working" binary: the
// fake bundle payloads double as markers.
func statOKVerifyBinary(path string) error {
	if _, err := os.Stat(path); err != nil {
		return errors.New("missing")
	}
	return nil
}

// writeBundleFixture materializes an image-bundle dir with the three fake
// assets and a real SHA256SUMS manifest (digests of the actual bytes).
func writeBundleFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"firecracker": []byte("fake firecracker binary"),
		"jailer":      []byte("fake jailer binary"),
		"vmlinux.bin": []byte("fake kernel blob"),
	}
	manifest := &strings.Builder{}
	for _, name := range []string{"firecracker", "jailer", "vmlinux.bin"} {
		payload := files[name]
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0o755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(manifest, "%x  %s\n", sha256.Sum256(payload), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(manifest.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// closedServer returns the URL of an already-closed server: any request
// against it fails, proving the code under test stayed off the network.
func closedServer() string {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	return server.URL
}

// mustGlob is filepath.Glob for assertions.
func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	require.NoError(t, err)
	return matches
}

func TestAssetEnsureInstallsFromBundle(t *testing.T) {
	if _, err := assetArch(); err != nil {
		t.Skipf("asset install only runs on supported arch: %v", err)
	}
	// Stock pins + a served bundle: Ensure installs everything without
	// touching the network (closed release host and kernel URL).
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeBundleFixture(t, bundle)
	dir := filepath.Join(t.TempDir(), "fc")
	closed := closedServer()
	config := AssetConfig{
		Dir:          dir,
		BundleDir:    bundle,
		KernelURL:    closed + "/vmlinux.bin",
		ReleaseBase:  closed,
		VerifyBinary: statOKVerifyBinary,
	}
	require.NoError(t, config.Ensure(context.Background()))
	for _, name := range []string{"firecracker", "jailer", "vmlinux.bin"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, name)
		require.NotZero(t, info.Size(), name)
	}
	require.Empty(t, mustGlob(t, filepath.Join(dir, "*.download")), "no partial files may remain")
}

func TestAssetEnsureFallsBackToDownloadWhenBundleMissing(t *testing.T) {
	if _, err := assetArch(); err != nil {
		t.Skipf("asset install only runs on supported arch: %v", err)
	}
	// A missing bundle (bare-process run) must not fail Ensure.
	server := serveAssets(t)
	config := AssetConfig{
		Dir:          filepath.Join(t.TempDir(), "fc"),
		BundleDir:    filepath.Join(t.TempDir(), "no-such-bundle"),
		FCVersion:    "v1.16.1",
		KernelURL:    server.URL + "/vmlinux.bin",
		ReleaseBase:  server.URL,
		VerifyBinary: statOKVerifyBinary,
	}
	require.NoError(t, config.Ensure(context.Background()))
	for _, name := range []string{"firecracker", "jailer", "vmlinux.bin"} {
		require.FileExists(t, filepath.Join(config.Dir, name))
	}
}

func TestBundleFallsBackOnDigestMismatch(t *testing.T) {
	// A tampered bundle file is refused; intact files still install.
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeBundleFixture(t, bundle)
	if err := os.WriteFile(filepath.Join(bundle, "firecracker"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "fc")
	config := AssetConfig{Dir: dir, BundleDir: bundle, VerifyBinary: statOKVerifyBinary}
	err := config.installFromBundle(dir)
	require.ErrorContains(t, err, "sha256 mismatch")
	require.FileExists(t, filepath.Join(dir, "jailer"), "intact files still install")
	require.NoFileExists(t, filepath.Join(dir, "firecracker"))
	require.Empty(t, mustGlob(t, filepath.Join(dir, "*.download")))
}

func TestBundleGating(t *testing.T) {
	// Custom pins gate the bundle per file: custom KernelURL keeps the
	// kernel out but serves the stock binaries, custom FCVersion the
	// reverse.
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeBundleFixture(t, bundle)

	customKernel := AssetConfig{
		Dir:          filepath.Join(t.TempDir(), "fc-kernel"),
		BundleDir:    bundle,
		KernelURL:    "https://mirror.example/vmlinux.bin",
		VerifyBinary: statOKVerifyBinary,
	}
	require.NoError(t, customKernel.installFromBundle(customKernel.Dir))
	require.FileExists(t, filepath.Join(customKernel.Dir, "firecracker"))
	require.FileExists(t, filepath.Join(customKernel.Dir, "jailer"))
	require.NoFileExists(t, filepath.Join(customKernel.Dir, "vmlinux.bin"))

	customVersion := AssetConfig{
		Dir:          filepath.Join(t.TempDir(), "fc-version"),
		BundleDir:    bundle,
		FCVersion:    "v1.17.0",
		VerifyBinary: statOKVerifyBinary,
	}
	require.NoError(t, customVersion.installFromBundle(customVersion.Dir))
	require.NoFileExists(t, filepath.Join(customVersion.Dir, "firecracker"))
	require.NoFileExists(t, filepath.Join(customVersion.Dir, "jailer"))
	require.FileExists(t, filepath.Join(customVersion.Dir, "vmlinux.bin"))
}

func TestBundleMissingManifestIsAnError(t *testing.T) {
	config := AssetConfig{
		Dir:          filepath.Join(t.TempDir(), "fc"),
		BundleDir:    t.TempDir(),
		VerifyBinary: statOKVerifyBinary,
	}
	require.ErrorContains(t, config.installFromBundle(config.Dir), "SHA256SUMS")
}

func TestParseSHA256SumsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(path, []byte("deadbeef  firecracker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parseSHA256Sums(path)
	require.ErrorContains(t, err, "malformed")
}

func TestDockerfileBundlePinsMatchGoDefaults(t *testing.T) {
	// A drift would silently strand the bundle (or serve a different
	// asset than the download path).
	payload, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "build", "Dockerfile.firecracker-runtime"))
	require.NoError(t, err)
	pins := map[string]string{}
	for _, line := range strings.Split(string(payload), "\n") {
		for _, name := range []string{"FC_VERSION", "FC_KERNEL_URL"} {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG "+name+"="); ok {
				pins[name] = value
			}
		}
	}
	require.Equal(t, DefaultFCVersion, pins["FC_VERSION"], "Dockerfile FC_VERSION drifted from DefaultFCVersion")
	require.Equal(t, defaultKernelURL, pins["FC_KERNEL_URL"], "Dockerfile FC_KERNEL_URL drifted from defaultKernelURL")
}
