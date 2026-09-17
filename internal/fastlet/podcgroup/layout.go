// Package podcgroup discovers the Kubernetes-owned cgroup of the current
// Fastlet Pod and derives child cgroup identities for Sandbox runtimes.
package podcgroup

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// HostRoot is the node cgroup hierarchy mounted into the Fastlet Pod.
	// It is platform-owned and intentionally not part of Runtime Environment.
	HostRoot = "/host/sys/fs/cgroup"

	HostPath   = "/sys/fs/cgroup"
	VolumeName = "host-cgroup"
	shimGroup  = "fast-sandbox-shims"
)

type Version string

const (
	VersionV1 Version = "v1"
	VersionV2 Version = "v2"
)

// Layout describes the current Fastlet Pod cgroup as the host runtime sees it.
// PodPath is an absolute cgroup path, not a filesystem path below HostRoot.
type Layout struct {
	Version Version
	PodPath string
	Systemd bool
}

// Discover locates the current Pod cgroup by UID. It supports the standard
// cgroupfs and systemd layouts on cgroup v1 and v2. This remains correct when
// the Pod has a private cgroup namespace and /proc/self/cgroup only reports /.
func Discover(root, podUID string) (Layout, error) {
	if root == "" {
		return Layout{}, errors.New("cgroup root is required")
	}
	if err := validatePodUID(podUID); err != nil {
		return Layout{}, err
	}
	if _, err := os.Stat(root); err != nil {
		return Layout{}, fmt.Errorf("stat host cgroup root %q: %w", root, err)
	}

	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err == nil {
		path, err := discoverInHierarchy(root, podUID)
		if err != nil {
			return Layout{}, err
		}
		return newLayout(VersionV2, path), nil
	}

	controllerRoots, err := v1ControllerRoots(root)
	if err != nil {
		return Layout{}, err
	}
	var found []string
	for _, controllerRoot := range controllerRoots {
		path, err := discoverInHierarchy(controllerRoot, podUID)
		if err == nil {
			found = append(found, path)
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Layout{}, err
		}
	}
	if len(found) == 0 {
		return Layout{}, fmt.Errorf("locate Pod UID %q below cgroup v1 root %q: %w", podUID, root, fs.ErrNotExist)
	}
	sort.Strings(found)
	for _, path := range found[1:] {
		if path != found[0] {
			return Layout{}, fmt.Errorf("pod UID %q has inconsistent cgroup v1 paths %q and %q", podUID, found[0], path)
		}
	}
	return newLayout(VersionV1, found[0]), nil
}

// SandboxPath returns a filesystem-style OCI cgroupsPath below the Fastlet
// Pod. This form is understood by shims such as Kata that do not interpret
// runc's systemd slice:prefix:name syntax.
func (l Layout) SandboxPath(sandboxID string) (string, error) {
	if l.PodPath == "" {
		return "", errors.New("Pod cgroup path is empty")
	}
	if sandboxID == "" {
		return "", errors.New("sandbox ID is required")
	}
	name := sandboxCgroupName(sandboxID)
	return filepath.ToSlash(filepath.Join(l.PodPath, "fast-sandbox", name)), nil
}

// SandboxSystemdPath returns runc's slice:prefix:name representation. It is
// only valid for shims that explicitly implement systemd cgroup semantics;
// other shims may interpret the colons as an ordinary directory name.
func (l Layout) SandboxSystemdPath(sandboxID string) (string, error) {
	if !l.Systemd {
		return l.SandboxPath(sandboxID)
	}
	if l.PodPath == "" {
		return "", errors.New("Pod cgroup path is empty")
	}
	if sandboxID == "" {
		return "", errors.New("sandbox ID is required")
	}
	base := filepath.Base(l.PodPath)
	if !strings.HasSuffix(base, ".slice") {
		return "", fmt.Errorf("systemd Pod cgroup %q is not a slice", l.PodPath)
	}
	return base + ":fast-sandbox:" + sandboxCgroupName(sandboxID), nil
}

// RemoveSandboxGroups removes empty filesystem-style Sandbox cgroups after a
// shim has deleted the task. Kata prefixes its leaf with "kata_" while other
// shims use the OCI leaf name directly, so both deterministic names are
// removed. A populated cgroup is returned as an error instead of hidden.
func (l Layout) RemoveSandboxGroups(root, sandboxID string) error {
	path, err := l.SandboxPath(sandboxID)
	if err != nil {
		return err
	}
	leaf := filepath.Base(path)
	parent := filepath.Dir(path)

	roots := []string{root}
	if l.Version == VersionV1 {
		roots, err = v1ControllerRoots(root)
		if err != nil {
			return err
		}
	}

	for _, controllerRoot := range roots {
		for _, name := range []string{leaf, "kata_" + leaf} {
			candidate := cgroupFilesystemPath(controllerRoot, filepath.Join(parent, name))
			if err := os.Remove(candidate); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove Sandbox cgroup %q: %w", candidate, err)
			}
		}
	}
	return nil
}

