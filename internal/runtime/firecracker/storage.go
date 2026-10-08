package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// checkStateRootStorage rejects deployments that would pay a full rootfs copy
// on every create. Check the Fastlet's own mount and cp, not just node labels.
func checkStateRootStorage(ctx context.Context, root string) error {
	fsType, err := stateRootFilesystemType(root)
	if err != nil {
		return fmt.Errorf("statfs %q: %w", root, err)
	}
	return probeStateRootStorage(ctx, root, fsType, func(ctx context.Context, source, target string) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "cp", "--reflink=always", source, target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("cp --reflink=always: %w (%s)", err, strings.TrimSpace(string(output)))
		}
		return nil
	})
}

func probeStateRootStorage(ctx context.Context, root, fsType string, clone func(context.Context, string, string) error) error {
	if fsType != "xfs" {
		return fmt.Errorf("StateRoot %q must use XFS with reflink=1; found %s; mount an XFS filesystem before starting Fastlet", root, fsType)
	}
	dir, err := os.MkdirTemp(root, ".startup-reflink-")
	if err != nil {
		return fmt.Errorf("create reflink probe in %q: %w", root, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "clone")
	if err := os.WriteFile(source, []byte("fast-sandbox startup reflink probe\n"), 0o600); err != nil {
		return fmt.Errorf("write reflink probe: %w", err)
	}
	if err := clone(ctx, source, target); err != nil {
		return fmt.Errorf("StateRoot %q must support reflink cloning; enable XFS reflink=1 and install GNU cp (coreutils): %w", root, err)
	}
	return nil
}
