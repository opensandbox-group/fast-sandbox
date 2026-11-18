package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

// The derived qualification records everything a native or OverlayBD
// capture attests. There is no authored profile: the node derives the whole
// contract from its own deployment — the source manifest's boot provenance,
// the deployed OverlayBD bundle, and the StateRoot filesystem identity.
// ExclusiveExt4 follows the managed VMM release layout: a Fastlet that boots
// its microVMs from the configured releases directory owns the deployment's
// storage contract (#224).
type rootfsQualification struct {
	Backend         string
	KernelSHA256    string
	OverlayBDSHA256 string
	StorageRevision string
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
	// The backend follows the source encoding. Native captures derive their
	// storage identity from the StateRoot — the same role the deployed
	// bundle's REVISION plays for OverlayBD (#224).
	if source.Native {
		q.Backend = captureBackendNative
		revision, err := stateRootStorageRevision(source.StateRoot)
		if err != nil {
			return rootfsQualification{}, err
		}
		q.StorageRevision = revision
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
	if !q.ExclusiveExt4 {
		return rootfsQualification{}, errors.New("rootfs-only capture requires a managed VMM release layout")
	}
	return q, nil
}

// stateRootStorageRevision derives a stable storage identity from the
// StateRoot filesystem, standing in for the bundle REVISION the OverlayBD
// path derives: changing the backing filesystem invalidates prior native
// capture journals and attestations.
func stateRootStorageRevision(stateRoot string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(stateRoot, &st); err != nil {
		return "", fmt.Errorf("read the StateRoot storage identity: %w", err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("statfs:%x:%x:%d", st.Fsid.Val[0], st.Fsid.Val[1], st.Blocks)))
	return hex.EncodeToString(sum[:]), nil
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
