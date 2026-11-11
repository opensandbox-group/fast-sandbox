package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/firecrackervmm"
)

const managedRootfsCaptureVersion = "1.17.0"

func (d *Driver) selectVMM(ctx context.Context, version string) (*firecrackervmm.Selection, error) {
	if d.config.VMMDir == "" {
		return nil, nil //nolint:nilnil // Fixed-binary mode has no managed selection.
	}
	if version == "" {
		return nil, fmt.Errorf("existing artifact has no Firecracker version")
	}
	selected, err := firecrackervmm.Resolve(ctx, d.config.VMMDir, version, d.config.JailerPath != "")
	if err != nil {
		return nil, err
	}
	return &selected, nil
}

func (d *Driver) selectCachedVMM(ctx context.Context, image string) (*firecrackervmm.Selection, error) {
	if d.config.VMMDir == "" {
		return nil, nil //nolint:nilnil // Fixed-binary mode has no managed selection.
	}
	raw, err := os.ReadFile(cachedManifestPath(d.config.StateRoot, image))
	if err != nil {
		return nil, err
	}
	manifest, err := artifacts.DecodeVMManifest(raw)
	if err != nil {
		return nil, err
	}
	return d.selectVMM(ctx, manifest.Compatibility.FirecrackerVersion)
}

func (d *Driver) preparedVMM(ctx context.Context, id string, manifest artifacts.VMManifest) (*firecrackervmm.Selection, error) {
	selected, err := d.selectVMM(ctx, manifest.Compatibility.FirecrackerVersion)
	if err != nil || selected == nil {
		return selected, err
	}
	raw, err := os.ReadFile(filepath.Join(d.ociRoot(), "sandboxes", id, "state.json"))
	if err != nil {
		return nil, err
	}
	var state struct {
		VMM *firecrackervmm.Selection `json:"vmm"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.VMM == nil || state.VMM.BinaryPath != selected.BinaryPath || state.VMM.Version != selected.Version {
		return nil, fmt.Errorf("prepared OCI VMM selection disagrees with manifest")
	}
	return selected, nil
}

func (d *Driver) selectionVersion(selected *firecrackervmm.Selection) string {
	if selected != nil {
		return selected.Version
	}
	return d.firecrackerVersion()
}

// Managed releases are verified at delivery. Compare executable contents:
// jailer copies the selected executable into its chroot, producing a new inode.
// Operators need not maintain a second digest profile for managed VMMs.
func qualifyVMMExecutable(state *SandboxState, expectedDigest string) error {
	if state.VMM != nil {
		installed, err := captureFileHash(state.VMM.BinaryPath)
		if err != nil {
			return err
		}
		if expectedDigest != "" && expectedDigest != installed {
			return fmt.Errorf("managed VMM differs from explicit qualification digest")
		}
		expectedDigest = installed
	}
	if expectedDigest == "" {
		return fmt.Errorf("custom VMM requires firecrackerSHA256 qualification")
	}
	sum, err := captureFileHash(fmt.Sprintf("/proc/%d/exe", state.PID))
	if err != nil {
		return err
	}
	if sum != expectedDigest {
		return fmt.Errorf("VMM executable does not match qualified artifact")
	}
	return nil
}

func checkRootfsCaptureVersion(selected *firecrackervmm.Selection, version string) error {
	if selected != nil {
		if selected.Version != version || version != managedRootfsCaptureVersion {
			return fmt.Errorf("managed RootfsOnly capture requires Firecracker 1.17.0; running %s", version)
		}
		return nil
	}
	// Preserve explicitly qualified custom 1.16.1 deployments, including the
	// historical vsock fix backport; stock managed 1.16.1 must never capture.
	if version != "1.16.1" && version != managedRootfsCaptureVersion {
		return fmt.Errorf("unsupported RootfsOnly capture version %s", version)
	}
	return nil
}
