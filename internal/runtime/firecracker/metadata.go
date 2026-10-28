package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// vmMetadataName is the write-once discovery record next to the process log
// (the jail root in jailer mode, the state directory in direct mode). It
// carries the full Sandbox UID, which the jail directory name truncates to
// 32 characters. The reader contract: docs/reference/vm-metadata.md.
const vmMetadataName = "fast-sandbox.json"

const vmMetadataVersion = 1

// VMMetadata is the immutable per-VM discovery record for node-local agents;
// it carries no lifecycle state.
type VMMetadata struct {
	Version          int    `json:"version"`
	UID              string `json:"uid"`
	Namespace        string `json:"namespace"`
	Name             string `json:"name"`
	PlacementAttempt int64  `json:"placementAttempt,omitempty"`
}

// writeVMMetadataFile emits the record with a single write call: readers see
// either the full content or a short prefix and retry. Write-once (O_EXCL),
// no fsync — the worst case on power loss is one uncollected console.
func writeVMMetadataFile(directory string, metadata VMMetadata) error {
	if directory == "" || metadata.UID == "" {
		return fmt.Errorf("%w: VM metadata requires a directory and a Sandbox UID", ErrInvalidConfig)
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(directory, vmMetadataName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// publishVMMetadata writes the record in the background; consumers treat a
// missing record as "not ready yet". A permanent failure is logged — that
// VM's console cannot be collected, but provisioning is unaffected.
func (d *Driver) publishVMMetadata(directory string, identity fastletapi.SandboxIdentity) {
	go func() {
		metadata := VMMetadata{
			Version:          vmMetadataVersion,
			UID:              identity.SandboxUID,
			Namespace:        identity.Namespace,
			Name:             identity.Name,
			PlacementAttempt: identity.AssignmentAttempt,
		}
		if err := writeVMMetadataFile(directory, metadata); err != nil {
			klog.ErrorS(err, "failed to write the VM discovery metadata; node-local agents cannot collect this VM's console output",
				"sandboxID", identity.SandboxUID, "path", filepath.Join(directory, vmMetadataName))
		}
	}()
}