// EnsureShimGroup creates a dedicated leaf below the Pod cgroup. A cgroup v2
// Pod parent with controllers enabled cannot itself contain shim processes.
func (l Layout) EnsureShimGroup(root string) error {
	if l.PodPath == "" {
		return errors.New("Pod cgroup path is empty")
	}
	if l.Version == VersionV2 {
		if err := os.MkdirAll(cgroupFilesystemPath(root, l.ShimPath()), 0o755); err != nil {
			return fmt.Errorf("create shim cgroup: %w", err)
		}
		return nil
	}
	roots, err := v1ControllerRoots(root)
	if err != nil {
		return err
	}
	for _, controllerRoot := range roots {
		podPath := cgroupFilesystemPath(controllerRoot, l.PodPath)
		if info, statErr := os.Stat(podPath); statErr != nil || !info.IsDir() {
			continue
		}
		if err := os.MkdirAll(filepath.Join(podPath, shimGroup), 0o755); err != nil {
			return fmt.Errorf("create shim cgroup in %q: %w", controllerRoot, err)
		}
	}
	return nil
}

// ShimPath returns the existing dedicated leaf suitable for containerd's
// runc shim-cgroup option. Sandbox workload processes remain in SandboxPath.
func (l Layout) ShimPath() string {
	return filepath.ToSlash(filepath.Join(l.PodPath, shimGroup))
}

func newLayout(version Version, path string) Layout {
	return Layout{Version: version, PodPath: path, Systemd: strings.HasSuffix(filepath.Base(path), ".slice")}
}

func validatePodUID(uid string) error {
	if uid == "" {
		return errors.New("Pod UID is required")
	}
	for _, char := range uid {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return fmt.Errorf("pod UID %q contains an invalid character", uid)
	}
	return nil
}

func discoverInHierarchy(root, podUID string) (string, error) {
	for _, candidate := range standardPodPaths(podUID) {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(candidate, "/"))))
		if err == nil && info.IsDir() {
			return candidate, nil
		}
	}

	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrPermission) {
				return fs.SkipDir
			}
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if pathDepth(relative) > 8 {
			return fs.SkipDir
		}
		if matchesPodDirectory(entry.Name(), podUID) {
			matches = append(matches, "/"+filepath.ToSlash(relative))
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("scan cgroup hierarchy %q: %w", root, err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("locate Pod UID %q below %q: %w", podUID, root, fs.ErrNotExist)
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		return "", fmt.Errorf("pod UID %q matches multiple cgroups below %q: %v", podUID, root, matches)
	}
	return matches[0], nil
}

func v1ControllerRoots(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read cgroup v1 root %q: %w", root, err)
	}
	var preferred, remaining []string
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.Contains(name, "cpu") || name == "memory" || name == "pids":
			preferred = append(preferred, path)
		default:
			remaining = append(remaining, path)
		}
	}
	sort.Strings(preferred)
	sort.Strings(remaining)
	roots := append(preferred, remaining...)
	if len(roots) == 0 {
		return nil, fmt.Errorf("cgroup v1 root %q has no controller hierarchies", root)
	}
	return roots, nil
}

func standardPodPaths(uid string) []string {
	systemdUID := strings.ReplaceAll(uid, "-", "_")
	return []string{
		"/kubepods/pod" + uid,
		"/kubepods/burstable/pod" + uid,
		"/kubepods/besteffort/pod" + uid,
		"/kubepods.slice/kubepods-pod" + systemdUID + ".slice",
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + systemdUID + ".slice",
		"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + systemdUID + ".slice",
	}
}

func podDirectoryNames(uid string) map[string]struct{} {
	names := make(map[string]struct{}, 6)
	for _, path := range standardPodPaths(uid) {
		names[filepath.Base(path)] = struct{}{}
	}
	return names
}

func matchesPodDirectory(name, uid string) bool {
	if _, ok := podDirectoryNames(uid)[name]; ok {
		return true
	}
	systemdUID := strings.ReplaceAll(uid, "-", "_")
	// Some distributions prefix the kubelet slice hierarchy (for example
	// kubelet-kubepods-...). The Pod UID suffix remains the stable identity.
	return strings.HasSuffix(name, "-pod"+systemdUID+".slice")
}

func sandboxCgroupName(id string) string {
	digest := sha256.Sum256([]byte(id))
	return fmt.Sprintf("fsb-%x", digest[:8])
}

func pathDepth(path string) int {
	depth := 0
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part != "" && part != "." {
			depth++
		}
	}
	return depth
}

func cgroupFilesystemPath(root, cgroupPath string) string {
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(cgroupPath, "/")))
}
