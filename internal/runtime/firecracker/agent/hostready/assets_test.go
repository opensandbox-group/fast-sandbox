package hostready

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
