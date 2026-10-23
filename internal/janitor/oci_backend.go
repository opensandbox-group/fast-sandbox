package janitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// OCIBackend reuses fsb-vm teardown for resources left by a vanished Fastlet.
// Pod-scoped paths prevent an old cleanup from releasing a replacement's devices.
type OCIBackend struct {
	stateRoot string
	run       func(context.Context, ...string) error
}

func NewOCIBackend(stateRoot string) *OCIBackend {
	return &OCIBackend{stateRoot: stateRoot, run: func(ctx context.Context, args ...string) error {
		command := exec.CommandContext(ctx, filepath.Join(runtimecatalog.OCIToolsDir, "current/bin/fsb-vm"), args...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("fsb-vm cleanup: %w: %s", err, output)
		}
		return nil
	}}
}

func (*OCIBackend) Name() ResourceBackend { return BackendOCI }

func (b *OCIBackend) Scan(_ context.Context) ([]ResourceIdentity, error) {
	var result []ResourceIdentity
	homes, err := filepath.Glob(filepath.Join(b.stateRoot, "oci/pods/*/sandboxes/*"))
	if err != nil {
		return nil, err
	}
	for _, home := range homes {
		info, err := os.Lstat(home)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || strings.HasPrefix(filepath.Base(home), ".") {
			continue
		}
		pod := filepath.Base(filepath.Dir(filepath.Dir(home)))
		result = append(result, ResourceIdentity{Backend: BackendOCI, ResourceID: "sandbox/" + pod + "/" + filepath.Base(home), FastletPodUID: pod, CreatedAt: info.ModTime()})
	}
	caches, err := filepath.Glob(filepath.Join(b.stateRoot, "oci/pods/*/sandboxes/.snapfiles"))
	if err != nil {
		return nil, err
	}
	for _, path := range caches {
		root := filepath.Dir(filepath.Dir(path))
		pod := filepath.Base(root)
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		result = append(result, ResourceIdentity{Backend: BackendOCI, ResourceID: "snapfiles/" + pod, FastletPodUID: pod, CreatedAt: info.ModTime()})
	}
	owners, err := filepath.Glob(filepath.Join(b.stateRoot, "snapshots/*/*/.pack-owner.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range owners {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var owner fastletapi.SandboxIdentity
		if err := json.Unmarshal(raw, &owner); err != nil {
			return nil, err
		}
		if owner.FastletPodUID == "" || owner.FastletPodUID != filepath.Base(filepath.Dir(filepath.Dir(path))) {
			return nil, errors.New("pack ownership does not match its Pod directory")
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		result = append(result, ResourceIdentity{Backend: BackendOCI, ResourceID: "pack/" + owner.FastletPodUID + "/" + filepath.Base(filepath.Dir(path)), FastletPodUID: owner.FastletPodUID, CreatedAt: info.ModTime()})
	}
	return result, nil
}

func (b *OCIBackend) Cleanup(ctx context.Context, expected ResourceIdentity) error {
	// Recheck the on-disk owner after Janitor revalidates Pod absence.
	resources, err := b.Scan(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, current := range resources {
		if current.ResourceID == expected.ResourceID {
			if !sameResourceFence(expected, current) {
				return errors.New("OCI resource ownership changed before cleanup")
			}
			found = true
		}
	}
	if !found {
		return nil
	}
	parts := strings.Split(expected.ResourceID, "/")
	socket := filepath.Join(runtimecatalog.OCISocketDir, "ublkd.sock")
	switch {
	case len(parts) == 3 && parts[0] == "sandbox":
		root := filepath.Join(b.stateRoot, "oci/pods", parts[1])
		if err := b.run(ctx, "--ublkd-socket", socket, "--state-dir", filepath.Join(root, "sandboxes"), "--runtime-dir", filepath.Join(root, "runtime"), "delete", parts[2]); err != nil {
			return err
		}
		// Remove empty Pod directories without touching retained/unknown operations.
		_ = os.Remove(filepath.Join(root, "sandboxes"))
		_ = os.Remove(filepath.Join(root, "runtime"))
	case len(parts) == 2 && parts[0] == "snapfiles":
		root := filepath.Join(b.stateRoot, "oci/pods", parts[1])
		if err := b.run(ctx, "--ublkd-socket", socket, "--state-dir", filepath.Join(root, "sandboxes"), "cleanup-snapfiles"); err != nil {
			return err
		}
		cache := filepath.Join(root, "sandboxes/.snapfiles")
		entries, err := os.ReadDir(cache)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() != ".locks" {
				return fmt.Errorf("shared snapfiles cleanup retained %s", filepath.Join(cache, entry.Name()))
			}
		}
		// Janitor has confirmed this Pod is gone; no live CLI can hold these locks.
		if err := os.RemoveAll(cache); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(root, "sandboxes"))
		_ = os.Remove(filepath.Join(root, "runtime"))
		_ = os.Remove(root)
	case len(parts) == 3 && parts[0] == "pack":
		staging := filepath.Join(b.stateRoot, "snapshots", parts[1], parts[2])
		if err := b.run(ctx, "--ublkd-socket", socket, "--build-work-dir", filepath.Join(staging, "pack-work"), "cleanup-pack", "--bundle-dir", staging); err != nil {
			return err
		}
		return os.RemoveAll(staging)
	default:
		return errors.New("invalid OCI cleanup identity")
	}
	return nil
}
