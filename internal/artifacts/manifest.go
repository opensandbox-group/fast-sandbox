package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// unknownProvenanceValue is the placeholder manifest value for host
// metadata fields (firecracker version, kernel release, CPU model) that
// cannot be detected when the artifact set is produced.
const unknownProvenanceValue = "unknown"

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
	return os.WriteFile(filepath.Join(workdir, SHA256SUMSName), []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:gosec // published artifact-set member; kept world-readable like every other staged artifact file
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

// CheckpointReference returns the canonical cache reference of a checkpoint
// artifact set. A checkpoint is instance-private: it publishes no image
// index and is never addressable as a CreateSandbox image. Producers and
// consumers (the node cache and the driver restore path) therefore agree on
// this synthetic reference derived from the immutable manifest digest, so
// the standard image cache layout keyed by imageKey(reference) is reused
// unchanged across hosts.
func CheckpointReference(artifactDigest string) string {
	return "checkpoint:sha256:" + artifactDigest
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
		return unknownProvenanceValue
	}
	return parseFirecrackerVersion(string(output))
}

// parseFirecrackerVersion extracts the version from `firecracker --version`
// output ("Firecracker v1.16.1"): the first field carrying a v-prefixed
// version token. Older releases printed the bare "v" as a separate token
// with the version following; both forms parse identically.
func parseFirecrackerVersion(output string) string {
	fields := strings.Fields(output)
	for index, field := range fields {
		version, ok := strings.CutPrefix(field, "v")
		if !ok {
			continue
		}
		if version != "" {
			return version
		}
		if index+1 < len(fields) {
			return strings.TrimPrefix(fields[index+1], "v")
		}
	}
	return unknownProvenanceValue
}

// HostKernelRelease returns the host kernel release (uname -r).
func HostKernelRelease() string {
	output, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return unknownProvenanceValue
	}
	return strings.TrimSpace(string(output))
}

// CPU vendors as they appear in /proc/cpuinfo vendor_id.
const (
	VendorGenuineIntel = "GenuineIntel"
	VendorAuthenticAMD = "AuthenticAMD"
)

// CPUIdentity is the host's CPUID identity from /proc/cpuinfo (the model
// name is display-only).
type CPUIdentity struct {
	Vendor    string `json:"vendor"`
	Family    int    `json:"cpuFamily"`
	Model     int    `json:"cpuModel"`
	Stepping  int    `json:"cpuStepping"`
	ModelName string `json:"cpuModelName"`
}

// HostCPUIdentity returns the CPU identity of the first /proc/cpuinfo
// processor.
func HostCPUIdentity() CPUIdentity {
	payload, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return CPUIdentity{Vendor: unknownProvenanceValue}
	}
	return parseCPUIdentity(payload)
}

// parseCPUIdentity parses the first processor block of /proc/cpuinfo
// content; unparsable fields stay 0 / "unknown".
func parseCPUIdentity(payload []byte) CPUIdentity {
	identity := CPUIdentity{Vendor: unknownProvenanceValue}
	for _, line := range strings.Split(string(payload), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch name {
		case "vendor_id":
			if identity.Vendor == unknownProvenanceValue {
				identity.Vendor = value
			}
		case "cpu family":
			if identity.Family == 0 {
				identity.Family, _ = strconv.Atoi(value)
			}
		case "model":
			if identity.Model == 0 {
				identity.Model, _ = strconv.Atoi(value)
			}
		case "stepping":
			if identity.Stepping == 0 {
				identity.Stepping, _ = strconv.Atoi(value)
			}
		case "model name":
			if identity.ModelName == "" {
				identity.ModelName = value
			}
		}
	}
	return identity
}

// SnapshotCompatibility mirrors the manifest compatibility block. Zero
// Vendor and CPUTemplate means a pre-structured (legacy) manifest.
type SnapshotCompatibility struct {
	Vendor             string `json:"vendor"`
	CPUFamily          int    `json:"cpuFamily"`
	CPUModel           int    `json:"cpuModel"`
	CPUStepping        int    `json:"cpuStepping"`
	CPUModelName       string `json:"cpuModelName"`
	CPUTemplate        string `json:"cpuTemplate"`
	FirecrackerVersion string `json:"firecrackerVersion"`
	HostKernel         string `json:"hostKernel"`
}

// fms is one allowlist entry: an exact vendor/family/model/stepping
// identity.
type fms struct {
	vendor   string
	family   int
	model    int
	stepping int
}

