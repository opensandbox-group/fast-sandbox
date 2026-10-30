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

// composeVMMetadata validates the record fields and emits the JSON payload.
func composeVMMetadata(metadata VMMetadata) ([]byte, error) {
	if metadata.UID == "" {
		return nil, fmt.Errorf("%w: VM metadata requires a Sandbox UID", ErrInvalidConfig)
	}
	return json.Marshal(metadata)
}

// createVMMetadataRecord creates the write-once record file (0o640). O_EXCL:
// an existing record is never rewritten; the caller treats an existing record
// as already published.
func createVMMetadataRecord(directory string) (*os.File, error) {
	if directory == "" {
		return nil, fmt.Errorf("%w: VM metadata requires a directory", ErrInvalidConfig)
	}
	return os.OpenFile(filepath.Join(directory, vmMetadataName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
}

// writeVMMetadataPayload emits the payload with a single write call: readers
// see either the full content or a short prefix and retry. No fsync — the
// worst case on power loss is one uncollected console.
func writeVMMetadataPayload(file *os.File, payload []byte) error {
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// publishVMMetadata creates the record on the create path and hands the
// payload write to a goroutine: EnsureSandbox waits only for the O_EXCL
// create, never for the file system to accept the payload. The goroutine
// writes exclusively through the file handle captured here, so its payload
// can never land in a directory that was removed and recreated for a
// replacement VM — the stale handle keeps pointing at the unlinked inode —
// and teardown never observes a new file appearing in a directory it already
// removed. It never fails provisioning: a create or write error (e.g. disk
// full) is logged — that VM's console cannot be collected, but the VM itself
// is unaffected.
func (d *Driver) publishVMMetadata(directory string, identity fastletapi.SandboxIdentity) {
	payload, err := composeVMMetadata(VMMetadata{
		Version:          vmMetadataVersion,
		UID:              identity.SandboxUID,
		Namespace:        identity.Namespace,
		Name:             identity.Name,
		PlacementAttempt: identity.AssignmentAttempt,
	})
	if err != nil {
		klog.ErrorS(err, "failed to compose the VM discovery metadata; node-local agents cannot collect this VM's console output",
			"sandboxID", identity.SandboxUID)
		return
	}
	file, err := createVMMetadataRecord(directory)
	if err != nil {
		if !os.IsExist(err) {
			klog.ErrorS(err, "failed to create the VM discovery metadata; node-local agents cannot collect this VM's console output",
				"sandboxID", identity.SandboxUID, "path", filepath.Join(directory, vmMetadataName))
		}
		return
	}
	write := d.writeVMMetadataPayload
	d.metadataPublication.Add(1)
	go func() {
		defer d.metadataPublication.Done()
		if err := write(file, payload); err != nil {
			klog.ErrorS(err, "failed to write the VM discovery metadata; node-local agents cannot collect this VM's console output",
				"sandboxID", identity.SandboxUID, "path", filepath.Join(directory, vmMetadataName))
		}
	}()
}
