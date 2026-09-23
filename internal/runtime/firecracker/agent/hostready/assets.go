package hostready

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Firecracker asset pins: the release the runtime plan points at
// (config/runtime-environments.yaml binaryPath). No guest kernel: it is
// a build-time asset baked into the snapshots, and restore never boots
// one (#84).
const (
	// DefaultFCVersion is the pinned Firecracker release.
	DefaultFCVersion = "v1.16.1"
	// DefaultAssetsDir is the on-node install target (matches
	// config/runtime-environments.yaml binaryPath).
	DefaultAssetsDir = "/opt/fast-sandbox/firecracker"
	// releaseBaseURL is the Firecracker GitHub release root.
	releaseBaseURL = "https://github.com/firecracker-microvm/firecracker/releases"
	// maxAssetBytes bounds one downloaded asset.
	maxAssetBytes = 1 << 30
)

// Fallback download timeouts (AssetConfig.HTTPClient == nil); the body
// transfer stays unbounded.
const (
	defaultDialTimeout           = 10 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	// downloadProgressInterval is the byte stride of the progress logs.
	downloadProgressInterval = 8 << 20
)

// Asset file names inside the assets dir. The release tarballs carry
// version-suffixed binaries; the local copies use these bare names.
const (
	assetFirecracker = "firecracker"
	assetJailer      = "jailer"
)

// defaultHTTPClient is the nil-HTTPClient fallback: like
// http.DefaultClient, but with explicit connect/TLS/response-header
// timeouts.
var defaultHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
}()

// AssetConfig describes the Firecracker asset installation.
type AssetConfig struct {
	// Dir is the install target (default DefaultAssetsDir).
	Dir string
	// FCVersion is the pinned Firecracker release (default v1.16.1).
	FCVersion string
	// ReleaseBase overrides the GitHub release root (tests point it at an
	// httptest server).
	ReleaseBase string
	// BundleDir is the in-image copy of the pinned assets (Dockerfile
	// fc-assets stage); empty = defaultBundleDir. Served instead of the
	// download path when the config keeps the stock pins.
	BundleDir string
	// HTTPClient fetches the assets (nil = a timed-out default client).
	HTTPClient *http.Client
	// VerifyBinary runs "<path> --version" (nil = exec the binary).
	VerifyBinary func(path string) error
}

// assetArch maps the Go arch onto the Firecracker release asset arch.
// Only amd64 is supported today: arm64 nodes fail the cpu-arch check and
// are never labeled ready, and the installer refuses them as a second
// line of defense.
func assetArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	default:
		return "", fmt.Errorf("architecture %s is not supported yet (x86_64 only)", runtime.GOARCH)
	}
}

// Ensure materializes {Dir}/{firecracker,jailer}, downloading whatever is
// missing and verifying the result. It is idempotent: a fully installed
// and verifiable directory short-circuits without network.
func (c AssetConfig) Ensure(ctx context.Context) error {
	dir := c.dir()
	if err := c.verify(); err == nil {
		klog.InfoS("firecracker assets already installed", "dir", dir)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create asset dir %s: %w", dir, err)
	}
	arch, err := assetArch()
	if err != nil {
		return err
	}
	// Zero-network source first; the downloads below per-file skip
	// whatever the bundle installed, so a bad bundle only costs a log.
	if err := c.installFromBundle(dir); err != nil {
		klog.InfoS("bundled firecracker assets unusable; falling back to downloads",
			"bundle", c.bundleDir(), "err", err)
	}
	binary := filepath.Join(dir, assetFirecracker)
	jailer := filepath.Join(dir, assetJailer)
	if c.binaryMissing(binary) || c.binaryMissing(jailer) {
		version := c.version()
		url := fmt.Sprintf("%s/download/%s/firecracker-%s-%s.tgz", c.base(), version, version, arch)
		if err := c.installTarball(ctx, url, dir, arch); err != nil {
			return err
		}
	}
	if err := c.verify(); err != nil {
		return fmt.Errorf("verify installed firecracker assets: %w", err)
	}
	klog.InfoS("firecracker assets installed", "dir", dir, "version", c.version())
	return nil
}

