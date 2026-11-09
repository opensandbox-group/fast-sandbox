package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fast-sandbox/internal/artifacts"
	infracontract "fast-sandbox/internal/infra/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// sandboxStateDir holds one directory per managed Sandbox under the runtime
// StateRoot. Layout:
//
//	<StateRoot>/sandboxes/<sandboxID>/meta.json
//	<StateRoot>/sandboxes/<sandboxID>/api.sock
//
// The directory is the durable anchor for Fastlet restart recovery.
const sandboxStateDir = "sandboxes"

// sandboxMetaName is the persisted runtime state of one Sandbox.
const sandboxMetaName = "meta.json"

// processLogName receives the firecracker process stdout/stderr of a Sandbox.
const processLogName = "firecracker.log"

// VMPhase mirrors the Firecracker machine state tracked by the driver.
type VMPhase string

const (
	PhaseStarting VMPhase = "Starting"
	PhaseRunning  VMPhase = vmRunning
	PhaseStopped  VMPhase = "Stopped"
	// PhaseFailed is a durable terminal observation. Physical resources may
	// already be reclaimed, but the identity remains until Controller ACK.
	PhaseFailed VMPhase = "runtime-failed"
)

// SandboxState is the durable per-Sandbox driver record. It keeps stable
// runtime configuration separate from Driver-produced allocation state so
// restart recovery preserves both authority boundaries.
type SandboxState struct {
	BootKernelSHA256  string                              `json:"bootKernelSHA256,omitempty"`
	BootCompatibility *artifacts.SnapshotCompatibility    `json:"bootCompatibility,omitempty"`
	Config            fastletapi.RuntimeSandboxConfig     `json:"config"`
	Allocation        fastletapi.RuntimeAllocation        `json:"allocation"`
	Phase             VMPhase                             `json:"phase"`
	PID               int                                 `json:"pid,omitempty"`
	APIAddress        string                              `json:"apiAddress,omitempty"`
	CreatedAt         int64                               `json:"createdAt"`
	FailureMessage    string                              `json:"failureMessage,omitempty"`
	InfraServices     []infracontract.ServiceEndpoint     `json:"infraServices,omitempty"`
	InfraDiagnostics  []infracontract.ComponentDiagnostic `json:"infraDiagnostics,omitempty"`
	// StageDurations records the per-stage create timing of the Sandbox
	// (acquire/rootfs/infra/launch/configure/boot) for load bottleneck
	// analysis (serial vs parallel clone batches).
	StageDurations map[string]time.Duration `json:"stageDurations,omitempty"`
}

// validateSandboxID rejects identities that could escape the StateRoot or
// contain characters unsafe for host paths and Firecracker ids.
func validateSandboxID(sandboxID string) error {
	if sandboxID == "" || sandboxID == "." || sandboxID == ".." {
		return fmt.Errorf("%w: invalid sandbox id %q", ErrInvalidConfig, sandboxID)
	}
	for _, character := range sandboxID {
		if !isSafeIDCharacter(character) {
			return fmt.Errorf("%w: invalid sandbox id %q", ErrInvalidConfig, sandboxID)
		}
	}
	return nil
}

// isSafeIDCharacter allows the DNS-style identifier set plus underscore and
// dot, matching what firecracker --id accepts.
func isSafeIDCharacter(character rune) bool {
	return (character >= 'a' && character <= 'z') ||
		(character >= 'A' && character <= 'Z') ||
		(character >= '0' && character <= '9') ||
		character == '-' || character == '_' || character == '.'
}

// sandboxDir returns the per-Sandbox state directory under stateRoot.
func sandboxDir(stateRoot, sandboxID string) (string, error) {
	if err := validateSandboxID(sandboxID); err != nil {
		return "", err
	}
	return filepath.Join(stateRoot, sandboxStateDir, sandboxID), nil
}

// ensureSandboxDir creates the per-Sandbox state directory.
func ensureSandboxDir(stateRoot, sandboxID string) (string, error) {
	directory, err := sandboxDir(stateRoot, sandboxID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", err
	}
	return directory, nil
}

// metaPath returns the meta file path for a Sandbox state directory.
func metaPath(directory string) string {
	return filepath.Join(directory, sandboxMetaName)
}

// saveState replaces meta.json atomically so process death cannot truncate
// the ownership record needed to recover attached OCI devices.
func saveState(directory string, state *SandboxState) error {
	if state == nil {
		return fmt.Errorf("%w: sandbox state is required", ErrInvalidConfig)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".meta-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), metaPath(directory)); err != nil {
		return err
	}
	return syncCaptureDirectory(directory)
}

// loadState reads the Sandbox state; os.ErrNotExist means the Sandbox is not
// managed by this Fastlet.
func loadState(directory string) (*SandboxState, error) {
	payload, err := os.ReadFile(metaPath(directory))
	if err != nil {
		return nil, err
	}
	var state SandboxState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, fmt.Errorf("decode sandbox state %s: %w", directory, err)
	}
	if state.Config.Identity.SandboxUID == "" || state.Phase == "" {
		return nil, fmt.Errorf("%w: incomplete sandbox state in %s", ErrInvalidConfig, directory)
	}
	return &state, nil
}

// removeSandboxDir removes the Sandbox state directory and everything under it.
func removeSandboxDir(directory string) error {
	if directory == "" || filepath.Base(directory) == sandboxStateDir {
		return fmt.Errorf("%w: refusing to remove %q", ErrInvalidConfig, directory)
	}
	return os.RemoveAll(directory)
}

// listSandboxDirs returns the managed Sandbox state directories, skipping
// entries that fail to parse as Sandbox IDs.
func listSandboxDirs(stateRoot string) ([]string, error) {
	base := filepath.Join(stateRoot, sandboxStateDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := validateSandboxID(entry.Name()); err != nil {
			continue
		}
		directories = append(directories, filepath.Join(base, entry.Name()))
	}
	return directories, nil
}
