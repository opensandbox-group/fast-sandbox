package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fast-sandbox/internal/artifacts"
)

// createOptions holds the create-command flags (ADR-007/009/010). The machine
// sizing is fixed by the snapshot, so create takes no vCPU/memory flags.
type createOptions struct {
	prepareOnly  bool
	templateFile string
	kernel       string
	network      string
	force        bool
}

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
	flags.BoolVar(&options.prepareOnly, "prepare-only", false, "attach template disks for an external Firecracker lifecycle owner")
	flags.StringVarP(&options.templateFile, "file", "f", "", "path to template.json (required)")
	flags.StringVar(&options.kernel, "kernel", "", "override the resolved vmlinux path")
	flags.StringVar(&options.network, "network", networkNone, "network provider (only 'none' is supported)")
	flags.BoolVar(&options.force, "force", false, "downgrade version-compatibility errors to warnings")
	_ = command.MarkFlagRequired("file")
	return command
}

// rollback accumulates compensating actions executed in reverse order when
// the build flow fails partway. Steps are pushed in dependency order (a
// directory before the devices, mounts and VMM backed by its files), so the
// first step that fails stops the rollback: every earlier step guards a
// resource the failed one may still use, such as the config and upper of a
// live device or a local lower blob. Those are kept for a retry and listed
// in the returned error.
type rollback struct {
	steps []rollbackStep
}

type rollbackStep struct {
	describe func() string
	undo     func() error
}

func (r *rollback) push(resource string, undo func() error) {
	r.pushDescribed(func() string { return resource }, undo)
}

// pushDescribed registers a step whose resource description is rendered
// only when the rollback reports it (a reattached device changes its id).
func (r *rollback) pushDescribed(describe func() string, undo func() error) {
	r.steps = append(r.steps, rollbackStep{describe: describe, undo: undo})
}

func (r *rollback) run() error {
	steps := r.steps
	r.steps = nil
	for index := len(steps) - 1; index >= 0; index-- {
		if err := steps[index].undo(); err != nil {
			kept := make([]string, 0, index+1)
			for _, step := range steps[:index+1] {
				kept = append(kept, step.describe())
			}
			return fmt.Errorf("rollback stopped at %s: %w; kept for retry: %s",
				steps[index].describe(), err, strings.Join(kept, ", "))
		}
	}
	return nil
}

func (r *rollback) disarm() { r.steps = nil }

