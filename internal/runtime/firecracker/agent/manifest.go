package agent

// Published manifest: the content-addressed file
// list of one build, downloaded at index.manifestRef and verified against
// index.artifactDigest before any artifact is pulled.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"fast-sandbox/internal/artifacts"
)

// manifest adds transport-specific helpers to the shared metadata model.
type manifest artifacts.VMManifest

func parseManifest(payload []byte) (manifest, error) {
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if document.IsRootfsOnly() {
		return manifest{}, fmt.Errorf("rootfsOnly requires OCI transport")
	}
	if len(document.Files) == 0 {
		return manifest{}, fmt.Errorf("manifest has no native files; OCI artifacts must be prepared through fsb-vm")
	}
	return manifest(document), nil
}

// chunkBinding returns the optional chunk index binding of a manifest
// document (nil for a whole-file set). It is a manifest extension outside
// the shared VMManifest model, so it is decoded from the raw bytes.
func chunkBinding(payload []byte) (*artifacts.ChunkIndexRef, error) {
	var document struct {
		Chunks *artifacts.ChunkIndexRef `json:"chunks,omitempty"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("decode manifest chunk binding: %w", err)
	}
	return document.Chunks, nil
}

// nativeFile pairs a published artifact with its local cache name and the
// expected digest: the whole-file sha256, or for a chunked artifact published
// without one, its chunk-list digest (artifacts.ChunkListDigest).
type nativeFile struct {
	publish   string
	cache     string
	sha256    string
	sizeBytes int64
	chunkList *artifacts.ChunkListDigest
}

// chunkListMarkerPrefix keeps a chunk-list digest apart from any whole-file
// sha256 in the verified-cache marker.
const chunkListMarkerPrefix = "chunklist-v1:"

// markerSum is the identity the verified-cache marker records for the file:
// the whole-file sha256 when the manifest has one (so markers written before
// chunk-list digests stay valid), else the prefixed chunk-list digest.
func (f nativeFile) markerSum() string {
	if f.sha256 != "" || f.chunkList == nil {
		return f.sha256
	}
	return chunkListMarkerPrefix + f.chunkList.SHA256
}

// manifestVMStateName and manifestMemoryName are the published artifact
// names of the guest memory snapshots; they double as the local cache names
// and are shared with the publish list.
const (
	manifestVMStateName = "vmstate.snap"
	manifestMemoryName  = "memory.snap"
)

// nativeArtifactNames maps published artifact names to the local cache
// names. rootfs.ext4 is renamed to rootfs.img so
// the existing resolveRootfsImage consumer needs no change; the rest keep
// their published names. OverlayBD images use the separate OCI path.
var nativeArtifactNames = []struct{ publish, cache string }{
	{"rootfs.ext4", nativeRootfsCacheName},
	{manifestVMStateName, manifestVMStateName},
	{manifestMemoryName, manifestMemoryName},
}

// nativeFiles validates that the manifest carries the complete native
// artifact set and returns the mapping with their expected digests.
func (m manifest) nativeFiles() ([]nativeFile, error) {
	files := make([]nativeFile, 0, len(nativeArtifactNames))
	for _, name := range nativeArtifactNames {
		entry, ok := m.Files[name.publish]
		if !ok {
			return nil, fmt.Errorf("manifest is missing the %s artifact", name.publish)
		}
		file := nativeFile{publish: name.publish, cache: name.cache, sizeBytes: entry.SizeBytes}
		switch {
		case entry.SHA256 != "":
			if !validSHA256(entry.SHA256) {
				return nil, fmt.Errorf("manifest artifact %s has an invalid sha256", name.publish)
			}
			file.sha256 = entry.SHA256
		case entry.ChunkList != nil:
			if err := entry.ChunkList.Validate(); err != nil {
				return nil, fmt.Errorf("manifest artifact %s: %w", name.publish, err)
			}
		default:
			return nil, fmt.Errorf("manifest artifact %s has an invalid sha256", name.publish)
		}
		file.chunkList = entry.ChunkList
		files = append(files, file)
	}
	return files, nil
}

// vaultFile returns the optional vault bundle entry of a checkpoint set.
// An absent entry is (file, false, nil) — golden images never carry one; a
// present-but-malformed entry fails the pull, matching nativeFiles(): the
// committed manifest lists the file, so silently skipping it would strand
// the checkpoint's vault state.
func (m manifest) vaultFile() (nativeFile, bool, error) {
	return m.bundleFile(publishVaultName)
}

// policyFile returns the optional recorded-policy bundle entry of a
// snapshot set (checkpoint or template). Same contract as vaultFile().
func (m manifest) policyFile() (nativeFile, bool, error) {
	return m.bundleFile(publishPolicyName)
}

// bundleFile resolves one optional state bundle entry by published name.
func (m manifest) bundleFile(name string) (nativeFile, bool, error) {
	if m.Platform == nil {
		return nativeFile{}, false, nil
	}
	entry, ok := m.Platform.StateFiles[name]
	if !ok {
		return nativeFile{}, false, nil
	}
	if !validSHA256(entry.SHA256) {
		return nativeFile{}, false, fmt.Errorf("manifest artifact %s has an invalid sha256", name)
	}
	return nativeFile{
		publish: name, cache: name,
		sha256: entry.SHA256, sizeBytes: entry.SizeBytes,
	}, true, nil
}

// validSHA256 reports whether sum is a lowercase sha256 hex digest.
func validSHA256(sum string) bool {
	if len(sum) != sha256.Size*2 {
		return false
	}
	for _, character := range sum {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
