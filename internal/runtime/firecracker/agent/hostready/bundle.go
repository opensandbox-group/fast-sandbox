package hostready

// bundle.go installs the Firecracker assets from the copy baked into the
// firecracker-runtime image (Dockerfile fc-assets stage). The bundle is an
// optimization, never a requirement: Ensure falls back to downloads when
// it is absent, incomplete, or pinned away by config.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// defaultBundleDir is the in-image location of the pinned assets.
const defaultBundleDir = "/usr/local/share/fast-sandbox/fc-assets"

// installFromBundle copies the needed subset of the image bundle into the
// assets dir, digest-verifying every file against SHA256SUMS. Custom pins
// gate files out silently (the download path serves them); a needed file
// the bundle cannot serve is an error, so the caller falls back.
func (c AssetConfig) installFromBundle(dir string) error {
	bundle := c.bundleDir()
	manifest, err := parseSHA256Sums(filepath.Join(bundle, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("prepare asset dir %s: %w", dir, err)
	}
	stockBinary := c.version() == DefaultFCVersion && c.ReleaseBase == ""
	entries := []struct {
		name    string
		allowed bool
	}{
		{name: assetFirecracker, allowed: stockBinary},
		{name: assetJailer, allowed: stockBinary},
	}
	var failures []error
	for _, entry := range entries {
		if !entry.allowed || !c.bundleNeeds(dir, entry.name) {
			continue
		}
		want, ok := manifest[entry.name]
		if !ok {
			failures = append(failures, fmt.Errorf("%s: not listed in SHA256SUMS", entry.name))
			continue
		}
		if err := copyVerified(filepath.Join(bundle, entry.name), filepath.Join(dir, entry.name), want); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", entry.name, err))
			continue
		}
		klog.InfoS("firecracker asset installed from the image bundle", "file", entry.name, "dir", dir)
	}
	return errors.Join(failures...)
}

// bundleNeeds reports whether dir/name still needs an install (same
// predicate as the download path, so a working install is never replaced).
func (c AssetConfig) bundleNeeds(dir, name string) bool {
	return c.binaryMissing(filepath.Join(dir, name))
}

// copyVerified copies src to dst after verifying its digest against want
// (temp file + rename, 0o755 throughout: every bundle file is executed).
func copyVerified(src, dst, want string) error {
	payload, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	tmp := dst + ".download"
	//nolint:gosec // the staged bundle binaries are executed (firecracker/jailer); they need 0o755
	if err := os.WriteFile(tmp, payload, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// parseSHA256Sums reads a sha256sum-format manifest into name -> digest.
func parseSHA256Sums(path string) (map[string]string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	manifest := make(map[string]string)
	for number, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != hex.EncodedLen(sha256.Size) {
			return nil, fmt.Errorf("SHA256SUMS line %d is malformed: %q", number+1, line)
		}
		manifest[strings.TrimPrefix(fields[1], "./")] = fields[0]
	}
	if len(manifest) == 0 {
		return nil, errors.New("SHA256SUMS lists no entries")
	}
	return manifest, nil
}

func (c AssetConfig) bundleDir() string {
	if c.BundleDir != "" {
		return c.BundleDir
	}
	return defaultBundleDir
}
