package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SHA256Of returns the lowercase hex SHA-256 of a byte slice.
func SHA256Of(payload []byte) string {
	digest := sha256Sum(payload)
	return digest
}

// Digest16 returns the first 16 hex characters of a manifest document's
// SHA-256: the per-build namespace of one artifact set in the store
// (<storeRoot>/<digest16>/...).
func Digest16(manifestBytes []byte) string {
	return SHA256Of(manifestBytes)[:16]
}

// MarshalManifest serializes a manifest document with the canonical
// producer encoding: two-space indented JSON plus a trailing newline. The
// artifact digest and the per-build namespace are both derived from these
// exact bytes, so every producer must marshal identically.
func MarshalManifest(document map[string]any) ([]byte, error) {
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// FileEntry describes one artifact in a manifest: a sparse-aware sha256 and
// the logical (sparse) size. cache memoizes checksums across the manifest
// and SHA256SUMS of one artifact set; it may be nil.
func FileEntry(path string, cache map[string]string) (map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	sum, err := SHA256FileCached(path, cache)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sha256":    sum,
		"sizeBytes": info.Size(),
	}, nil
}

// SHA256SUMSName is the checksum manifest file of an artifact set.
const SHA256SUMSName = "SHA256SUMS"

// WriteSHA256SUMS writes SHA256SUMS covering the given artifact set
// (relative names), reusing the memoized checksums already computed for the
// manifest. Lines are "<sha256>  <relative-path>" joined with newlines plus
// one trailing newline.
func WriteSHA256SUMS(workdir string, artifacts []string, cache map[string]string) error {
	lines := make([]string, 0, len(artifacts))
	for _, relative := range artifacts {
		sum, err := SHA256FileCached(filepath.Join(workdir, relative), cache)
		if err != nil {
			return err
		}
		lines = append(lines, sum+"  "+relative)
	}
	return os.WriteFile(filepath.Join(workdir, SHA256SUMSName), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// SHA256FileCached returns the checksum of path, memoized in cache.
func SHA256FileCached(path string, cache map[string]string) (string, error) {
	if cache == nil {
		cache = map[string]string{}
	}
	if sum, ok := cache[path]; ok {
		return sum, nil
	}
	sum, err := SHA256File(path)
	if err != nil {
		return "", err
	}
	cache[path] = sum
	return sum, nil
}

// ImageIndexKey derives the content-addressed index key of an image or
// template-name reference. It matches the consumer-side cache key, so a
// consumer can resolve the latest published manifest without control-plane
// coordination. The key must be byte-identical across producers and
// consumers: no normalization.
func ImageIndexKey(reference string) string {
	return SHA256Of([]byte(reference))
}

// ImageIndexPayload builds the image index document pointing at a published
// manifest. The image field equals the key the document is written under so
// pull-side byte matching works unchanged.
func ImageIndexPayload(image, manifestURI, artifactDigest string, now time.Time) ([]byte, error) {
	document := struct {
		Image          string `json:"image"`
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
		UpdatedAt      string `json:"updatedAt"`
	}{
		Image:          image,
		ManifestRef:    manifestURI,
		ArtifactDigest: artifactDigest,
		UpdatedAt:      now.UTC().Format(time.RFC3339),
	}
	return json.MarshalIndent(document, "", "  ")
}

// FirecrackerVersion reads a Firecracker binary's version (e.g. "1.16.1"),
// falling back to "unknown".
func FirecrackerVersion(binary string) string {
	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	fields := strings.Fields(string(output))
	for index, field := range fields {
		if field == "v" && index+1 < len(fields) {
			return strings.TrimPrefix(fields[index+1], "v")
		}
	}
	return "unknown"
}

// HostKernelRelease returns the host kernel release (uname -r).
func HostKernelRelease() string {
	output, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

// HostCPUModel returns the first CPU model name from /proc/cpuinfo.
func HostCPUModel() string {
	payload, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return "unknown"
}

// SizeGiB rounds a byte size up to whole SI GiB, minimum one. It matches the
// builder's rootfsSize convention ("<N>G" of the rounded artifact size).
func SizeGiB(sizeBytes int64) int {
	gib := (sizeBytes + (1 << 30) - 1) >> 30
	if gib < 1 {
		return 1
	}
	return int(gib)
}

func sha256Sum(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
