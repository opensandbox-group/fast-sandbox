package firecracker

// snapshot_driver.go implements the optional Snapshotter contract
// (runtimecontract.Snapshotter) for the live firecracker driver: pause the
// running microVM, dump the writable instance rootfs plus the vmstate/memory
// pair, resume the VM on every path, assemble a restore-compatible manifest
// (byte-format shared with the golden-image builder via internal/artifacts),
// and publish the set through the node runtime-agent. Local mode (no agent)
// refuses snapshots: publication requires the agent's store credentials.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"fast-sandbox/internal/artifacts"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	"k8s.io/klog/v2"
)

// snapshotStagingDir holds per-snapshot staging directories:
//
//	<StateRoot>/snapshots/<snapshotID>/{rootfs.ext4, vmstate.snap, memory.snap, manifest.json, SHA256SUMS}
const snapshotStagingDir = "snapshots"

// publishedRootfsName is the rootfs file name in a published artifact set.
// The instance drive file is rootfs.img; consumers (the agent pull layer)
// rename it back on arrival, exactly as for builder-produced sets.
const publishedRootfsName = "rootfs.ext4"

// jailerSnapshotDumpDir is the directory (inside the jail root) the jailed
// VMM dumps its snapshot files into: a chrooted VMM cannot write outside its
// jail, so the driver addresses the dump files CHROOT-RELATIVELY and moves
// them into the staging directory afterwards. The dump uses dedicated file
// names: the same directory also holds the hard-linked restore snapshots,
// and truncating those through the shared inodes would corrupt the cache.
const (
	jailerSnapshotDumpDir = "snapshots"
	jailedDumpVMStateName = "dump-vmstate.snap"
	jailedDumpMemoryName  = "dump-memory.snap"
	// jailerSpillDirName is the chroot-relative mount point of the snapshot
	// spill root inside a jailed VMM (bind-mounted at instance creation —
	// the jailer clones its mount namespace at process start, so a dump
	// window bind would never be visible to the running VMM).
	jailerSpillDirName = "spill"
)

// snapshotManifestName is the commit-point document of the artifact set.
const snapshotManifestName = "manifest.json"

// snapshotSpillDirEnv names a fast local staging area (tmpfs, an emptyDir
// with medium: Memory, or a local NVMe path) mounted into the fastlet. When
// set (and roomy enough), the paused VMM dumps vmstate/memory THERE and the
// driver moves the files to the publish staging after the resume — the
// memory dump then pays tmpfs bandwidth instead of the (often
// network-backed) StateRoot filesystem, shrinking the business-visible
// pause window from seconds to sub-second. Empty (default) keeps the
// legacy direct-to-staging dump.
const snapshotSpillDirEnv = "FAST_SANDBOX_SNAPSHOT_SPILL_DIR"

// Driver implements the optional snapshot extension.
var _ runtimecontract.Snapshotter = (*Driver)(nil)

// snapshotMu serializes the pause/dump/resume window across all Sandboxes of
// this driver: only one VM on the node is ever paused at a time. Manifest
// assembly and publication run outside the lock.
var snapshotMu sync.Mutex

