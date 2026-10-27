package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
)

const ociEnabled = "true"

func validateOCISupport(image string) error {
	if !strings.HasPrefix(image, ocitemplate.Prefix) {
		return nil
	}
	switch os.Getenv("FAST_SANDBOX_OCI_ENABLED") {
	case ociEnabled:
		return nil
	case "", "false":
		return fmt.Errorf("%w: OCI templates and checkpoints require FAST_SANDBOX_OCI_ENABLED=true", ErrInvalidConfig)
	default:
		return fmt.Errorf("%w: FAST_SANDBOX_OCI_ENABLED must be true or false", ErrInvalidConfig)
	}
}

func validateOCIReference(image string) error {
	ref, err := registry.ParseReference(strings.TrimPrefix(image, ocitemplate.Prefix))
	if err != nil || ref.Reference == "" {
		return fmt.Errorf("%w: OCI template requires a registry reference with a tag or digest", ErrInvalidConfig)
	}
	return nil
}

// ociRoot separates Pod-private backing files while the daemon/cache are shared.
func (d *Driver) ociRoot() string {
	return ociRootForPod(d.config.StateRoot, d.podUID)
}

func ociRootForPod(stateRoot, podUID string) string {
	root := filepath.Join(stateRoot, "oci")
	if podUID != "" {
		root = filepath.Join(root, "pods", podUID)
	}
	return root
}

func (d *Driver) fsbArgs() []string {
	tools := filepath.Join(runtimecatalog.OCIToolsDir, "current", "bin")
	if resolved, err := filepath.EvalSymlinks(tools); err == nil {
		tools = resolved
	}
	return []string{
		"--state-dir", filepath.Join(d.ociRoot(), "sandboxes"),
		"--manifest-cache-dir", filepath.Join(d.config.StateRoot, "oci/manifest-cache"),
		"--runtime-dir", filepath.Join(d.ociRoot(), "runtime"),
		"--build-work-dir", filepath.Join(d.ociRoot(), "builds"),
		"--build-output-dir", filepath.Join(d.ociRoot(), "artifacts"),
		"--firecracker", d.config.BinaryPath,
		"--ublkd-socket", filepath.Join(runtimecatalog.OCISocketDir, "ublkd.sock"),
		"--cred-file", filepath.Join(runtimecatalog.OCISocketDir, "cred.json"),
		"--reject-credential-conflicts",
		"--overlaybd-create", filepath.Join(tools, "overlaybd-create"),
		"--overlaybd-commit", filepath.Join(tools, "overlaybd-commit"),
		"--overlaybd-resize", filepath.Join(tools, "overlaybd-resize"),
	}
}
func (d *Driver) attachOCI(ctx context.Context, id, image string) (rootfs, vmstate, memory string, resultErr error) {
	if err := validateOCIReference(image); err != nil {
		return "", "", "", err
	}
	registryPath := os.Getenv("FAST_SANDBOX_REGISTRY_CONFIG_PATH")
	if registryPath == "" {
		registryPath = registryconfig.MountPath
	}
	credential, _, err := registryconfig.NewFileProvider(registryPath).Credentials(strings.TrimPrefix(image, ocitemplate.Prefix))
	if err != nil {
		return "", "", "", err
	}
	if _, err := ocitemplate.BasicCredential(credential.Username, credential.Password, credential.IdentityToken); err != nil {
		return "", "", "", fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	key := credential.Host
	if credential.RepositoryPrefix != "" {
		key += "/" + strings.Trim(credential.RepositoryPrefix, "/")
	}
	auth, err := json.Marshal(map[string]any{"auths": map[string]any{key: map[string]string{"username": credential.Username, "password": credential.Password}}})
	if err != nil {
		return "", "", "", err
	}
	document := map[string]any{"sandbox_id": id, "template": map[string]any{"reference": image, "auth": map[string]string{"dockerAuth": string(auth)}}}
	raw, err := json.Marshal(document)
	if err != nil {
		return "", "", "", err
	}
	dir := filepath.Join(d.ociRoot(), "inputs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", "", err
	}
	file := filepath.Join(dir, id+".json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		return "", "", "", err
	}
	defer os.Remove(file)
	home := filepath.Join(d.ociRoot(), "sandboxes", id)
	if _, err = os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		return "", "", "", fmt.Errorf("OCI sandbox home already exists or is inaccessible: %s", home)
	}
	// fsb-vm rolls back normal failures. Retry teardown if it retained resources,
	// or if attachment succeeded but reading the resulting state fails.
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, d.releaseOCI(context.Background(), id))
		}
	}()
	args := append(d.fsbArgs(), "create", "--prepare-only", "-f", file)
	output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput()
	if err != nil {
		return "", "", "", fmt.Errorf("attach OCI template: %w: %s", err, output)
	}
	var state struct {
		Rootfs struct {
			Path string `json:"dev_path"`
		} `json:"rootfs_device"`
	}
	raw, err = os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		return "", "", "", err
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return "", "", "", err
	}
	if state.Rootfs.Path == "" {
		return "", "", "", fmt.Errorf("OCI attachment state has no rootfs device")
	}
	return state.Rootfs.Path, filepath.Join(home, "mnt/vmstate.bin"), filepath.Join(home, "mnt/memfile"), nil
}

// Platform metadata comes from the mounted snapfiles, never registry annotations.
type ociRestoreMetadata = artifacts.VMManifest

