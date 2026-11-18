package firecracker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/firecrackervmm"
)

// The authored profile carries only what the node cannot derive: the
// executable digest of a custom VMM and an explicit exclusive-writer
// declaration. Everything else — backend, kernel and overlaybd digests, the
// storage revision — follows the deployed source and topology.
type rootfsQualificationProfile struct {
	FirecrackerSHA256 string `json:"firecrackerSHA256,omitempty"`
	ExclusiveExt4     *bool  `json:"exclusiveExt4,omitempty"`
}

// ExclusiveExt4 remains a topology assumption, not a proof about arbitrary writers.
type rootfsQualification struct {
	Backend           string
	FirecrackerSHA256 string
	KernelSHA256      string
	OverlayBDSHA256   string
	StorageRevision   string
	ExclusiveExt4     bool
}

type rootfsQualificationSources struct {
	Native           bool
	KernelSHA256     string
	ManagedVMM       bool
	OverlayBDBundle  string
	StandardTopology bool
	StateRoot        string
}

func (d *Driver) rootfsQualificationSources(state *SandboxState, m artifacts.VMManifest) rootfsQualificationSources {
	managed := state.VMM.IsManaged(d.config.VMMDir)
	return rootfsQualificationSources{
		KernelSHA256: m.Kernel.SHA256, ManagedVMM: managed,
		OverlayBDBundle:  filepath.Join(runtimecatalog.OCIToolsDir, "current"),
		StandardTopology: managed && filepath.Clean(d.config.VMMDir) == firecrackervmm.DefaultDir && filepath.Clean(d.config.StateRoot) == runtimecatalog.FirecrackerStateRootDir,
		StateRoot:        d.config.StateRoot,
	}
}

func readRootfsQualification(source rootfsQualificationSources) (rootfsQualification, error) {
	profile, err := readQualificationOverrides()
	if err != nil {
		return rootfsQualification{}, err
	}
	q := rootfsQualification{FirecrackerSHA256: profile.FirecrackerSHA256}
	// The backend follows the source encoding; there is no profile field for
	// it. Native captures derive their storage identity from the StateRoot —
	// the same role the deployed bundle's REVISION plays for OverlayBD (#224).
	if source.Native {
		q.Backend = captureBackendNative
		if q.StorageRevision, err = stateRootStorageRevision(source.StateRoot); err != nil {
			return rootfsQualification{}, err
		}
	} else {
		entries, revision, err := readOverlayBDBundle(source.OverlayBDBundle)
		if err != nil {
			return rootfsQualification{}, err
		}
		// bin/overlaybd-ublkd is a wrapper, not the running executable.
		var exists bool
		q.OverlayBDSHA256, exists = entries["libexec/overlaybd-ublkd"]
		if !exists {
			return rootfsQualification{}, errors.New("OverlayBD bundle does not list libexec/overlaybd-ublkd")
		}
		q.StorageRevision = revision
	}
	q.KernelSHA256 = source.KernelSHA256
	if !validQualificationDigest(q.KernelSHA256) || (!source.Native && !validQualificationDigest(q.OverlayBDSHA256)) {
		return rootfsQualification{}, errors.New("qualification requires SHA-256 digests")
	}
	if !source.ManagedVMM && q.FirecrackerSHA256 == "" {
		return rootfsQualification{}, errors.New("custom VMM requires firecrackerSHA256 qualification")
	}
	if profile.ExclusiveExt4 != nil {
		q.ExclusiveExt4 = *profile.ExclusiveExt4
	} else {
		q.ExclusiveExt4 = source.StandardTopology
	}
	if !q.ExclusiveExt4 {
		return rootfsQualification{}, errors.New("capture requires the exclusive-writer storage contract; custom topologies must declare exclusiveExt4")
	}
	return q, nil
}

// stateRootStorageRevision derives a stable storage identity from the
// StateRoot filesystem, standing in for the bundle REVISION the OverlayBD
// path derives: changing the backing filesystem invalidates prior native
// capture journals and attestations.
func stateRootStorageRevision(stateRoot string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(stateRoot, &st); err != nil {
		return "", fmt.Errorf("read the StateRoot storage identity: %w", err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("statfs:%x:%x:%d", st.Fsid.Val[0], st.Fsid.Val[1], st.Blocks)))
	return hex.EncodeToString(sum[:]), nil
}

func readQualificationOverrides() (rootfsQualificationProfile, error) {
	var profile rootfsQualificationProfile
	path := os.Getenv("FAST_SANDBOX_ROOTFS_QUALIFICATION")
	if path == "" {
		return profile, nil
	}
	if !filepath.IsAbs(path) {
		return profile, errors.New("FAST_SANDBOX_ROOTFS_QUALIFICATION must be an absolute path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return profile, err
	}
	// Exact-key matching: encoding/json also accepts case variants of the
	// known fields, so DisallowUnknownFields alone would let a typo'd
	// override silently fall back to the derived default.
	var provided map[string]json.RawMessage
	if err = json.Unmarshal(raw, &provided); err != nil || provided == nil {
		return profile, errors.New("FAST_SANDBOX_ROOTFS_QUALIFICATION must be a JSON object")
	}
	for name, value := range provided {
		if name != "firecrackerSHA256" && name != "exclusiveExt4" {
			return profile, fmt.Errorf("unknown qualification override %s", name)
		}
		if string(value) == "null" {
			return profile, fmt.Errorf("qualification override %s cannot be null", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&profile); err != nil {
		return profile, fmt.Errorf("parse FAST_SANDBOX_ROOTFS_QUALIFICATION: %w", err)
	}
	if rawDigest, ok := provided["firecrackerSHA256"]; ok {
		var digest string
		if err = json.Unmarshal(rawDigest, &digest); err != nil {
			return profile, fmt.Errorf("firecrackerSHA256 must be a string digest: %w", err)
		}
		if !validQualificationDigest(digest) {
			return profile, errors.New("firecrackerSHA256 must be a lowercase SHA-256 digest")
		}
		profile.FirecrackerSHA256 = digest
	}
	return profile, nil
}

func readOverlayBDBundle(path string) (map[string]string, string, error) {
	// Resolve current once so a rolling upgrade cannot mix two releases.
	bundle, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("OverlayBD bundle unavailable: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "SHA256SUMS"))
	if err != nil {
		return nil, "", err
	}
	entries, err := artifacts.ParseSHA256SUMS(raw)
	if err != nil {
		return nil, "", err
	}
	revision, err := os.ReadFile(filepath.Join(bundle, "REVISION"))
	if err != nil {
		return nil, "", err
	}
	wantRevision := fmt.Sprintf("%x", sha256.Sum256(raw))
	if strings.TrimSpace(string(revision)) != wantRevision {
		return nil, "", errors.New("OverlayBD REVISION does not match SHA256SUMS")
	}
	return entries, wantRevision, nil
}

func validQualificationDigest(sum string) bool {
	b, err := hex.DecodeString(sum)
	return err == nil && len(b) == sha256.Size && strings.ToLower(sum) == sum
}
