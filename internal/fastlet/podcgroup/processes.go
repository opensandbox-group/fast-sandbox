package podcgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// VerifySandboxProcessesAbsent checks the kernel hierarchy independently of
// containerd and its shim. standalonePath is the OCI cgroup outside Kubernetes.
func VerifySandboxProcessesAbsent(root, podUID, sandboxID, standalonePath string) error {
	_, err := os.Stat(filepath.Join(root, "cgroup.controllers"))
	unified := err == nil
	if !unified {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		root = filepath.Join(root, "pids")
		if _, err := os.Stat(filepath.Join(root, "cgroup.procs")); err != nil {
			return fmt.Errorf("verify host pids hierarchy: %w", err)
		}
	} else if _, err := os.Stat(filepath.Join(root, "cgroup.procs")); err != nil {
		return fmt.Errorf("verify host unified hierarchy: %w", err)
	}

	if podUID == "" {
		return verifyCgroupEmpty(cgroupFilesystemPath(root, standalonePath), unified)
	}
	if err := validatePodUID(podUID); err != nil {
		return err
	}
	podPath, err := findPodCgroup(root, podUID, true)
	if errors.Is(err, errPodCgroupAbsent) {
		return verifyLiteralCgroupEmpty(root, podUID, sandboxID, unified)
	}
	if err != nil {
		return err
	}
	layout := Layout{PodPath: podPath}
	path, err := layout.SandboxPath(sandboxID)
	if err != nil {
		return err
	}
	if err := verifyCgroupEmpty(cgroupFilesystemPath(root, path), unified); err != nil {
		return err
	}
	if slice := filepath.Base(podPath); strings.HasSuffix(slice, ".slice") {
		// runc expands slice:prefix:name into <slice>/<prefix>-<name>.scope.
		scope := "fast-sandbox-" + filepath.Base(path) + ".scope"
		if err := verifyCgroupEmpty(cgroupFilesystemPath(root, filepath.Join(podPath, scope)), unified); err != nil {
			return err
		}
		// runc's filesystem driver can also take the colon-form string
		// literally, as a relative path under its parent cgroup.
		return verifyLiteralCgroupEmpty(root, podUID, sandboxID, unified)
	}
	return nil
}

func verifyLiteralCgroupEmpty(root, podUID, sandboxID string, unified bool) error {
	suffix := ":fast-sandbox:" + sandboxCgroupName(sandboxID)
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slice, found := strings.CutSuffix(entry.Name(), suffix)
		if entry.IsDir() && found && matchesPodDirectory(slice, podUID) {
			if err := verifyCgroupEmpty(path, unified); err != nil {
				return err
			}
			return fs.SkipDir
		}
		return nil
	})
}

func verifyCgroupEmpty(path string, unified bool) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if unified {
		data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return err
		}
		if strings.Contains("\n"+string(data)+"\n", "\npopulated 0\n") {
			return nil
		}
		return fmt.Errorf("cgroup %q is populated or its population is unknown", path)
	}
	return filepath.WalkDir(path, func(child string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(filepath.Join(child, "cgroup.procs"))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) != "" {
			return fmt.Errorf("cgroup %q still contains processes", child)
		}
		return nil
	})
}