type ociAttachment struct {
	rootfs, vmstate, memory string
	metadata                ociRestoreMetadata
	duration                time.Duration
}

func (d *Driver) prepareOCIRestore(ctx context.Context, directory, reference string, config *fastletapi.RuntimeSandboxConfig) (attachment ociAttachment, resultErr error) {
	started := time.Now()
	ctx, span := observability.Start(ctx, "fastlet.firecracker.oci.prepare")
	defer func() {
		attachment.duration = time.Since(started)
		observability.End(span, resultErr)
	}()
	id := config.Identity.SandboxUID
	// Record ownership before CLI side effects for crash recovery.
	if err := saveState(directory, &SandboxState{Config: *config, Phase: PhaseStarting}); err != nil {
		return attachment, err
	}
	var err error
	attachment.rootfs, attachment.vmstate, attachment.memory, err = d.attachOCI(ctx, id, reference)
	if err != nil {
		return attachment, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, d.releaseOCI(context.Background(), id))
		}
	}()
	attachment.metadata, err = readPreparedOCIMetadata(d.ociRoot(), id)
	if err != nil {
		return attachment, err
	}
	metadata := attachment.metadata
	if err := validateOCIResumeOwner(metadata, config); err != nil {
		return attachment, err
	}
	hasMachine := metadata.Machine.VCPUCount > 0 && metadata.Machine.MemoryMiB > 0
	if err := validateRestoreMachine(config.Limits, d.config, metadata.Machine, hasMachine); err != nil {
		return attachment, err
	}
	if err := checkRestoreCompatibility(metadata.Compatibility, artifacts.HostCPUIdentity(), d.firecrackerVersion(), reference); err != nil {
		return attachment, err
	}
	return attachment, nil
}

func readPreparedOCIMetadata(ociRoot, id string) (ociRestoreMetadata, error) {
	var metadata ociRestoreMetadata
	raw, err := readPreparedOCIManifest(ociRoot, id)
	if err != nil {
		return metadata, err
	}
	metadata, err = artifacts.DecodeVMManifest(raw)
	if err != nil {
		return metadata, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := requireStaticGuestNetwork(metadata.GuestNetwork); err != nil {
		return metadata, err
	}
	// Pair resolution authenticates checkpoint ownership through the OCI
	// manifest. The embedded platform manifest must agree before admission.
	stateRaw, err := os.ReadFile(filepath.Join(ociRoot, "sandboxes", id, "state.json"))
	if err != nil {
		return metadata, err
	}
	var state struct {
		Metadata json.RawMessage `json:"template_metadata"`
	}
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		return metadata, err
	}
	pair, err := ocitemplate.Decode(state.Metadata)
	if err != nil || pair == nil {
		return metadata, fmt.Errorf("%w: fsb-vm prepared state has no valid template pair", ErrInvalidConfig)
	}
	var envelope struct {
		Checkpoint *artifacts.CheckpointOwner `json:"checkpoint"`
	}
	if err := json.Unmarshal(state.Metadata, &envelope); err != nil {
		return metadata, err
	}
	owner := envelope.Checkpoint
	var embedded *artifacts.CheckpointOwner
	if metadata.Platform != nil {
		embedded = metadata.Platform.Checkpoint
	}
	if (owner == nil) != (embedded == nil) || (owner != nil && *owner != *embedded) {
		return metadata, fmt.Errorf("%w: snapfiles checkpoint owner disagrees with OCI manifest", ErrInvalidConfig)
	}
	return metadata, nil
}

func readPreparedOCIManifest(ociRoot, id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(ociRoot, "sandboxes", id, "mnt", snapshotManifestName))
}
func (d *Driver) releaseOCI(ctx context.Context, id string) error {
	home := filepath.Join(d.ociRoot(), "sandboxes", id)
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	args := append(d.fsbArgs(), "delete", id)
	output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("release OCI template devices: %w: %s", err, output)
	}
	return nil
}
func (d *Driver) prepareOCIInstance(id, directory, rootfs, vmstate, memory string) (string, string, string, error) {
	if d.config.JailerPath == "" {
		if err := os.Symlink(rootfs, filepath.Join(directory, "rootfs.dev")); err != nil {
			return "", "", "", err
		}
		return rootfs, "", filepath.Join(directory, "api.sock"), nil
	}
	root := jailerRoot(filepath.Join(d.config.StateRoot, jailerChrootBaseDir), filepath.Base(d.config.BinaryPath), truncatedSandboxID(id))
	if err := os.MkdirAll(filepath.Join(root, jailerChrootSnapshotsDir), 0750); err != nil {
		return "", "", "", err
	}
	for _, entry := range []struct{ source, target string }{{rootfs, "rootfs.dev"}, {vmstate, filepath.Join(jailerChrootSnapshotsDir, vmstateSnapshotName)}, {memory, filepath.Join(jailerChrootSnapshotsDir, memorySnapshotName)}} {
		target := filepath.Join(root, entry.target)
		file, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0640)
		if err != nil {
			return "", "", "", err
		}
		file.Close()
		if err = bindMount(entry.source, target); err != nil {
			return "", "", "", err
		}
	}
	d.prepareSnapshotSpill(root)
	return filepath.Join(root, "rootfs.dev"), root, filepath.Join(root, "api.sock"), nil
}
