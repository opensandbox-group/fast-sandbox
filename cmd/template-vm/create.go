package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// createOptions holds the create-command flags (ADR-007/009/010).
type createOptions struct {
	templateFile  string
	kernel        string
	vcpu          int
	memoryMB      int
	network       string
	rootfsDriveID string
	force         bool
}

// defaultRootfsDriveID is the drive_id the rootfs device is registered under
// in the template snapshot; it must match the vmstate (ADR create flow §7).
const defaultRootfsDriveID = "rootfs"

func newCreateCommand() *cobra.Command {
	options := &createOptions{}
	command := &cobra.Command{
		Use:   "create -f template.json",
		Short: "Restore and start a microVM from a remote template",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreate(cmd.Context(), options)
		},
	}
	flags := command.Flags()
	flags.StringVarP(&options.templateFile, "file", "f", "", "path to template.json (required)")
	flags.StringVar(&options.kernel, "kernel", "", "override the resolved vmlinux path")
	flags.IntVar(&options.vcpu, "vcpu", 0, "override metadata vcpu_count")
	flags.IntVar(&options.memoryMB, "memory-mb", 0, "override metadata memory_mb")
	flags.StringVar(&options.network, "network", networkNone, "network provider (only 'none' is supported)")
	flags.StringVar(&options.rootfsDriveID, "rootfs-drive-id", defaultRootfsDriveID, "rootfs drive_id baked in the snapshot vmstate")
	flags.BoolVar(&options.force, "force", false, "downgrade version-compatibility errors to warnings")
	_ = command.MarkFlagRequired("file")
	return command
}

// rollback accumulates compensating actions executed in reverse order when
// the create flow fails partway (ADR: failures roll back in reverse).
type rollback struct {
	steps []func()
}

func (r *rollback) push(step func()) { r.steps = append(r.steps, step) }

func (r *rollback) run() {
	for index := len(r.steps) - 1; index >= 0; index-- {
		r.steps[index]()
	}
}

func (r *rollback) disarm() { r.steps = nil }

func runCreate(ctx context.Context, options *createOptions) error {
	provider, err := selectNetworkProvider(options.network)
	if err != nil {
		return err
	}
	template, err := loadTemplate(options.templateFile)
	if err != nil {
		return err
	}

	home := global.sandboxHome(template.SandboxID)
	if _, err := os.Stat(home); err == nil {
		return fmt.Errorf("sandbox %q already exists at %s", template.SandboxID, home)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat sandbox home %s: %w", home, err)
	}
	runtimeDir := global.sandboxRuntime(template.SandboxID)

	// Startup reachability check: the daemon is pre-deployed via systemd; the
	// CLI never launches it (ADR-002).
	ublkd := newUblkdClient(global.ublkdSocket)
	if err := ublkd.Ping(ctx); err != nil {
		return err
	}

	dockerAuth, err := parseDockerConfig(template.Template.Auth.DockerAuth)
	if err != nil {
		return err
	}

	// Step 2: resolve both manifests into ordered lower-layer lists.
	rootfsImage, err := resolveImage(ctx, template.rootfsRef(), dockerAuth)
	if err != nil {
		return err
	}
	snapfilesImage, err := resolveImage(ctx, template.snapfilesRef(), dockerAuth)
	if err != nil {
		return err
	}

	// Step 3: merge the credential into overlaybd's global cred.json before
	// any device add, or overlaybd's on-demand pulls fail with 401.
	if err := mergeCredentials(global.credFile, dockerAuth); err != nil {
		return err
	}

	if err := os.MkdirAll(home, 0o750); err != nil {
		return fmt.Errorf("create sandbox home %s: %w", home, err)
	}
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		return fmt.Errorf("create runtime directory %s: %w", runtimeDir, err)
	}

	undo := &rollback{}
	undo.push(func() { _ = os.RemoveAll(home); _ = os.RemoveAll(runtimeDir) })
	defer undo.run()

	state := sandboxState{
		SandboxID:     template.SandboxID,
		RootfsRef:     template.rootfsRef(),
		SnapfilesRef:  template.snapfilesRef(),
		RootfsDriveID: options.rootfsDriveID,
		Network:       provider.Name(),
		MountPoint:    filepath.Join(home, snapfilesMount),
		APISocket:     filepath.Join(runtimeDir, apiSocketName),
		SerialLog:     filepath.Join(runtimeDir, serialLogName),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	// Step 4/5: the snapfiles device is attached and mounted first (a minor
	// reordering of the ADR's illustrative sequence) because metadata.json —
	// which lives on it — supplies the disk size that sets the rootfs hybrid
	// upper's virtual size. Attaching snapfiles read-only carries no such
	// dependency, so it is the safe first step.
	snapfilesConfig, err := writeSnapfilesConfig(home, snapfilesImage)
	if err != nil {
		return err
	}
	snapfilesDevID, snapfilesDevPath, err := ublkd.Add(ctx, snapfilesConfig)
	if err != nil {
		return err
	}
	state.SnapfilesDevID, state.SnapfilesDevPath = snapfilesDevID, snapfilesDevPath
	undo.push(func() { _ = ublkd.Del(context.Background(), snapfilesDevID) })

	if err := mountReadOnly(snapfilesDevPath, state.MountPoint); err != nil {
		return err
	}
	undo.push(func() { _ = unmount(state.MountPoint) })

	metadata, err := loadMetadata(filepath.Join(state.MountPoint, inDiskMetadata))
	if err != nil {
		return err
	}
	if err := metadata.validate(global.firecrackerBin, options.force); err != nil {
		return err
	}
	spec := metadata.resolveSpec(specOverrides{vcpu: options.vcpu, memoryMB: options.memoryMB})
	state.VCPU, state.MemoryMB = spec.vcpu, spec.memoryMB

	// rootfs device: create the hybrid writable upper sized from the
	// authoritative disk size, then attach it.
	rootfsConfig, err := writeRootfsConfig(ctx, home, rootfsImage, metadata.DiskSizeMB)
	if err != nil {
		return err
	}
	rootfsDevID, rootfsDevPath, err := ublkd.Add(ctx, rootfsConfig)
	if err != nil {
		return err
	}
	state.RootfsDevID, state.RootfsDevPath = rootfsDevID, rootfsDevPath
	undo.push(func() { _ = ublkd.Del(context.Background(), rootfsDevID) })

	// Step 6: resolve the kernel path (ADR-004). Firecracker snapshot restore
	// carries the guest kernel in the memory state, but the ADR requires the
	// host-local vmlinux to be present and version-matched.
	if _, err := resolveKernel(global.kernelDir, metadata.KernelVersion, options.kernel); err != nil {
		return err
	}

	// Step 7: launch Firecracker and resume from the in-disk snapshot.
	pid, err := launchFirecracker(global.firecrackerBin, state.APISocket, template.SandboxID, state.SerialLog)
	if err != nil {
		return err
	}
	state.FirecrackerPID = pid
	undo.push(func() { _ = killProcess(pid) })

	if err := resumeVM(ctx, state, metadata, provider, options.rootfsDriveID); err != nil {
		return err
	}

	// Step 8: persist state and report.
	if err := writeState(home, state); err != nil {
		return err
	}
	undo.disarm()
	printSandbox(state)
	return nil
}

