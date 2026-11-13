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

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/firecrackervmm"
)

// Expectations come from deployed bundles, with optional operator overrides.
// ExclusiveExt4 remains a topology assumption, not a proof about arbitrary writers.
type rootfsQualification struct {
	FirecrackerSHA256 string `json:"firecrackerSHA256"`
	KernelSHA256      string `json:"kernelSHA256"`
	OverlayBDSHA256   string `json:"overlaybdSHA256"`
	StorageRevision   string `json:"storageRevision"`
	ExclusiveExt4     bool   `json:"exclusiveExt4"`
}

type rootfsQualificationSources struct {
	KernelSHA256     string
	ManagedVMM       bool
	OverlayBDBundle  string
	StandardTopology bool
}

func (d *Driver) rootfsQualificationSources(state *SandboxState, m artifacts.VMManifest) rootfsQualificationSources {
	managed := state.VMM.IsManaged(d.config.VMMDir)
	return rootfsQualificationSources{
		KernelSHA256: m.Kernel.SHA256, ManagedVMM: managed,
		OverlayBDBundle:  filepath.Join(runtimecatalog.OCIToolsDir, "current"),
		StandardTopology: managed && filepath.Clean(d.config.VMMDir) == firecrackervmm.DefaultDir && filepath.Clean(d.config.StateRoot) == runtimecatalog.FirecrackerStateRootDir,
	}
}

func readRootfsQualification(source rootfsQualificationSources) (rootfsQualification, error) {
	q, provided, err := readQualificationOverrides()
	if err != nil {
		return q, err
	}
	if _, ok := provided["kernelSHA256"]; !ok {
		q.KernelSHA256 = source.KernelSHA256
	}
	if _, ok := provided["firecrackerSHA256"]; !ok && !source.ManagedVMM {
		return q, errors.New("custom VMM requires explicit firecrackerSHA256 qualification")
	}
	_, hasBackend := provided["overlaybdSHA256"]
	_, hasRevision := provided["storageRevision"]
	if !hasBackend || !hasRevision {
		entries, revision, err := readOverlayBDBundle(source.OverlayBDBundle)
		if err != nil {
			return q, err
		}
		if !hasBackend {
			// bin/overlaybd-ublkd is a wrapper, not the running executable.
			var exists bool
			q.OverlayBDSHA256, exists = entries["libexec/overlaybd-ublkd"]
			if !exists {
				return q, errors.New("OverlayBD bundle does not list libexec/overlaybd-ublkd")
			}
		}
		if !hasRevision {
			q.StorageRevision = revision
		}
	}
	if _, ok := provided["exclusiveExt4"]; !ok {
		q.ExclusiveExt4 = source.StandardTopology
	}
	digests := []string{q.KernelSHA256, q.OverlayBDSHA256}
	if _, explicit := provided["firecrackerSHA256"]; explicit || q.FirecrackerSHA256 != "" {
		digests = append(digests, q.FirecrackerSHA256)
	}
	for _, sum := range digests {
		b, err := hex.DecodeString(sum)
		if err != nil || len(b) != sha256.Size || strings.ToLower(sum) != sum {
			return q, errors.New("qualification requires SHA256 digests")
		}
	}
	if !q.ExclusiveExt4 || strings.TrimSpace(q.StorageRevision) == "" {
		return q, errors.New("qualification requires exclusive ext4 and a storage revision; custom topologies need explicit overrides")
	}
	return q, nil
}

func readQualificationOverrides() (rootfsQualification, map[string]json.RawMessage, error) {
	var q rootfsQualification
	provided := map[string]json.RawMessage{}
	if path := os.Getenv("FAST_SANDBOX_ROOTFS_QUALIFICATION"); path != "" {
		if !filepath.IsAbs(path) {
			return q, nil, errors.New("FAST_SANDBOX_ROOTFS_QUALIFICATION must be an absolute path")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return q, nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&q); err != nil {
			return q, nil, err
		}
		if err = json.Unmarshal(raw, &provided); err != nil || provided == nil {
			return q, nil, errors.New("qualification overrides must be a JSON object")
		}
		for name, value := range provided {
			switch name {
			case "firecrackerSHA256", "kernelSHA256", "overlaybdSHA256", "storageRevision", "exclusiveExt4":
			default:
				return q, nil, fmt.Errorf("unknown qualification override %s", name)
			}
			if string(value) == "null" {
				return q, nil, fmt.Errorf("qualification override %s cannot be null", name)
			}
		}
	}
	return q, provided, nil
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