// CreateSnapshot snapshots a running Sandbox in place and publishes the
// artifact set under the template name. The VM is always resumed; a failed
// dump leaves the Sandbox running and discards the staging directory without
// publishing anything.
func (d *Driver) CreateSnapshot(ctx context.Context, input *runtimecontract.SnapshotInput) (*runtimecontract.SnapshotResult, error) {
	if input == nil || input.SandboxID == "" || input.SnapshotID == "" || input.TemplateName == "" {
		return nil, fmt.Errorf("%w: sandboxId, snapshotId, and templateName are required", ErrInvalidConfig)
	}
	if err := validateSandboxID(input.SnapshotID); err != nil {
		return nil, fmt.Errorf("%w: invalid snapshot id %q", ErrInvalidConfig, input.SnapshotID)
	}
	d.mu.RLock()
	plan := dumpPlan{
		stateRoot:   d.config.StateRoot,
		binaryName:  filepath.Base(d.config.BinaryPath),
		sandboxID:   input.SandboxID,
		snapshotID:  input.SnapshotID,
		jailed:      d.config.JailerPath != "",
		bootTimeout: d.config.BootTimeoutSeconds,
		spillRoot:   d.snapshotSpillRoot(),
	}
	firecrackerBinary := d.config.BinaryPath
	d.mu.RUnlock()
	plan.sandboxDir = filepath.Join(plan.stateRoot, sandboxStateDir, plan.sandboxID)
	plan.staging = filepath.Join(plan.stateRoot, snapshotStagingDir, plan.snapshotID)
	if err := os.RemoveAll(plan.staging); err != nil {
		return nil, fmt.Errorf("clear stale snapshot staging %s: %w", plan.staging, err)
	}

	// Capacity gate BEFORE any pause: a dump that cannot land wastes the
	// business interruption. Self-heal once through the image cache GC.
	if err := d.ensureDumpCapacity(plan); err != nil {
		return nil, err
	}

	dumpErr := d.dumpRunningSandbox(ctx, &plan)
	if dumpErr != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, dumpErr
	}
	klog.InfoS("firecracker sandbox dumped",
		"sandboxId", plan.sandboxID, "snapshotId", plan.snapshotID,
		"pauseWindow", plan.pauseWindow.String(), "spillMove", plan.spillMove.String(),
		"spilled", plan.spilled)

	sizeBytes, err := assembleSnapshotManifest(plan.stateRoot, plan.staging, plan.sandboxDir, firecrackerBinary, input.ActionBindings)
	if err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}

	client, err := d.agentClientOrNil()
	if err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}
	if client == nil {
		_ = os.RemoveAll(plan.staging)
		return nil, fmt.Errorf("%w: live snapshots require the firecracker runtime-agent for artifact publication", ErrInvalidConfig)
	}
	// The pause window is closed and the staged set is complete: report
	// Publishing before the (potentially long) upload — the caller may
	// admit the next snapshot of this Sandbox from here on.
	if input.OnPublishing != nil {
		input.OnPublishing()
	}
	publishStarted := time.Now()
	outcome, publishErr := client.PublishImage(ctx, "snapshot-"+plan.snapshotID, input.TemplateName, plan.staging)
	_ = os.RemoveAll(plan.staging)
	if publishErr != nil {
		return nil, fmt.Errorf("publish snapshot artifacts: %w", publishErr)
	}
	klog.InfoS("firecracker snapshot published",
		"sandboxId", plan.sandboxID, "snapshotId", plan.snapshotID, "templateName", input.TemplateName,
		"manifestRef", outcome.ManifestRef, "sizeBytes", sizeBytes, "publish", time.Since(publishStarted).String())
	return &runtimecontract.SnapshotResult{
		SnapshotID:     input.SnapshotID,
		ManifestRef:    outcome.ManifestRef,
		ArtifactDigest: outcome.ArtifactDigest,
		SizeBytes:      sizeBytes,
	}, nil
}

// DeleteSnapshot discards the staging directory of a snapshot. It never
// unpublishes stored objects. A missing directory is a successful no-op.
func (d *Driver) DeleteSnapshot(_ context.Context, snapshotID string) error {
	if err := validateSandboxID(snapshotID); err != nil {
		return fmt.Errorf("%w: invalid snapshot id %q", ErrInvalidConfig, snapshotID)
	}
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	staging := filepath.Join(stateRoot, snapshotStagingDir, snapshotID)
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove snapshot staging %s: %w", staging, err)
	}
	return nil
}

// defaultSnapshotCapacityWait bounds the best-effort GC + recheck window
// when the staging filesystem cannot hold the artifact set (a field so
// tests can shorten it).
const defaultSnapshotCapacityWait = 15 * time.Second

// ensureDumpCapacity verifies the staging filesystem can hold the full
// artifact set (instance rootfs + memory + margin), once after triggering
// the image-cache GC. It must run before the VM is paused: an ENOSPC
// mid-dump would burn the pause window for nothing. Returns the
// retryable ErrInsufficientStorage sentinel when space stays short.
func (d *Driver) ensureDumpCapacity(plan dumpPlan) error {
	need, err := d.dumpFootprintBytes(plan)
	if err != nil {
		// The dump itself will report a precise error; do not block on
		// unmeasurable inputs (e.g. a vanished state directory).
		return nil
	}
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	if stagingFree(stateRoot) >= need {
		return nil
	}
	d.TriggerImageGC()
	wait := d.snapshotCapacityWait
	if wait <= 0 {
		wait = defaultSnapshotCapacityWait
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if stagingFree(stateRoot) >= need {
			return nil
		}
	}
	free := stagingFree(stateRoot)
	klog.InfoS("snapshot staging space insufficient; parking the task",
		"stateRoot", stateRoot, "needBytes", need, "freeBytes", free)
	return fmt.Errorf("%w: staging needs %d bytes, %d free", runtimecontract.ErrInsufficientStorage, need, free)
}

