package agent

// Published manifest: the content-addressed file
// list of one build, downloaded at index.manifestRef and verified against
// index.artifactDigest before any artifact is pulled.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// manifestFile describes one published artifact.
type manifestFile struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// manifest is the consumer-side view of manifest.json; only the file list
// is needed by the pull layer.
type manifest struct {
	Files map[string]manifestFile `json:"files"`
}

// parseManifest decodes a downloaded manifest document.
func parseManifest(payload []byte) (manifest, error) {
	var document manifest
	if err := json.Unmarshal(payload, &document); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if len(document.Files) == 0 {
		return manifest{}, fmt.Errorf("manifest has no files")
	}
	return document, nil
}

// nativeFile pairs a published artifact with its local cache name and the
// expected digest.
type nativeFile struct {
	publish   string
	cache     string
	sha256    string
	sizeBytes int64
}

// nativeArtifactNames maps published artifact names to the local cache
// names. rootfs.ext4 is renamed to rootfs.img so
// the existing resolveRootfsImage consumer needs no change; the rest keep
// their published names. OverlayBD layers are deliberately absent: they
// arrive with the overlaybd stage.
var nativeArtifactNames = []struct{ publish, cache string }{
	{"rootfs.ext4", nativeRootfsCacheName},
	{"vmstate.snap", "vmstate.snap"},
	{"memory.snap", "memory.snap"},
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
		if !validSHA256(entry.SHA256) {
			return nil, fmt.Errorf("manifest artifact %s has an invalid sha256", name.publish)
		}
		files = append(files, nativeFile{
			publish: name.publish, cache: name.cache,
			sha256: entry.SHA256, sizeBytes: entry.SizeBytes,
		})
	}
	return files, nil
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
