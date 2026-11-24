// Package firecrackervmm describes managed, versioned Firecracker installations.
package firecrackervmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "embed"
)

const DefaultDir = "/opt/fast-sandbox/firecracker"

//go:embed releases.json
var Releases []byte

// Catalog is published only after every advertised version is installed.
type Catalog struct {
	SchemaVersion int      `json:"schemaVersion"`
	LegacyVersion string   `json:"legacyVersion"`
	Versions      []string `json:"versions"`
}

func ParseCatalog(raw []byte) (Catalog, error) {
	var c Catalog
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("unexpected trailing catalog data")
	}
	if c.SchemaVersion != 1 || len(c.Versions) == 0 {
		return c, fmt.Errorf("invalid Firecracker catalog schema or empty versions")
	}
	seen := map[string]bool{}
	for _, v := range c.Versions {
		if _, err := versionParts(v); err != nil {
			return c, err
		}
		if seen[v] {
			return c, fmt.Errorf("duplicate Firecracker version %q", v)
		}
		seen[v] = true
	}
	if !seen[c.LegacyVersion] {
		return c, fmt.Errorf("legacy version %q is not in the catalog", c.LegacyVersion)
	}
	return c, nil
}

// Accept canonical stable versions only, never paths or prerelease selectors.
func versionParts(v string) ([3]uint64, error) {
	var parts [3]uint64
	fields := strings.Split(v, ".")
	if len(fields) != len(parts) {
		return parts, fmt.Errorf("invalid Firecracker version %q", v)
	}
	for i, field := range fields {
		n, err := strconv.ParseUint(field, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != field {
			return parts, fmt.Errorf("invalid Firecracker version %q", v)
		}
		parts[i] = n
	}
	return parts, nil
}

type binaryVersionKey struct {
	path      string
	dev       uint64
	ino       uint64
	size      int64
	mtimeNano int64
}

var (
	binaryVersionMu    sync.RWMutex
	binaryVersionCache = map[binaryVersionKey]string{}
)

func statBinaryKey(path string) (binaryVersionKey, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return binaryVersionKey{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return binaryVersionKey{}, false
	}
	return binaryVersionKey{
		path:      path,
		dev:       uint64(st.Dev),
		ino:       uint64(st.Ino),
		size:      info.Size(),
		mtimeNano: info.ModTime().UnixNano(),
	}, true
}

// BinaryVersion probes a selected executable with a bounded deadline.
// Verified versions are cached by executable file identity (path/dev/ino/size/mtime)
// so repeated VMM selections avoid redundant subprocess probes while immediately
// detecting replaced or modified binaries.
func BinaryVersion(ctx context.Context, path string) (string, error) {
	key, hasKey := statBinaryKey(path)
	if hasKey {
		binaryVersionMu.RLock()
		cached, ok := binaryVersionCache[key]
		binaryVersionMu.RUnlock()
		if ok {
			return cached, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("probe %s: %w", path, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || (fields[0] != "Firecracker" && fields[0] != "Jailer") {
		return "", fmt.Errorf("unexpected VMM version output from %s", path)
	}
	v := strings.TrimPrefix(fields[1], "v")
	if _, err := versionParts(v); err != nil {
		return "", err
	}
	if hasKey {
		binaryVersionMu.Lock()
		binaryVersionCache[key] = v
		binaryVersionMu.Unlock()
	}
	return v, nil
}
