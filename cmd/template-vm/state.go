package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// In-disk layout convention of the snapfiles ext4 filesystem (ADR-006). These
// are an existing external convention the CLI adapts to; they are centralized
// here so a builder-side change is a one-line update.
const (
	inDiskVMState   = "vmstate.bin"
	inDiskMemfile   = "memfile"
	inDiskMetadata  = "metadata.json"
	snapfilesMount  = "mnt"
	apiSocketName   = "api.sock"
	serialLogName   = "firecracker.log"
	stateFileName   = "state.json"
	stateFileFormat = 1
)

// sandboxState is the persisted record of a created sandbox (state.json). It
// is the source of truth for list liveness checks and idempotent delete.
type sandboxState struct {
	FormatVersion int    `json:"format_version"`
	SandboxID     string `json:"sandbox_id"`
	RootfsRef     string `json:"rootfs_ref"`
	SnapfilesRef  string `json:"snapfiles_ref"`

	RootfsDevID      int    `json:"rootfs_dev_id"`
	RootfsDevPath    string `json:"rootfs_dev_path"`
	SnapfilesDevID   int    `json:"snapfiles_dev_id"`
	SnapfilesDevPath string `json:"snapfiles_dev_path"`

	MountPoint     string `json:"mount_point"`
	APISocket      string `json:"api_socket"`
	SerialLog      string `json:"serial_log"`
	FirecrackerPID int    `json:"firecracker_pid"`
	RootfsDriveID  string `json:"rootfs_drive_id"`

	Network   string `json:"network"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	CreatedAt string `json:"created_at"`
}

// statePath returns the state.json path of a sandbox home.
func statePath(home string) string {
	return filepath.Join(home, stateFileName)
}

// writeState persists state atomically (tmp+rename) inside the sandbox home.
func writeState(home string, state sandboxState) error {
	state.FormatVersion = stateFileFormat
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	target := statePath(home)
	temp, err := os.CreateTemp(home, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		return fmt.Errorf("commit state %s: %w", target, err)
	}
	return nil
}

// readState loads a sandbox's state.json.
func readState(home string) (sandboxState, error) {
	payload, err := os.ReadFile(statePath(home))
	if err != nil {
		return sandboxState{}, err
	}
	var state sandboxState
	if err := json.Unmarshal(payload, &state); err != nil {
		return sandboxState{}, fmt.Errorf("parse state %s: %w", statePath(home), err)
	}
	return state, nil
}

// listSandboxIDs enumerates the sandbox ids under the state directory.
func listSandboxIDs(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}
