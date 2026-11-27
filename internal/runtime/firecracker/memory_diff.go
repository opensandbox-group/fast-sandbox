package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fast-sandbox/internal/artifacts"
)

const memoryDiffBaseName = "memory-diff-base"

func initializeMemoryDiffBase(directory, memory string) error {
	base := filepath.Join(directory, memoryDiffBaseName)
	if err := os.Remove(base); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Link(memory, base)
}

// takeMemoryDiffBase invalidates the old baseline before Firecracker can
// reset its dirty log. Failure or interruption then forces a Full next time.
func takeMemoryDiffBase(directory, staging string) (string, error) {
	base := filepath.Join(directory, memoryDiffBaseName)
	basePath := filepath.Join(staging, ".memory-diff-parent")
	if err := os.Rename(base, basePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return basePath, nil
}

func prepareMemoryCapture(plan *dumpPlan, trackDirtyPages bool, jailDumpDir, spillDir string) (string, error) {
	plan.snapshotType = "Full"
	if !trackDirtyPages {
		return "", nil
	}
	basePath, err := takeMemoryDiffBase(plan.sandboxDir, plan.staging)
	if err != nil {
		return "", fmt.Errorf("invalidate memory diff base: %w", err)
	}
	if basePath == "" {
		return "", nil
	}
	dumpDir := plan.staging
	switch {
	case plan.spilled:
		dumpDir = spillDir
	case plan.jailed:
		dumpDir = jailDumpDir
	}
	if artifacts.SupportsMemoryDiffExtents(dumpDir) {
		plan.snapshotType = "Diff"
	}
	return basePath, nil
}

func materializeMemoryDiff(basePath, diffPath, targetPath string) error {
	merged := targetPath + ".merged"
	defer os.Remove(merged)
	if err := copyReflinkOrCopy(basePath, merged); err != nil {
		return fmt.Errorf("copy memory diff base: %w", err)
	}
	if err := artifacts.ApplyMemoryDiff(merged, diffPath); err != nil {
		return fmt.Errorf("apply Firecracker memory diff: %w", err)
	}
	return os.Rename(merged, targetPath)
}

// stageCapturedMemory runs after resume, while the per-sandbox mutation lock
// is still held. Apply directly from spill so an ordinary copy cannot fill
// the sparse diff's holes before they are interpreted.
func stageCapturedMemory(plan *dumpPlan, basePath, jailDumpDir, spillDir string) error {
	memory := filepath.Join(plan.staging, memorySnapshotName)
	vmstate := filepath.Join(plan.staging, vmstateSnapshotName)
	sourceMemory, sourceVMState := memory, vmstate
	move := os.Rename
	switch {
	case plan.spilled:
		sourceMemory = filepath.Join(spillDir, jailedDumpMemoryName)
		sourceVMState = filepath.Join(spillDir, jailedDumpVMStateName)
		move = func(src, dst string) error {
			started := time.Now()
			err := relocateSpilled(plan, src, dst)
			plan.spillMove += time.Since(started)
			return err
		}
	case plan.jailed:
		sourceMemory = filepath.Join(jailDumpDir, jailedDumpMemoryName)
		sourceVMState = filepath.Join(jailDumpDir, jailedDumpVMStateName)
	}
	if sourceVMState != vmstate {
		if err := move(sourceVMState, vmstate); err != nil {
			return fmt.Errorf("stage VM state: %w", err)
		}
	}
	if plan.snapshotType == "Diff" {
		mergeStarted := time.Now()
		err := materializeMemoryDiff(basePath, sourceMemory, memory)
		plan.memoryMerge = time.Since(mergeStarted)
		if plan.jailed && !plan.spilled {
			_ = os.Remove(sourceMemory)
		}
		return err
	}
	if sourceMemory != memory {
		if err := move(sourceMemory, memory); err != nil {
			return fmt.Errorf("stage guest memory: %w", err)
		}
	}
	return nil
}
