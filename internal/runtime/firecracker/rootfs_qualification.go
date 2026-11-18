package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

// The derived qualification records everything a native or OverlayBD
// capture attests. There is no authored profile: the node derives the whole
// contract from its own deployment — the source manifest's boot provenance,
// the deployed OverlayBD bundle, and the exclusive-writer contract implied
// by the managed VMM release layout (#224).
type rootfsQualification struct {
	Backend         string
	KernelSHA256    string
	OverlayBDSHA256 string
	ExclusiveExt4   bool
}

type rootfsQualificationSources struct {
	Native          bool
	KernelSHA256    string
	ManagedVMM      bool
	OverlayBDBundle string
	StateRoot       string
}

func (d *Driver) rootfsQualificationSources(state *SandboxState, m artifacts.VMManifest) rootfsQualificationSources {
	managed := state.VMM.IsManaged(d.config.VMMDir)
	return rootfsQualificationSources{
		KernelSHA256: m.Kernel.SHA256, ManagedVMM: managed,
		OverlayBDBundle: filepath.Join(runtimecatalog.OCIToolsDir, "current"),
		StateRoot:       d.config.StateRoot,
	}
}

func readRootfsQualification(source rootfsQualificationSources) (rootfsQualification, error) {
	q := rootfsQualification{ExclusiveExt4: source.ManagedVMM}
	// The backend follows the source encoding.
	if source.Native {
		q.Backend = captureBackendNative
	} else {
		entries, _, err := readOverlayBDBundle(source.OverlayBDBundle)
		if err != nil {
			return rootfsQualification{}, err
		}
		// bin/overlaybd-ublkd is a wrapper, not the running executable.
		var exists bool
		q.OverlayBDSHA256, exists = entries["libexec/overlaybd-ublkd"]
		if !exists {
			return rootfsQualification{}, errors.New("OverlayBD bundle does not list libexec/overlaybd-ublkd")
		}
	}
	q.KernelSHA256 = source.KernelSHA256
	if !validQualificationDigest(q.KernelSHA256) {
		return rootfsQualification{}, errors.New("the source template does not record a valid kernel SHA-256; rebuild the template and recreate the sandbox")
	}
	if !source.Native && !validQualificationDigest(q.OverlayBDSHA256) {
		return rootfsQualification{}, errors.New("the deployed OverlayBD bundle does not provide a valid libexec/overlaybd-ublkd digest")
	}
	if !q.ExclusiveExt4 {
		return rootfsQualification{}, errors.New("rootfs-only capture requires a managed VMM release layout")
	}
	return q, nil
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