func (m fms) String() string {
	return fmt.Sprintf("%s family %d model %d stepping %d", m.vendor, m.family, m.model, m.stepping)
}

// cpuTemplateAllowlists pins, per Firecracker release, the CPUs each static
// template permits (mirrors upstream static_cpu_templates).
var cpuTemplateAllowlists = map[string]map[string][]fms{
	"1.16.1": {
		"T2": {
			{vendor: VendorGenuineIntel, family: 6, model: 85, stepping: 4},  // Skylake-SP
			{vendor: VendorGenuineIntel, family: 6, model: 85, stepping: 7},  // Cascade Lake-SP
			{vendor: VendorGenuineIntel, family: 6, model: 106, stepping: 6}, // Ice Lake-SP
		},
		"T2A": {
			{vendor: VendorAuthenticAMD, family: 25, model: 1, stepping: 1}, // EPYC Milan
		},
	},
}

// CompatibilityCPUTemplate reports which template tier the identity
// supports under the given Firecracker version: "T2", "T2A", or "none"
// (only identity-matched unmasked snapshots). An unknown version yields
// "none".
func CompatibilityCPUTemplate(version string, identity CPUIdentity) string {
	entry := fms{vendor: identity.Vendor, family: identity.Family, model: identity.Model, stepping: identity.Stepping}
	for template, allowlist := range cpuTemplateAllowlists[version] {
		for _, candidate := range allowlist {
			if candidate == entry {
				return template
			}
		}
	}
	return "none"
}

// Admission errors from MatchRestoreCompatibility: legacy is admitted with
// a warning by the caller, the rest reject the restore.
var (
	ErrLegacyCompatibility        = errors.New("manifest carries no structured compatibility")
	ErrFirecrackerVersionMismatch = errors.New("firecracker version mismatch")
	ErrUnknownCPUTemplate         = errors.New("unknown cpu template")
	ErrCPUIncompatible            = errors.New("snapshot CPU incompatible with this node")
)

// MatchRestoreCompatibility decides whether a node may restore a snapshot
// with the given compatibility block:
//
//   - "T2"/"T2A": node CPU must be in that template's per-version allowlist
//     (the mask normalizes the guest CPUID, so build-host identity does not
//     matter);
//   - "none": node vendor/family/model must equal the snapshot identity
//     (stepping is recorded but not compared);
//   - legacy block: ErrLegacyCompatibility, the caller may admit with a
//     warning.
//
// Manifest and node Firecracker versions must be equal in every tier.
func MatchRestoreCompatibility(compat SnapshotCompatibility, local CPUIdentity, localFirecrackerVersion string) error {
	if compat.CPUTemplate == "" && compat.Vendor == "" {
		return ErrLegacyCompatibility
	}
	if compat.FirecrackerVersion != localFirecrackerVersion {
		return fmt.Errorf("%w: snapshot built with %q, node runs %q", ErrFirecrackerVersionMismatch, compat.FirecrackerVersion, localFirecrackerVersion)
	}
	switch compat.CPUTemplate {
	case "T2", "T2A":
		allowlist, ok := cpuTemplateAllowlists[compat.FirecrackerVersion][compat.CPUTemplate]
		if !ok {
			return fmt.Errorf("%w: %q has no %q allowlist row", ErrUnknownCPUTemplate, compat.FirecrackerVersion, compat.CPUTemplate)
		}
		localFMS := fms{vendor: local.Vendor, family: local.Family, model: local.Model, stepping: local.Stepping}
		for _, entry := range allowlist {
			if entry == localFMS {
				return nil
			}
		}
		return fmt.Errorf("%w: template %q permits %v, node is %s", ErrCPUIncompatible, compat.CPUTemplate, allowlist, localFMS)
	case "", "none":
		if compat.Vendor == local.Vendor && compat.CPUFamily == local.Family && compat.CPUModel == local.Model {
			return nil
		}
		return fmt.Errorf("%w: unmasked snapshot identity is %s family %d model %d, node is %s family %d model %d",
			ErrCPUIncompatible, compat.Vendor, compat.CPUFamily, compat.CPUModel, local.Vendor, local.Family, local.Model)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownCPUTemplate, compat.CPUTemplate)
	}
}

// SizeGiB rounds a byte size up to whole GiB, minimum one. It matches the
// builder's machine.rootfs convention ("<N>Gi" of the rounded artifact size).
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