// verify executes both binaries.
func (c AssetConfig) verify() error {
	dir := c.dir()
	verify := c.VerifyBinary
	if verify == nil {
		verify = verifyBinary
	}
	for _, name := range []string{assetFirecracker, assetJailer} {
		if err := verify(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// binaryMissing reports whether the binary at path cannot execute.
func (c AssetConfig) binaryMissing(path string) bool {
	verify := c.VerifyBinary
	if verify == nil {
		verify = verifyBinary
	}
	return verify(path) != nil
}

// installTarball downloads url and extracts the firecracker/jailer
// binaries for arch into dir (the tarball layout nests release-v*/).
func (c AssetConfig) installTarball(ctx context.Context, url, dir, arch string) error {
	tmp, err := os.MkdirTemp(dir, "install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tarball := filepath.Join(tmp, "firecracker.tgz")
	if err := c.download(ctx, url, tarball); err != nil {
		return err
	}
	if err := extractTarball(tarball, tmp, arch); err != nil {
		return err
	}
	for name, target := range map[string]string{
		assetFirecracker: filepath.Join(dir, assetFirecracker),
		assetJailer:      filepath.Join(dir, assetJailer),
	} {
		matches, err := filepath.Glob(filepath.Join(tmp, name+"-*-"+arch))
		if err != nil || len(matches) != 1 {
			if err == nil {
				err = fmt.Errorf("found %d candidates", len(matches))
			}
			return fmt.Errorf("locate %s in the release tarball: %w", name, err)
		}
		if err := os.Rename(matches[0], target); err != nil {
			return err
		}
		if err := os.Chmod(target, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// isAssetBinary matches a release tarball entry ("jailer-v1.16.1-x86_64")
// as one of the two binaries for arch.
func isAssetBinary(base, arch string) bool {
	for _, name := range []string{"firecracker-", "jailer-"} {
		if strings.HasPrefix(base, name) && strings.HasSuffix(base, "-"+arch) {
			return true
		}
	}
	return false
}

// extractTarball unpacks the gzipped tarball into dest, skipping anything
// that is not a regular file with a clean relative path.
func extractTarball(tarball, dest, arch string) error {
	file, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open %s: %w", tarball, err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", tarball, err)
		}
		name := header.Name
		if header.Typeflag != tar.TypeReg || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			continue
		}
		base := filepath.Base(filepath.Clean(name))
		if !isAssetBinary(base, arch) {
			continue
		}
		payload, err := io.ReadAll(io.LimitReader(reader, maxAssetBytes))
		if err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dest, base), payload, 0o755); err != nil { //nolint:gosec // installing executable host binaries
			return err
		}
	}
}

// download streams url into path, rejecting bodies at or over the size
// cap (a silent truncation would hand the node a corrupt binary that
// still passes the non-empty verify).
func (c AssetConfig) download(ctx context.Context, url, path string) error {
	start := time.Now()
	klog.InfoS("asset download started", "url", url, "dest", path)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		klog.ErrorS(err, "asset download failed", "url", url, "elapsed", time.Since(start).Round(time.Millisecond))
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", url, response.StatusCode)
	}
	if response.ContentLength > maxAssetBytes {
		return fmt.Errorf("fetch %s: body of %d bytes exceeds the %d byte asset cap", url, response.ContentLength, maxAssetBytes)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	// Read one byte past the cap so an over-long body with no
	// Content-Length is detected instead of silently truncated.
	written, err := io.Copy(file, io.LimitReader(&progressReader{reader: response.Body, url: url}, maxAssetBytes+1))
	if err != nil {
		klog.ErrorS(err, "asset download failed", "url", url, "bytes", written, "elapsed", time.Since(start).Round(time.Millisecond))
		return fmt.Errorf("download %s: %w", url, err)
	}
	if written > maxAssetBytes {
		return fmt.Errorf("download %s: body exceeds the %d byte asset cap", url, maxAssetBytes)
	}
	klog.InfoS("asset download finished", "url", url, "dest", path, "bytes", written, "duration", time.Since(start).Round(time.Millisecond))
	return nil
}

// httpClient returns the injected client or the timed-out default.
func (c AssetConfig) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultHTTPClient
}

// progressReader logs a heartbeat every downloadProgressInterval bytes.
type progressReader struct {
	reader  io.Reader
	url     string
	written int64
	marked  int64
}

func (p *progressReader) Read(buffer []byte) (int, error) {
	n, err := p.reader.Read(buffer)
	p.written += int64(n)
	if p.written-p.marked >= downloadProgressInterval {
		klog.InfoS("asset download in progress", "url", p.url, "bytes", p.written)
		p.marked = p.written
	}
	return n, err
}

func (c AssetConfig) dir() string {
	if c.Dir != "" {
		return c.Dir
	}
	return DefaultAssetsDir
}

func (c AssetConfig) version() string {
	if c.FCVersion != "" {
		return c.FCVersion
	}
	return DefaultFCVersion
}

func (c AssetConfig) base() string {
	if c.ReleaseBase != "" {
		return c.ReleaseBase
	}
	return releaseBaseURL
}
