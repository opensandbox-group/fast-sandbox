package hostready

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"fast-sandbox/internal/firecrackervmm"
)

// Install complete immutable release pairs before publishing their catalog.
// Legacy flat installs remain untouched; a bad managed bundle never falls
// back to an unrelated network download or blesses an existing binary.
func (c AssetConfig) installVersionedBundle(ctx context.Context) error {
	if c.ReleaseBase != "" || c.version() != DefaultFCVersion {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(c.bundleDir(), "catalog.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} // Legacy image or custom deployment.
	if err != nil {
		return err
	}
	catalog, err := firecrackervmm.ParseCatalog(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir(), 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.dir(), ".install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	versions := filepath.Join(c.dir(), "versions")
	if err := os.MkdirAll(versions, 0755); err != nil {
		return err
	}
	if err := requireAssetDirectory(versions); err != nil {
		return err
	}
	for _, version := range catalog.Versions {
		if err := c.installVersion(ctx, version, versions); err != nil {
			return fmt.Errorf("install Firecracker %s: %w", version, err)
		}
	}
	existing, err := os.ReadFile(filepath.Join(c.dir(), "catalog.json"))
	if err == nil && bytes.Equal(existing, raw) {
		return nil
	}
	tmp, err := os.CreateTemp(c.dir(), ".catalog-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(raw); err != nil {
		return err
	}
	if err = tmp.Chmod(0644); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(c.dir(), "catalog.json")); err != nil {
		return err
	}
	return syncAssetDirectory(c.dir())
}

func (c AssetConfig) installVersion(ctx context.Context, version, versions string) error {
	source := filepath.Join(c.bundleDir(), "versions", version)
	if err := requireAssetDirectory(source); err != nil {
		return err
	}
	sums, err := parseSHA256Sums(filepath.Join(source, "SHA256SUMS"))
	if err != nil {
		return err
	}
	destination := filepath.Join(versions, version)
	if _, err := os.Lstat(destination); err == nil {
		if err := requireAssetDirectory(destination); err != nil {
			return err
		}
		return verifyVersionPair(ctx, destination, version, sums)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(versions, ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, name := range []string{assetFirecracker, assetJailer} {
		if err := copyVerified(filepath.Join(source, name), filepath.Join(stage, name), sums[name]); err != nil {
			return err
		}
		file, err := os.OpenFile(filepath.Join(stage, name), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	if err := verifyVersionPair(ctx, stage, version, sums); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(source, "SHA256SUMS"))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stage, "SHA256SUMS"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	// The directory itself is the publication unit. There is no visible half-pair.
	if err := os.Chmod(stage, 0755); err != nil {
		return err
	}
	if err := syncAssetDirectory(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return syncAssetDirectory(versions)
}

func requireAssetDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("asset directory %q is not a real directory", path)
	}
	return nil
}

func syncAssetDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func verifyVersionPair(ctx context.Context, dir, version string, sums map[string]string) error {
	for _, name := range []string{assetFirecracker, assetJailer} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("invalid executable %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != sums[name] {
			return fmt.Errorf("immutable version %s: %s checksum mismatch", version, name)
		}
		actual, err := firecrackervmm.BinaryVersion(ctx, path)
		if err != nil {
			return err
		}
		if actual != version {
			return fmt.Errorf("%s reports %s, expected %s", name, actual, version)
		}
	}
	return nil
}
