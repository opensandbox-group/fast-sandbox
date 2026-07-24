package state

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	boxliteprotocol "fast-sandbox/internal/runtime/boxlite/protocol"
)

const (
	Version               = 1
	OwnerFileName         = "fast-sandbox-owner.json"
	MetadataDirectoryName = "fast-sandbox-metadata"
	BundleDirectoryName   = "fast-sandbox-bundles"
	RuntimeLockFileName   = ".lock"
)

type OwnerRecord struct {
	Version       int    `json:"version"`
	FastletPodUID string `json:"fastletPodUid"`
	CreatedAt     int64  `json:"createdAt"`
}

type SandboxRecord struct {
	Version          int                           `json:"version"`
	Namespace        string                        `json:"namespace"`
	SpecHash         string                        `json:"specHash"`
	Request          boxliteprotocol.EnsureRequest `json:"request"`
	BoxID            string                        `json:"boxId,omitempty"`
	HostPort         uint32                        `json:"hostPort"`
	TunnelCredential string                        `json:"tunnelCredential"`
	CreatedAt        int64                         `json:"createdAt"`
	BundleRoot       string                        `json:"bundleRoot"`
}

func HomeDirectory(stateRoot, fastletPodUID string) string {
	return filepath.Join(stateRoot, SafeSegment(fastletPodUID))
}

func RecordFileName(sandboxUID string) string {
	digest := sha256.Sum256([]byte(sandboxUID))
	return hex.EncodeToString(digest[:]) + ".json"
}

func SafeSegment(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}
