package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"fast-sandbox/internal/firecrackerconfig"
)

const pinnedKernelsDir = "pinned-kernels"

type fileStatKey struct {
	path      string
	dev       uint64
	ino       uint64
	size      int64
	mtimeNano int64
}

type cachedKernel struct {
	key    fileStatKey
	path   string
	sha256 string
}

func fileKeyFromInfo(path string, info os.FileInfo) (fileStatKey, error) {
	if !info.Mode().IsRegular() {
		return fileStatKey{}, fmt.Errorf("kernel %s is not a regular file", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStatKey{}, fmt.Errorf("stat identity unavailable for %s", path)
	}
	return fileStatKey{
		path:      path,
		dev:       uint64(st.Dev),
		ino:       uint64(st.Ino),
		size:      info.Size(),
		mtimeNano: info.ModTime().UnixNano(),
	}, nil
}

func statRegularFileKey(path string) (fileStatKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStatKey{}, err
	}
	return fileKeyFromInfo(path, info)
}

// pinBootKernel binds source into target as an immutable private inode, caching
// verified node kernels under stateRoot by file identity and hard-linking on hits.
func (d *Driver) pinBootKernel(stateRoot, source, target string) (string, error) {
	if stateRoot == "" {
		return firecrackerconfig.PinKernel(source, target)
	}
	key, err := statRegularFileKey(source)
	if err != nil {
		return firecrackerconfig.PinKernel(source, target)
	}

	d.mu.RLock()
	entry, ok := d.kernelCache[source]
	d.mu.RUnlock()
	if ok && entry.key == key {
		if err := os.Link(entry.path, target); err == nil {
			return entry.sha256, nil
		} else if errors.Is(err, os.ErrExist) {
			return "", err
		}
	}

	cacheDir := filepath.Join(stateRoot, pinnedKernelsDir)
	cachedPath, sum, pinnedKey, err := pinKernelIntoCache(cacheDir, source)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	if d.kernelCache == nil {
		d.kernelCache = make(map[string]cachedKernel)
	}
	d.kernelCache[source] = cachedKernel{key: pinnedKey, path: cachedPath, sha256: sum}
	d.mu.Unlock()

	if err := os.Link(cachedPath, target); err == nil {
		return sum, nil
	} else if errors.Is(err, os.ErrExist) {
		return "", err
	}
	return firecrackerconfig.PinKernel(source, target)
}

func pinKernelIntoCache(cacheDir, source string) (cachedPath, sum string, key fileStatKey, resultErr error) {
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return "", "", fileStatKey{}, err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", "", fileStatKey{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", "", fileStatKey{}, err
	}
	key, err = fileKeyFromInfo(source, info)
	if err != nil {
		return "", "", fileStatKey{}, err
	}
	temp, err := os.CreateTemp(cacheDir, ".kernel-*.tmp")
	if err != nil {
		return "", "", fileStatKey{}, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, hash), input); err != nil {
		_ = temp.Close()
		return "", "", fileStatKey{}, err
	}
	if err := temp.Chmod(0o444); err != nil {
		_ = temp.Close()
		return "", "", fileStatKey{}, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", "", fileStatKey{}, err
	}
	if err := temp.Close(); err != nil {
		return "", "", fileStatKey{}, err
	}
	sum = hex.EncodeToString(hash.Sum(nil))
	final := filepath.Join(cacheDir, sum)
	if err := os.Link(tempName, final); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", "", fileStatKey{}, err
		}
		ok, verifyErr := verifyCachedKernel(final, sum)
		if verifyErr != nil || !ok {
			if err := os.Rename(tempName, final); err != nil {
				return "", "", fileStatKey{}, err
			}
		}
	}
	return final, sum, key, nil
}

func verifyCachedKernel(path, wantSHA256 string) (bool, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !linfo.Mode().IsRegular() {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	finfo, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA256, nil
}