// resumeVM drives the Firecracker restore sequence: LoadSnapshot must be the
// first API call (v1.16 rejects prior device/machine config); the restored
// rootfs drive is then redirected to the freshly attached ublk device, the
// network is acquired, and the VM is resumed (ADR create flow §7).
func resumeVM(ctx context.Context, state sandboxState, metadata vmMetadata, provider networkProvider, rootfsDriveID string) error {
	client := newFirecrackerClient(state.APISocket)
	defer client.close()

	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.waitReady(readyCtx); err != nil {
		return err
	}

	if err := client.loadSnapshot(ctx, snapshotLoadRequest{
		SnapshotPath: filepath.Join(state.MountPoint, inDiskVMState),
		MemBackend: snapshotMemBackend{
			BackendType: "File",
			BackendPath: filepath.Join(state.MountPoint, inDiskMemfile),
		},
		ResumeVM: false,
	}); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	if err := client.updateDrive(ctx, rootfsDriveID, state.RootfsDevPath); err != nil {
		return fmt.Errorf("redirect rootfs drive %q to %s: %w", rootfsDriveID, state.RootfsDevPath, err)
	}

	if _, cleanup, err := provider.Acquire(ctx, metadata); err != nil {
		return fmt.Errorf("acquire network: %w", err)
	} else {
		// The cleanup callback is a no-op for the none provider; teardown of
		// a real device is owned by delete once the networking integration
		// lands (ADR-010).
		_ = cleanup
	}

	if err := client.resume(ctx); err != nil {
		return fmt.Errorf("resume VM: %w", err)
	}
	return nil
}

// resolveKernel maps metadata.kernel_version to a host-local vmlinux path
// (ADR-004). An explicit override wins; a missing file is a hard error.
func resolveKernel(kernelDir, kernelVersion, override string) (string, error) {
	path := override
	if path == "" {
		path = filepath.Join(kernelDir, "vmlinux-"+kernelVersion)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("kernel %s not found (kernel_version %q): %w", path, kernelVersion, err)
	}
	return path, nil
}

// printSandbox reports the created sandbox on stdout.
func printSandbox(state sandboxState) {
	fmt.Printf("sandbox %s created\n", state.SandboxID)
	fmt.Printf("  firecracker pid : %d\n", state.FirecrackerPID)
	fmt.Printf("  api socket      : %s\n", state.APISocket)
	fmt.Printf("  rootfs device   : %s (dev_id %d)\n", state.RootfsDevPath, state.RootfsDevID)
	fmt.Printf("  snapfiles device: %s (dev_id %d)\n", state.SnapfilesDevPath, state.SnapfilesDevID)
	fmt.Printf("  network         : %s\n", state.Network)
}