func runCreate(ctx context.Context, options *createOptions) error { //nolint:gocognit // ordered device transaction keeps rollback ownership visible
	provider, err := selectNetworkProvider(options.network)
	if err != nil {
		return err
	}
	template, err := loadTemplate(options.templateFile)
	if err != nil {
		return err
	}
	dockerAuth, err := parseDockerConfig(template.Template.Auth.DockerAuth)
	if err != nil {
		return err
	}

	// Startup reachability check: the daemon is pre-deployed via systemd; the
	// CLI never launches it (ADR-002).
	ublkd := newUblkdClient(global.ublkdSocket)
	if err := ublkd.Ping(ctx); err != nil {
		return err
	}
	if err := resolveTemplate(ctx, &template, dockerAuth); err != nil {
		return err
	}

	release, err := lockSandbox(template.SandboxID)
	if err != nil {
		return err
	}
	defer release()

	layout := newSandboxLayout(template.SandboxID)
	if err := claimSandbox(layout, template.SandboxID); err != nil {
		return err
	}
	state := &sandboxState{
		SandboxID:        template.SandboxID,
		Phase:            phaseCreating,
		RootfsRef:        template.rootfsRef(),
		SnapfilesRef:     template.snapfilesRef(),
		TemplateMetadata: template.Metadata,
		Network:          provider.Name(),
		MountPoint:       layout.mountPoint,
		APISocket:        layout.apiSocket,
		SerialLog:        layout.serialLog,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	// The creating record is the first durable write: from here on a crash
	// leaves state.json behind, and delete reclaims whatever was allocated.
	if err := writeState(layout.home, state); err != nil {
		return errors.Join(err, removeSandboxDirs(layout))
	}

	created := false
	defer func() {
		if created {
			return
		}
		if err := releaseSandbox(context.Background(), ublkd, layout, state); err != nil {
			fmt.Fprintf(os.Stderr, "warning: rollback of sandbox %s is incomplete; state kept in %s, retry with `delete %s`: %v\n",
				template.SandboxID, layout.home, template.SandboxID, err)
			return
		}
		if err := removeSandboxDirs(layout); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		if err := pruneSharedSnapfiles(context.Background(), ublkd, false); err != nil {
			fmt.Fprintf(os.Stderr, "warning: prune shared snapfiles: %v\n", err)
		}
	}()
	persist := func() error { return writeState(layout.home, state) }

	// Synchronize credentials before resolving manifests, including failed creates.
	// Step 3: hand the credential to overlaybd's global cred.json before any
	// device add, or overlaybd's on-demand pulls fail with 401.
	handoff, err := overlaybdCredentials(dockerAuth, state.RootfsRef, state.SnapfilesRef)
	if err != nil {
		return err
	}
	if err := mergeCredentials(global.credFile, handoff); err != nil {
		return err
	}

	// Step 2: resolve both manifests into ordered lower-layer lists.
	rootfsImage, err := resolveImage(ctx, state.RootfsRef, dockerAuth)
	if err != nil {
		return err
	}
	snapfilesImage, err := resolveImage(ctx, state.SnapfilesRef, dockerAuth)
	if err != nil {
		return err
	}

	// Step 4/5: the snapfiles device is attached and mounted first (a minor
	// reordering of the ADR's illustrative sequence) because manifest.json —
	// which lives on it — supplies the disk size that sets the rootfs hybrid
	// upper's virtual size. Attaching snapfiles read-only carries no such
	// dependency, so it is the safe first step.
	if err := acquireSharedSnapfiles(ctx, ublkd, layout.home, state, snapfilesImage, dockerAuth); err != nil {
		return err
	}
	layout.mountPoint = state.MountPoint

	metadata, err := loadManifest(filepath.Join(layout.mountPoint, inDiskManifest))
	if err != nil {
		return err
	}
	if err := validateManifest(metadata, global.firecrackerBin, options.force); err != nil {
		return err
	}
	state.VCPU, state.MemoryMB = metadata.Machine.VCPUCount, int(metadata.Machine.MemoryMiB)

	// rootfs device: create the hybrid writable upper sized from the
	// authoritative disk size, then attach it.
	rootfsConfig, err := writeRootfsConfig(ctx, layout.home, rootfsImage, int(metadata.Machine.RootfsSizeMiB))
	if err != nil {
		return err
	}
	rootfsDevPath, err := addSandboxDevice(ctx, ublkd, rootfsConfig, &state.Rootfs, persist)
	if err != nil {
		return err
	}

	if options.prepareOnly {
		created = true
		return nil
	}
	// Step 6: resolve the kernel path (ADR-004). Firecracker snapshot restore
	// carries the guest kernel in the memory state, but the ADR requires the
	// host-local vmlinux to be present and version-matched.
	if _, err := resolveKernel(global.kernelDir, metadata.Kernel.Version, options.kernel); err != nil {
		return err
	}

	// Step 7: point the snapshot's relative rootfs drive path at this host's
	// device, launch Firecracker inside the runtime directory and resume.
	if _, err := linkRootfsDrive(layout.runtime, rootfsDevPath); err != nil {
		return err
	}
	process, err := launchFirecracker(global.firecrackerBin, layout.runtime, layout.apiSocket, template.SandboxID, layout.serialLog)
	if err != nil {
		return err
	}
	state.Firecracker = &process
	if err := persist(); err != nil {
		return err
	}
	if err := resumeVM(ctx, layout, metadata, provider); err != nil {
		return err
	}

	// Step 8: mark the sandbox running and report.
	state.Phase = phaseRunning
	if err := persist(); err != nil {
		return err
	}
	created = true
	printSandbox(state)
	return nil
}

// claimSandbox atomically takes ownership of the sandbox id: os.Mkdir fails
// if the home already exists, so two creates can never share (and later roll
// back) the same directories even without the operation lock.
func claimSandbox(layout sandboxLayout, sandboxID string) error {
	if err := os.MkdirAll(filepath.Dir(layout.home), 0o750); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Mkdir(layout.home, 0o750); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("sandbox %q already exists at %s", sandboxID, layout.home)
		}
		return fmt.Errorf("create sandbox home %s: %w", layout.home, err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.runtime), 0o750); err != nil {
		_ = os.Remove(layout.home)
		return fmt.Errorf("create runtime directory root: %w", err)
	}
	if err := os.Mkdir(layout.runtime, 0o750); err != nil {
		_ = os.Remove(layout.home)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("sandbox %q has a leftover runtime directory %s; run `delete %s` first", sandboxID, layout.runtime, sandboxID)
		}
		return fmt.Errorf("create runtime directory %s: %w", layout.runtime, err)
	}
	return nil
}

