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
	inDiskManifest  = "manifest.json"
	snapfilesMount  = "mnt"
	apiSocketName   = "api.sock"
	serialLogName   = "firecracker.log"
	stateFileName   = "state.json"
	stateFileFormat = 2
)

// sandboxPhase is the lifecycle phase persisted in state.json.
type sandboxPhase string

const (
	// phaseCreating: create is (or was, if it crashed) allocating resources.
	phaseCreating sandboxPhase = "creating"
	// phaseRunning: the VM was resumed and every resource is recorded.
	phaseRunning sandboxPhase = "running"
	// phaseDeleting: a teardown started and has not released everything yet.
	phaseDeleting sandboxPhase = "deleting"
)

// deviceRecord tracks one ublk device a sandbox owns. Config is the absolute
// config.v1.json path handed to /v1/add; ublkd reports it verbatim in
// /v1/list, so it is the device's ownership identity. The dev_id is only
// informational: ids are recycled, so teardown deletes a device only while
// ublkd still lists it under this config.
type deviceRecord struct {
	Config  string `json:"config"`
	DevID   int    `json:"dev_id"`
	DevPath string `json:"dev_path,omitempty"`

	// addOutcomeUnknown is deliberately process-local. If create loses the Add
	// response, its immediate rollback must retain the backing files when List
	// is empty. A later explicit delete can safely retry from persisted state.
	addOutcomeUnknown bool `json:"-"`
}

// path returns the recorded device path for display ("-" when none).
func (r *deviceRecord) path() string {
	if r == nil || r.DevPath == "" {
		return "-"
	}
	return r.DevPath
}

// sandboxState is the persisted record of a sandbox (state.json). create
// writes it before allocating the first resource and updates it after every
// step; teardown clears each record once that resource is released. It is the
// source of truth for list and for a retryable delete.
type sandboxState struct {
	FormatVersion    int             `json:"format_version"`
	SandboxID        string          `json:"sandbox_id"`
	Phase            sandboxPhase    `json:"phase"`
	RootfsRef        string          `json:"rootfs_ref"`
	SnapfilesRef     string          `json:"snapfiles_ref"`
	TemplateMetadata json.RawMessage `json:"template_metadata,omitempty"`

	// A nil device record means "not owned"; a record with an empty DevPath
	// means an add was issued whose reply was never recorded.
	Rootfs    *deviceRecord `json:"rootfs_device,omitempty"`
	Snapfiles *deviceRecord `json:"snapfiles_device,omitempty"`
	// SharedSnapfiles is a durable lease; shared devices are never Sandbox-owned.
	SharedSnapfiles string `json:"shared_snapfiles,omitempty"`

	MountPoint  string         `json:"mount_point"`
	APISocket   string         `json:"api_socket"`
	SerialLog   string         `json:"serial_log"`
	Firecracker *processRecord `json:"firecracker,omitempty"`

	Network   string `json:"network"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	CreatedAt string `json:"created_at"`
}

// sandboxLayout derives every per-sandbox path from the id. Teardown relies
// on these paths being deterministic: it can reclaim the resources of a
// create that crashed before state.json recorded them.
type sandboxLayout struct {
	home            string
	runtime         string
	mountPoint      string
	apiSocket       string
	serialLog       string
	rootfsConfig    string
	snapfilesConfig string
	rootfsLink      string
}

func newSandboxLayout(sandboxID string) sandboxLayout {
	home := global.sandboxHome(sandboxID)
	runtime := global.sandboxRuntime(sandboxID)
	return sandboxLayout{
		home:            home,
		runtime:         runtime,
		mountPoint:      filepath.Join(home, snapfilesMount),
		apiSocket:       filepath.Join(runtime, apiSocketName),
		serialLog:       filepath.Join(runtime, serialLogName),
		rootfsConfig:    filepath.Join(home, rootfsConfigName),
		snapfilesConfig: filepath.Join(home, snapfilesConfigName),
		rootfsLink:      filepath.Join(runtime, rootfsDriveLink),
	}
}

// statePath returns the state.json path of a sandbox home.
func statePath(home string) string {
	return filepath.Join(home, stateFileName)
}

// writeState persists state atomically (tmp+rename+fsync) inside the
// sandbox home.
func writeState(home string, state *sandboxState) error {
	state.FormatVersion = stateFileFormat
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := writeFileAtomic(statePath(home), data, 0o640); err != nil {
		return fmt.Errorf("commit state %s: %w", statePath(home), err)
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
	if state.FormatVersion != stateFileFormat {
		return sandboxState{}, fmt.Errorf("state %s has format_version %d, want %d", statePath(home), state.FormatVersion, stateFileFormat)
	}
	return state, nil
}

// listSandboxIDs enumerates the sandbox ids under the state directory. Names
// that are not valid sandbox ids (the lock directory, stray files) are
// skipped.
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
		if entry.IsDir() && validateSandboxID(entry.Name()) == nil {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}
