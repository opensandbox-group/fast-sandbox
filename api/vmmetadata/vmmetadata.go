// Package vmmetadata is the public, read-only contract of the per-VM
// discovery record (fast-sandbox.json) that the Firecracker driver writes
// when provisioning each VM. Node-local agents parse this record to map a VM
// directory back to its Sandbox identity without watching Sandbox CRs.
//
// Location (next to the VM's firecracker.log):
//
//	jailer mode:  <StateRoot>/jails/firecracker/<uid[:32]>/root/fast-sandbox.json
//	direct mode:  <StateRoot>/sandboxes/<sandbox-uid>/fast-sandbox.json
//
// Writer contract (the driver): the record is written once with a single
// write call and never modified afterwards; no fsync; the write is
// asynchronous and never fails provisioning; the surrounding jail/state
// directory is removed atomically at teardown, so the record never outlives
// the log.
//
// Consumer contract (node-local agents): a missing or unparseable record
// means "not ready yet" — skip and retry on the next periodic scan, never
// fatal. A directory disappearing is the stream-end signal. placementAttempt
// is optional and disambiguates a stale jail from its replacement when a
// sandbox moves back to the same node before GC.
package vmmetadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// FileName is the fixed record name inside a VM directory.
	FileName = "fast-sandbox.json"
	// CurrentVersion is the schema version emitted by this release.
	CurrentVersion = 1
)

// Metadata is the immutable per-VM discovery record. It is a write-once
// association only and carries no lifecycle state.
type Metadata struct {
	Version          int    `json:"version"`
	UID              string `json:"uid"`
	Namespace        string `json:"namespace"`
	Name             string `json:"name"`
	PlacementAttempt int64  `json:"placementAttempt,omitempty"`
}

// Read parses the record from a VM directory. Per the consumer contract,
// callers treat a returned error (including os.ErrNotExist and parse
// failures on a partially visible write) as "not ready yet" and retry.
func Read(directory string) (Metadata, error) {
	payload, err := os.ReadFile(filepath.Join(directory, FileName))
	if err != nil {
		return Metadata{}, err
	}
	return Parse(payload)
}

// Parse decodes and validates a record payload.
func Parse(payload []byte) (Metadata, error) {
	var metadata Metadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("parse VM metadata: %w", err)
	}
	if metadata.Version < 1 {
		return Metadata{}, fmt.Errorf("invalid VM metadata version %d", metadata.Version)
	}
	if metadata.UID == "" {
		return Metadata{}, fmt.Errorf("invalid VM metadata: uid is required")
	}
	return metadata, nil
}