// addSandboxDevice attaches config through ublkd, recording the device in
// *record before the add (so a crash mid-add leaves a trace) and completing
// the record once ublkd replies. It returns the /dev/ublkbN path.
func addSandboxDevice(ctx context.Context, ublkd *ublkdClient, config string, record **deviceRecord, persist func() error) (string, error) {
	*record = &deviceRecord{Config: config, DevID: -1}
	if err := persist(); err != nil {
		return "", err
	}
	devID, devPath, err := ublkd.Add(ctx, config)
	if err != nil {
		(*record).addOutcomeUnknown = errors.Is(err, errAddOutcomeUnknown)
		return "", err
	}
	(*record).DevID, (*record).DevPath = devID, devPath
	return devPath, persist()
}

// resumeVM drives the Firecracker restore sequence: LoadSnapshot must be the
// first API call (v1.16 rejects prior device/machine config) and opens the
// rootfs drive through the relative rootfs.dev link; the network is then
// acquired and the VM resumed (ADR create flow §7).
func resumeVM(ctx context.Context, layout sandboxLayout, metadata artifacts.VMManifest, provider networkProvider) error {
	client := newFirecrackerClient(layout.apiSocket)
	defer client.close()

	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.waitReady(readyCtx); err != nil {
		return err
	}

	if err := client.loadSnapshot(ctx, snapshotLoadRequest{
		SnapshotPath: filepath.Join(layout.mountPoint, inDiskVMState),
		MemBackend: snapshotMemBackend{
			BackendType: "File",
			BackendPath: filepath.Join(layout.mountPoint, inDiskMemfile),
		},
		ResumeVM: false,
	}); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
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

// resolveKernel maps manifest.kernel.version to a host-local vmlinux path
// (ADR-004). An explicit override wins; a missing file is a hard error.
func resolveKernel(kernelDir, kernelVersion, override string) (string, error) {
	path := override
	if path == "" {
		name := "vmlinux-" + kernelVersion
		// The standard Builder records its unversioned default filename.
		if kernelVersion == "vmlinux.bin" {
			name = kernelVersion
		}
		path = filepath.Join(kernelDir, name)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("kernel %s not found (kernel.version %q): %w", path, kernelVersion, err)
	}
	return path, nil
}

// printSandbox reports the created sandbox on stdout.
func printSandbox(state *sandboxState) {
	fmt.Printf("sandbox %s created\n", state.SandboxID)
	fmt.Printf("  firecracker pid : %d\n", state.Firecracker.PID)
	fmt.Printf("  api socket      : %s\n", state.APISocket)
	fmt.Printf("  rootfs device   : %s (dev_id %d)\n", state.Rootfs.DevPath, state.Rootfs.DevID)
	if state.SharedSnapfiles != "" {
		fmt.Printf("  snapfiles shared: %s\n", state.MountPoint)
	} else {
		fmt.Printf("  snapfiles device: %s (dev_id %d)\n", state.Snapfiles.DevPath, state.Snapfiles.DevID)
	}
	fmt.Printf("  vcpu / memory   : %d / %d MiB\n", state.VCPU, state.MemoryMB)
	fmt.Printf("  network         : %s\n", state.Network)
}