// dumpFootprintBytes computes the full artifact set size: the instance
// rootfs logical size plus the guest memory with margin.
func (d *Driver) dumpFootprintBytes(plan dumpPlan) (int64, error) {
	state, err := loadState(plan.sandboxDir)
	if err != nil {
		return 0, err
	}
	if state.Config.Spec.Memory == "" {
		return 0, fmt.Errorf("sandbox spec carries no memory size")
	}
	mib, err := parseMemMiB(state.Config.Spec.Memory)
	if err != nil {
		return 0, err
	}
	rootfs := filepath.Join(plan.sandboxDir, instanceRootfsName)
	if plan.jailed {
		rootfs = filepath.Join(plan.jailRoot(), rootfsImageName)
	}
	info, err := d.stat(rootfs)
	if err != nil {
		return 0, err
	}
	// vmstate is small; the margin covers filesystem overhead.
	return info.Size() + int64(mib)<<20 + (64 << 20) + int64(mib)<<20/8, nil
}

// stagingFree reports the free bytes of the filesystem holding the
// snapshot staging directory (the StateRoot).
func stagingFree(stateRoot string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(stateRoot, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}

// dumpPlan carries one serialized pause/dump/resume execution. spilled
// reports whether the dump landed on the fast spill area; pauseWindow
// covers pause→resume (the business-visible interruption), spillMove the
// post-resume relocation into the publish staging.
type dumpPlan struct {
	stateRoot   string
	binaryName  string
	sandboxID   string
	snapshotID  string
	sandboxDir  string
	staging     string
	jailed      bool
	bootTimeout int32
	spillRoot   string
	spilled     bool
	pauseWindow time.Duration
	spillMove   time.Duration
}

// jailRoot returns the jail root of the plan's Sandbox (jailer mode only).
func (p dumpPlan) jailRoot() string {
	return jailerRoot(filepath.Join(p.stateRoot, jailerChrootBaseDir), p.binaryName, truncatedSandboxID(p.sandboxID))
}

// snapshotSpillRoot returns the configured spill root ("" disables
// spilling). It is read per call so tests can inject without racing the
// driver lock.
func (d *Driver) snapshotSpillRoot() string {
	return strings.TrimSpace(os.Getenv(snapshotSpillDirEnv))
}

// spillDirFor resolves the per-snapshot spill directory and applies the
// capacity guard. It returns "" when spilling is disabled or the area
// cannot hold the dump (the caller falls back to the legacy
// direct-to-staging dump). The guard needs the VM's memory size: unknown
// sizes fall back rather than risk a mid-dump ENOSPC.
func (d *Driver) spillDirFor(snapshotID, memoryQuantity string) string {
	root := d.snapshotSpillRoot()
	if root == "" {
		return ""
	}
	if err := validateSandboxID(snapshotID); err != nil {
		return ""
	}
	memBytes := int64(512 << 20)
	if memoryQuantity != "" {
		if mib, err := parseMemMiB(memoryQuantity); err == nil && mib > 0 {
			memBytes = int64(mib) << 20
		} else {
			return ""
		}
	}
	// vmstate is small; the margin covers filesystem overhead growth.
	need := memBytes + memBytes/8 + (64 << 20)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return ""
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free < need {
		klog.InfoS("snapshot spill area too small, falling back to staging dump",
			"spillRoot", root, "freeBytes", free, "needBytes", need)
		return ""
	}
	dir := filepath.Join(root, snapshotID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	return dir
}

// dumpRunningSandbox performs the pause/dump/resume window. The rootfs copy
// and the vmstate/memory dump both happen inside the pause window for
// consistency; when a spill area is available the dump lands THERE and the
// relocation into the publish staging runs after the resume, so the pause
// window pays the spill area's bandwidth (tmpfs/local NVMe) instead of the
// StateRoot filesystem. The Sandbox keeps running on every failure path,
// with a failed resume joined after (and never masking) the dump error.
func (d *Driver) dumpRunningSandbox(ctx context.Context, plan *dumpPlan) error {
	snapshotMu.Lock()
	defer snapshotMu.Unlock()

	state, err := loadState(plan.sandboxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", runtimecontract.ErrSandboxNotFound, plan.sandboxID)
		}
		return err
	}
	if state.Phase != PhaseRunning {
		return fmt.Errorf("%w: sandbox runtime is %q, must be Running", ErrInvalidConfig, state.Phase)
	}
	if state.APIAddress == "" {
		return fmt.Errorf("%w: sandbox state carries no API socket", ErrInvalidConfig)
	}
	if err := os.MkdirAll(plan.staging, 0o750); err != nil {
		return err
	}

	// Resolve the spill area BEFORE pausing: an unspillable dump falls back
	// up front, never mid-window.
	spillDir := d.spillDirFor(plan.snapshotID, state.Config.Spec.Memory)
	plan.spilled = spillDir != ""

	// The instance root drive: the state-directory copy in direct mode, the
	// jail-root copy in jailer mode.
	rootfs := filepath.Join(plan.sandboxDir, instanceRootfsName)
	// Snapshot paths as the (possibly jailed) VMM sees them:
	//   - direct + no spill: absolute staging paths;
	//   - direct + spill: absolute paths inside the spill area;
	//   - jailed (either way): chroot-relative under snapshots/, with the
	//     spill area bind-mounted over the jail's snapshots/ when spilled —
	//     the restore hard links in that directory are never touched
	//     (dedicated dump file names, and hidden behind the bind anyway).
	vmstateTarget := filepath.Join(plan.staging, vmstateSnapshotName)
	memoryTarget := filepath.Join(plan.staging, memorySnapshotName)
	if plan.spilled && !plan.jailed {
		vmstateTarget = filepath.Join(spillDir, jailedDumpVMStateName)
		memoryTarget = filepath.Join(spillDir, jailedDumpMemoryName)
	}
	jailDumpDir := ""
	if plan.jailed {
		rootfs = filepath.Join(plan.jailRoot(), rootfsImageName)
		jailDumpDir = filepath.Join(plan.jailRoot(), jailerSnapshotDumpDir)
		if err := os.MkdirAll(jailDumpDir, 0o750); err != nil {
			return err
		}
		if plan.spilled {
			// The spill root was bind-mounted into the jail at instance
			// creation; the per-snapshot directory is reachable behind it.
			// A sandbox created before the spill was configured (or whose
			// bind failed) falls back to the legacy in-jail dump.
			if _, err := os.Stat(filepath.Join(plan.jailRoot(), jailerSpillDirName, plan.snapshotID)); err != nil {
				plan.spilled = false
				_ = os.RemoveAll(spillDir)
			}
		}
		if plan.spilled {
			vmstateTarget = filepath.ToSlash(filepath.Join("/", jailerSpillDirName, plan.snapshotID, jailedDumpVMStateName))
			memoryTarget = filepath.ToSlash(filepath.Join("/", jailerSpillDirName, plan.snapshotID, jailedDumpMemoryName))
		} else {
			vmstateTarget = filepath.ToSlash(filepath.Join("/", jailerSnapshotDumpDir, jailedDumpVMStateName))
			memoryTarget = filepath.ToSlash(filepath.Join("/", jailerSnapshotDumpDir, jailedDumpMemoryName))
		}
	}

	cleanupSpill := func() {
		if !plan.spilled {
			return
		}
		_ = os.RemoveAll(spillDir)
	}

	client := d.newClient(state.APIAddress)
	defer client.Close()
	pauseStarted := time.Now()
	if err := client.Pause(ctx); err != nil {
		plan.pauseWindow = time.Since(pauseStarted)
		cleanupSpill()
		return fmt.Errorf("pause microVM: %w", err)
	}
	dumpErr := func() error {
		if err := copyReflinkOrCopy(rootfs, filepath.Join(plan.staging, publishedRootfsName)); err != nil {
			return fmt.Errorf("copy instance rootfs: %w", err)
		}
		if err := client.CreateSnapshot(ctx, SnapshotCreateRequest{
			SnapshotType: "Full",
			SnapshotPath: vmstateTarget,
			MemFilePath:  memoryTarget,
		}); err != nil {
			return fmt.Errorf("create Firecracker snapshot: %w", err)
		}
		return nil
	}()
	// The VM resumes regardless of the dump outcome: the business-visible
	// pause window closes here.
	if _, resumeErr := resumeVM(ctx, client, plan.bootTimeout); resumeErr != nil {
		plan.pauseWindow = time.Since(pauseStarted)
		cleanupSpill()
		if dumpErr != nil {
			return errors.Join(dumpErr, fmt.Errorf("resume microVM after failed dump: %w", resumeErr))
		}
		return fmt.Errorf("resume microVM after snapshot: %w", resumeErr)
	}
	plan.pauseWindow = time.Since(pauseStarted)
	if dumpErr != nil {
		cleanupSpill()
		return dumpErr
	}
	if !plan.spilled {
		if plan.jailed {
			// Move the chroot-local dump into the staging directory: the
			// jailed VMM wrote under its own credentials, but the files are
			// regular and movable by this (privileged) driver.
			for dumped, staged := range map[string]string{
				jailedDumpVMStateName: vmstateSnapshotName,
				jailedDumpMemoryName:  memorySnapshotName,
			} {
				if err := os.Rename(filepath.Join(jailDumpDir, dumped), filepath.Join(plan.staging, staged)); err != nil {
					return fmt.Errorf("move dumped %s out of the jail root: %w", dumped, err)
				}
			}
		}
		return nil
	}

	// Spilled: relocate the dump into the publish staging OUTSIDE the pause
	// window (cross-device tmpfs→StateRoot is a plain copy). The jail's
	// spill bind persists with the sandbox and is released at its deletion.
	moveStarted := time.Now()
	for dumped, staged := range map[string]string{
		jailedDumpVMStateName: vmstateSnapshotName,
		jailedDumpMemoryName:  memorySnapshotName,
	} {
		if err := copyFile(filepath.Join(spillDir, dumped), filepath.Join(plan.staging, staged)); err != nil {
			_ = os.RemoveAll(spillDir)
			return fmt.Errorf("move spilled %s into the staging directory: %w", dumped, err)
		}
	}
	plan.spillMove = time.Since(moveStarted)
	_ = os.RemoveAll(spillDir)
	return nil
}

// assembleSnapshotManifest builds the restore-compatible manifest of the
// dumped set: the machine/guestNetwork/kernel/envs facts baked into the
// SOURCE image manifest are copied verbatim (they describe the vmstate
// lineage and are what restore validation checks), while the compatibility
// tuple, files, rootfsSize, format, and validation describe this dump. It
// returns the total logical size of the artifact set.
func assembleSnapshotManifest(stateRoot, staging, sandboxDir, firecrackerBinary string, actionBindings []runtimecontract.SnapshotActionBinding) (int64, error) {
	state, err := loadState(sandboxDir)
	if err != nil {
		return 0, err
	}
	document, err := readSourceManifest(stateRoot, state.Config.Spec.Image)
	if err != nil {
		return 0, err
	}

	cache := map[string]string{}
	files := map[string]any{}
	var sizeBytes, rootfsSize int64
	for _, name := range []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName} {
		entry, err := artifacts.FileEntry(filepath.Join(staging, name), cache)
		if err != nil {
			return 0, fmt.Errorf("checksum %s: %w", name, err)
		}
		files[name] = entry
		if entrySize, ok := entry["sizeBytes"].(int64); ok {
			sizeBytes += entrySize
			if name == publishedRootfsName {
				rootfsSize = entrySize
			}
		}
	}
	if err := artifacts.WriteSHA256SUMS(staging, []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName}, cache); err != nil {
		return 0, err
	}

	document["schemaVersion"] = 1
	document["runtime"] = "firecracker"
	document["sourceImage"] = state.Config.Spec.Image
	document["compatibility"] = map[string]any{
		"firecrackerVersion": artifacts.FirecrackerVersion(firecrackerBinary),
		"hostKernel":         artifacts.HostKernelRelease(),
		"cpuModel":           artifacts.HostCPUModel(),
	}
	document["files"] = files
	document["rootfsSize"] = fmt.Sprintf("%dG", artifacts.SizeGiB(rootfsSize))
	document["format"] = "native"
	document["validation"] = map[string]any{"booted": true, "restored": false}
	// Durable policy provenance: the artifact set outlives the
	// SandboxSnapshot CR (deleting the CR keeps the artifacts), so the
	// source Sandbox's action bindings ride in the manifest itself.
	// Optional field — every existing consumer (pull reads `files`,
	// restore reads machine/guestNetwork) ignores it.
	if len(actionBindings) > 0 {
		recorded := make([]map[string]string, 0, len(actionBindings))
		for _, binding := range actionBindings {
			recorded = append(recorded, map[string]string{"handler": binding.Handler, "input": binding.Input})
		}
		document["actionBindings"] = recorded
	}

	manifestBytes, err := artifacts.MarshalManifest(document)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(staging, snapshotManifestName), manifestBytes, 0o644); err != nil {
		return 0, err
	}
	return sizeBytes, nil
}

// readSourceManifest loads the cached manifest of the image the Sandbox
// boots from. machine and guestNetwork are required: restore validation
// rejects a snapshot manifest without them, so a hand-seeded cache cannot
// produce a restorable snapshot.
func readSourceManifest(stateRoot, image string) (map[string]any, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		return nil, fmt.Errorf("read source image manifest: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("decode source image manifest: %w", err)
	}
	for _, key := range []string{"machine", "guestNetwork"} {
		if _, ok := document[key]; !ok {
			return nil, fmt.Errorf("%w: source image manifest carries no %s; a restorable snapshot cannot be published", ErrInvalidConfig, key)
		}
	}
	return document, nil
}
