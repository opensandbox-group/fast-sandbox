package firecrackervmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ErrVersionUnavailable means the artifact requests a release outside the installed inventory.
var ErrVersionUnavailable = errors.New("Firecracker version unavailable")

// Selection is immutable for the lifetime of one VM.
type Selection struct {
	Version    string `json:"version"`
	BinaryPath string `json:"binaryPath"`
	JailerPath string `json:"jailerPath,omitempty"`
}

// IsManaged reports whether the selection uses the managed release layout.
// It does not replace executable identity or installed-release validation.
func (s *Selection) IsManaged(root string) bool {
	if s == nil || !filepath.IsAbs(root) {
		return false
	}
	if _, err := versionParts(s.Version); err != nil {
		return false
	}
	return s.BinaryPath == filepath.Join(releaseDir(root, s.Version), "firecracker")
}

func releaseDir(root, version string) string {
	return filepath.Join(root, "versions", version)
}

// Resolve uses an exact recorded version; only a new build may request latest
// with an empty version. The inventory is controlled by the FSB release image.
func Resolve(ctx context.Context, root, version string, jailed bool) (Selection, error) {
	var result Selection
	if !filepath.IsAbs(root) {
		return result, fmt.Errorf("VMM directory must be absolute")
	}
	raw, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		return result, err
	}
	catalog, err := ParseCatalog(raw)
	if err != nil {
		return result, err
	}
	if version == "" {
		version = catalog.Versions[0]
		for _, v := range catalog.Versions[1:] {
			a, _ := versionParts(v)
			b, _ := versionParts(version)
			if slices.Compare(a[:], b[:]) > 0 {
				version = v
			}
		}
	} else if !slices.Contains(catalog.Versions, version) {
		return result, fmt.Errorf("%w: %q; installed versions: %v", ErrVersionUnavailable, version, catalog.Versions)
	}
	dir := releaseDir(root, version)
	for _, path := range []string{filepath.Join(root, "versions"), dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return result, err
		}
		if !info.IsDir() {
			return result, fmt.Errorf("invalid managed VMM directory %s", path)
		}
	}
	result = Selection{Version: version, BinaryPath: filepath.Join(dir, "firecracker")}
	// A managed release is always installed as a complete pair, even in direct mode.
	for _, name := range []string{"firecracker", "jailer"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return Selection{}, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return Selection{}, fmt.Errorf("invalid managed executable %s", path)
		}
		actual, err := BinaryVersion(ctx, path)
		if err != nil {
			return Selection{}, err
		}
		if actual != version {
			return Selection{}, fmt.Errorf("%s reports %s, expected %s", path, actual, version)
		}
	}
	if jailed {
		result.JailerPath = filepath.Join(dir, "jailer")
	}
	return result, nil
}
