package hostready

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/klog/v2"
)

// Firecracker asset pins (the release the runtime plan and the kernel the
// guest expect; must match the removed config/runtime-installers pins).
const (
	// DefaultFCVersion is the pinned Firecracker release.
	DefaultFCVersion = "v1.16.1"
	// kernelURLTemplate is the Amazon microvm CI kernel (6.1, ACPI +
	// VMGenID; the quickstart 4.14 kernel's CRNG is not reseeded at
	// snapshot resume, execd /command hangs, #1695) with the %s arch
	// segment resolved from the node (x86_64 today; aarch64 when arm64
	// support lands).
	kernelURLTemplate = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260722-38359b8055fc-0/%s/vmlinux-6.1.176"
	// DefaultAssetsDir is the on-node install target (matches
	// config/runtime-environments.yaml binaryPath/kernelPath).
	DefaultAssetsDir = "/opt/fast-sandbox/firecracker"
	// releaseBaseURL is the Firecracker GitHub release root.
	releaseBaseURL = "https://github.com/firecracker-microvm/firecracker/releases"
	// maxAssetBytes bounds one downloaded asset (kernel ~ tens of MiB).
	maxAssetBytes = 1 << 30
)

// AssetConfig describes the Firecracker asset installation.
type AssetConfig struct {
	// Dir is the install target (default DefaultAssetsDir).
	Dir string
	// FCVersion is the pinned Firecracker release (default v1.16.1).
	FCVersion string
	// KernelURL is the guest kernel blob URL; empty derives the pinned
	// per-arch Amazon CI kernel (an explicit URL is used verbatim for
	// both architectures).
	KernelURL string
	// ReleaseBase overrides the GitHub release root (tests point it at an
	// httptest server).
	ReleaseBase string
	// HTTPClient fetches the assets (nil = default client).
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

// Ensure materializes {Dir}/{firecracker,jailer,vmlinux.bin}, downloading
// whatever is missing and verifying the result. It is idempotent: a fully
// installed and verifiable directory short-circuits without network.
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
	binary := filepath.Join(dir, "firecracker")
	jailer := filepath.Join(dir, "jailer")
	if c.binaryMissing(binary) || c.binaryMissing(jailer) {
		version := c.version()
		url := fmt.Sprintf("%s/download/%s/firecracker-%s-%s.tgz", c.base(), version, version, arch)
		if err := c.installTarball(ctx, url, dir, arch); err != nil {
			return err
		}
	}
	kernel := filepath.Join(dir, "vmlinux.bin")
	if info, err := os.Stat(kernel); err != nil || info.Size() == 0 {
		kernelURL, err := c.kernelURL()
		if err != nil {
			return err
		}
		if err := c.installFile(ctx, kernelURL, kernel); err != nil {
			return err
		}
	}
	if err := c.verify(); err != nil {
		return fmt.Errorf("verify installed firecracker assets: %w", err)
	}
	klog.InfoS("firecracker assets installed", "dir", dir, "version", c.version())
	return nil
}

// verify executes both binaries and checks the kernel blob exists.
func (c AssetConfig) verify() error {
	dir := c.dir()
	verify := c.VerifyBinary
	if verify == nil {
		verify = verifyBinary
	}
	for _, name := range []string{"firecracker", "jailer"} {
		if err := verify(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "vmlinux.bin"))
	if err != nil {
		return fmt.Errorf("vmlinux.bin: %w", err)
	}
	if info.Size() == 0 {
		return errors.New("vmlinux.bin is empty")
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
		"firecracker": filepath.Join(dir, "firecracker"),
		"jailer":      filepath.Join(dir, "jailer"),
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
		if err := os.WriteFile(filepath.Join(dest, base), payload, 0o755); err != nil {
			return err
		}
	}
}

// installFile downloads url to path (streaming, bounded).
func (c AssetConfig) installFile(ctx context.Context, url, path string) error {
	tmp := path + ".download"
	if err := c.download(ctx, url, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// download streams url into path, rejecting bodies at or over the size
// cap (a silent truncation would hand the node a corrupt kernel that
// still passes the non-empty verify).
func (c AssetConfig) download(ctx context.Context, url, path string) error {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
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
	written, err := io.Copy(file, io.LimitReader(response.Body, maxAssetBytes+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if written > maxAssetBytes {
		return fmt.Errorf("download %s: body exceeds the %d byte asset cap", url, maxAssetBytes)
	}
	return nil
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

// kernelURL resolves the guest kernel blob URL: an explicit URL is used
// verbatim; the empty default derives the pinned per-arch Amazon CI
// kernel (an arm64 node must never receive the x86_64 blob).
func (c AssetConfig) kernelURL() (string, error) {
	if c.KernelURL != "" {
		return c.KernelURL, nil
	}
	arch, err := assetArch()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(kernelURLTemplate, arch), nil
}

func (c AssetConfig) base() string {
	if c.ReleaseBase != "" {
		return c.ReleaseBase
	}
	return releaseBaseURL
}
