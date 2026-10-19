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

	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
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
// direct-to-staging dump.
const snapshotSpillDirEnv = "FAST_SANDBOX_SNAPSHOT_SPILL_DIR"

// egressBundleDirEnv names the directory that hands the vault bundle file to
// the egress Handler: the envelope carries a PATH and the Handler resolves it
// in its own mount namespace, so the directory must be a volume shared with
// the egress container (a hostPath, or the same emptyDir mounted twice).
// Empty keeps the legacy behavior — the bundle lives at the driver's
// snapshot staging path, which requires the staging area itself to be
// mounted into egress.
const egressBundleDirEnv = "FAST_SANDBOX_EGRESS_BUNDLE_DIR"

// Driver implements the optional snapshot extension.
var _ runtimecontract.Snapshotter = (*Driver)(nil)

// snapshotMu serializes the pause/dump/resume window across all Sandboxes of
// this driver: only one VM on the node is ever paused at a time. Manifest
// assembly and publication run outside the lock.
var snapshotMu sync.Mutex

// CreateSnapshot snapshots a running Sandbox in place and publishes the
// artifact set. Template snapshots publish under the template name so later
// creates boot from them; checkpoint snapshots publish an instance-private
// set (no index) that only the owning Sandbox resumes from. The VM is always
// resumed; a failed dump leaves the Sandbox running and discards the staging
// directory without publishing anything.
func (d *Driver) CreateSnapshot(ctx context.Context, input *runtimecontract.SnapshotInput) (*runtimecontract.SnapshotResult, error) {
	if input == nil || input.SandboxID == "" || input.SnapshotID == "" {
		return nil, fmt.Errorf("%w: sandboxId and snapshotId are required", ErrInvalidConfig)
	}
	kind := input.Kind
	if kind == "" {
		kind = fastletapi.SnapshotKindTemplate
	}
	switch kind {
	case fastletapi.SnapshotKindTemplate:
		if input.TemplateName == "" {
			return nil, fmt.Errorf("%w: templateName is required for a template snapshot", ErrInvalidConfig)
		}
	case fastletapi.SnapshotKindCheckpoint:
		if input.TemplateName != "" {
			return nil, fmt.Errorf("%w: checkpoint snapshots publish no template index; templateName must be empty", ErrInvalidConfig)
		}
	default:
		return nil, fmt.Errorf("%w: unknown snapshot kind %q", ErrInvalidConfig, kind)
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
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID,
		"pauseWindow", plan.pauseWindow.String(), "spillMove", plan.spillMove.String(),
		"spilled", plan.spilled, "rootfsClone", plan.rootfsClone.String(), "rootfsCopy", plan.rootfsCopy.String(),
		"pauseAPI", plan.pauseAPI.String(), "dumpAPI", plan.dumpAPI.String(), "resumeAPI", plan.resumeAPI.String())

	// Checkpoints carry the egress-owned vault file in the bundle: the export
	// runs here — inside the pause flow, strictly before the control plane
	// releases the runtime (the release is what sends REMOVE_BINDING and
	// purges the egress-local file). An export failure fails the checkpoint,
	// so the egress-local file survives for the retry. Template snapshots
	// NEVER export: the vault belongs to one sandbox instance, and a shared
	// boot image would leak it to every sandbox booted from the template.
	//
	// The staging directory may not exist yet on this path: create it, and
	// hand the bundle to the Handler through the shared egress bundle
	// directory when one is configured — the envelope carries a file PATH
	// and the Handler resolves it in its own mount namespace, where the
	// driver's staging is not mounted.
	if kind == fastletapi.SnapshotKindCheckpoint {
		if err := os.MkdirAll(plan.staging, 0o700); err != nil {
			return nil, fmt.Errorf("prepare snapshot staging %s: %w", plan.staging, err)
		}
		if err := d.stageCheckpointVault(ctx, input.SandboxID, plan.snapshotID, plan.staging); err != nil {
			_ = os.RemoveAll(plan.staging)
			return nil, err
		}
	}

	sizeBytes, err := assembleSnapshotManifest(plan.stateRoot, plan.staging, plan.sandboxDir, input.ActionBindings)
	if err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}
	klog.InfoS("firecracker checkpoint manifest assembled",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "kind", kind,
		"sizeBytes", sizeBytes, "staging", plan.staging)

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
	outcome, publishErr := client.PublishImage(ctx, "snapshot-"+plan.snapshotID, publishKind(kind), input.TemplateName, plan.staging)
	if publishErr != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, fmt.Errorf("publish snapshot artifacts: %w", publishErr)
	}
	result := &runtimecontract.SnapshotResult{
		SnapshotID:     input.SnapshotID,
		ManifestRef:    outcome.ManifestRef,
		ArtifactDigest: outcome.ArtifactDigest,
		SizeBytes:      sizeBytes,
	}
	// A checkpoint keeps the published set in the node-local image cache: a
	// resume that still lands on this node restores locally (~ms) instead of
	// pulling the full set back from the store (~16s per 3.5GiB). The commit
	// is best-effort — the store copy is authoritative, and any miss (cache
	// evicted by the image GC, or a resume on another node) falls back to the
	// normal pull path. Template snapshots keep discarding staging; their
	// same-node prewarm is issue #57.
	if kind == fastletapi.SnapshotKindCheckpoint {
		if cacheErr := d.commitCheckpointCache(result, plan.staging); cacheErr != nil {
			klog.ErrorS(cacheErr, "checkpoint local cache commit failed; a resume will pull from the store",
				"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "manifestRef", outcome.ManifestRef)
		}
	} else {
		_ = os.RemoveAll(plan.staging)
	}
	klog.InfoS("firecracker snapshot published",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "kind", kind, "templateName", input.TemplateName,
		"manifestRef", outcome.ManifestRef, "sizeBytes", sizeBytes, "publish", time.Since(publishStarted).String())
	return result, nil
}

// commitCheckpointCache moves a published checkpoint artifact set from its
// staging directory into the node-local image cache, under the canonical
// checkpoint reference (artifacts.CheckpointReference), in the standard cache
// layout (rootfs.img/vmstate.snap/memory.snap/manifest.json). The set is
// assembled in a non-digest staging subdirectory and renamed into place
// atomically, so the image GC — which only considers 64-hex cache names —
// observes either a complete entry or nothing at all. The staging directory
// is consumed on every path.
func (d *Driver) commitCheckpointCache(result *runtimecontract.SnapshotResult, staging string) error {
	if result == nil || result.ArtifactDigest == "" {
		return fmt.Errorf("%w: checkpoint result carries no artifact digest", ErrInvalidConfig)
	}
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	cacheDir := filepath.Join(stateRoot, imageCacheDir, imageKey(reference))
	if verifyRestorableImage(stateRoot, reference) == nil {
		// A concurrent replay already committed the identical set.
		_ = os.RemoveAll(staging)
		d.touchImage(reference)
		return nil
	}
	tmpDir := filepath.Join(stateRoot, imageCacheDir, ".staging-"+filepath.Base(staging))
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("prepare checkpoint cache staging: %w", err)
	}
	mappings := map[string]string{
		publishedRootfsName:  rootfsImageName,
		vmstateSnapshotName:  vmstateSnapshotName,
		memorySnapshotName:   memorySnapshotName,
		snapshotManifestName: snapshotManifestName,
	}
	// The vault file is an optional bundle member; it rides into the cache
	// so the restore path finds it next to the snapshot files. Mirror the
	// assembly/publish stat contract: only absence skips it — dropping the
	// file on a transient error would corrupt the node-local cache
	// asymmetrically against the published set.
	if _, err := os.Stat(filepath.Join(staging, vaultBundleName)); err == nil {
		mappings[vaultBundleName] = vaultBundleName
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(tmpDir)
		_ = os.RemoveAll(staging)
		return fmt.Errorf("stat staged %s for the cache commit: %w", vaultBundleName, err)
	}
	for from, to := range mappings {
		if err := os.Rename(filepath.Join(staging, from), filepath.Join(tmpDir, to)); err != nil {
			_ = os.RemoveAll(tmpDir)
			_ = os.RemoveAll(staging)
			return fmt.Errorf("stage checkpoint cache entry: %w", err)
		}
	}
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		_ = os.RemoveAll(staging)
		// A concurrent commit won the rename; the identical set is already
		// addressable.
		if verifyRestorableImage(stateRoot, reference) == nil {
			d.touchImage(reference)
			return nil
		}
		return fmt.Errorf("commit checkpoint cache entry: %w", err)
	}
	_ = os.RemoveAll(staging)
	d.touchImage(reference)
	return nil
}

// publishKind maps the Fastlet-side snapshot kind onto the runtime-agent's
// publish protocol. The two enums are deliberately separate layers (the
// Fastlet kind travels FastPath->Fastlet as JSON; the publish kind travels
// driver->agent), so the mapping is explicit: a raw string cast here would
// silently send "Checkpoint" where the agent expects "checkpoint".
func publishKind(kind fastletapi.SnapshotKind) string {
	if kind == fastletapi.SnapshotKindCheckpoint {
		return agentprotocol.PublishKindCheckpoint
	}
	return agentprotocol.PublishKindTemplate
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
		return nil //nolint:nilerr // unmeasurable footprint must not block the dump; the dump itself reports the precise error
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
	klog.InfoS("firecracker snapshot staging space insufficient; parking the task",
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
	rootfsClone time.Duration
	rootfsCopy  time.Duration
	pauseAPI    time.Duration
	dumpAPI     time.Duration
	resumeAPI   time.Duration
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
// cannot hold the dump (the caller falls back to the
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
		klog.InfoS("firecracker snapshot spill area too small, falling back to staging dump",
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
func (d *Driver) dumpRunningSandbox(ctx context.Context, plan *dumpPlan) error { //nolint:gocognit // pre-existing pause/dump/resume window; refactor tracked separately
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
	klog.InfoS("firecracker sandbox dump starting",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID,
		"staging", plan.staging, "jailed", plan.jailed, "spilled", plan.spilled)

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
			// bind failed) falls back to the in-jail dump.
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

	// Clone the instance rootfs BEFORE pausing when the filesystem supports
	// reflinks: the clone is atomic at the extent level (concurrent guest
	// writes COW into new extents and never touch the copy), and
	// disk-behind-memory is the crash-consistent direction — so a slow
	// reflink (seconds on a loaded host) no longer sits in the
	// business-visible pause window. Filesystems without reflinks keep the
	// in-window full copy: it is not atomic and must happen while paused.
	stagedRootfs := filepath.Join(plan.staging, publishedRootfsName)
	rootfsCloned := false
	reflinkStarted := time.Now()
	if err := reflinkOnly(rootfs, stagedRootfs); err == nil {
		rootfsCloned = true
	}
	plan.rootfsClone = time.Since(reflinkStarted)

	client := d.newClient(state.APIAddress)
	defer client.Close()
	pauseStarted := time.Now()
	if err := client.Pause(ctx); err != nil {
		plan.pauseWindow = time.Since(pauseStarted)
		cleanupSpill()
		return fmt.Errorf("pause microVM: %w", err)
	}
	plan.pauseAPI = time.Since(pauseStarted)
	klog.InfoS("firecracker sandbox paused for dump",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "spilled", plan.spilled,
		"rootfsCloned", rootfsCloned)
	dumpErr := func() error {
		if !rootfsCloned {
			copyStarted := time.Now()
			if err := copyReflinkOrCopy(rootfs, stagedRootfs); err != nil {
				return fmt.Errorf("copy instance rootfs: %w", err)
			}
			plan.rootfsCopy = time.Since(copyStarted)
		}
		dumpStarted := time.Now()
		err := client.CreateSnapshot(ctx, SnapshotCreateRequest{
			SnapshotType: "Full",
			SnapshotPath: vmstateTarget,
			MemFilePath:  memoryTarget,
		})
		plan.dumpAPI = time.Since(dumpStarted)
		if err != nil {
			return fmt.Errorf("create Firecracker snapshot: %w", err)
		}
		return nil
	}()
	// The VM resumes regardless of the dump outcome: the business-visible
	// pause window closes here.
	resumeStarted := time.Now()
	if _, resumeErr := resumeVM(ctx, client, plan.bootTimeout); resumeErr != nil {
		plan.pauseWindow = time.Since(pauseStarted)
		cleanupSpill()
		if dumpErr != nil {
			return errors.Join(dumpErr, fmt.Errorf("resume microVM after failed dump: %w", resumeErr))
		}
		return fmt.Errorf("resume microVM after snapshot: %w", resumeErr)
	}
	plan.resumeAPI = time.Since(resumeStarted)
	plan.pauseWindow = time.Since(pauseStarted)
	if dumpErr != nil {
		cleanupSpill()
		return dumpErr
	}
	klog.InfoS("firecracker sandbox resumed after dump",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "pauseWindow", plan.pauseWindow.String(),
		"rootfsClone", plan.rootfsClone.String(), "rootfsCopy", plan.rootfsCopy.String(),
		"pauseAPI", plan.pauseAPI.String(), "dumpAPI", plan.dumpAPI.String(), "resumeAPI", plan.resumeAPI.String())
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

// vaultBundleName is the staged vault file of a checkpoint bundle. It
// carries the egress-owned vault of one sandbox instance; the egress re-stamps
// its incarnation at restore import.
const vaultBundleName = "vault.json"

// egressBundleRoot returns the configured directory hands the vault bundle
// to the egress Handler through ("" keeps the legacy staging path). The
// envelope carries a file PATH and the Handler resolves it in its own mount
// namespace, so the directory must be a volume shared with the egress
// container (hostPath or same-emptyDir double mount). Read per call so
// tests can inject without racing the driver lock.
func (d *Driver) egressBundleRoot() string {
	return strings.TrimSpace(os.Getenv(egressBundleDirEnv))
}

// stageCheckpointVault exports the subject's egress vault into the checkpoint
// bundle. The Handler copies the file itself: a successful export with no
// file at the target simply means the subject held no vault; any transfer
// failure fails the checkpoint so the egress-local file survives for the
// retry. With a shared bundle root configured, the export lands there and
// the verified copy joins the publish staging afterwards.
func (d *Driver) stageCheckpointVault(ctx context.Context, sandboxUID, snapshotID, staging string) error {
	target := filepath.Join(staging, vaultBundleName)
	handoffDir := ""
	if root := d.egressBundleRoot(); root != "" {
		if err := validateSandboxID(snapshotID); err != nil {
			return fmt.Errorf("resolve the egress bundle handoff for snapshot %s: %w", snapshotID, err)
		}
		handoffDir = filepath.Join(root, snapshotID)
		if err := os.MkdirAll(handoffDir, 0o700); err != nil {
			return fmt.Errorf("prepare the egress bundle handoff directory: %w", err)
		}
		defer os.RemoveAll(handoffDir)
		target = filepath.Join(handoffDir, vaultBundleName)
	}
	if err := d.moveVault(ctx, sandboxUID, target); err != nil {
		return err
	}
	if handoffDir != "" {
		data, err := os.ReadFile(target)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil //nolint:nilerr // no vault to carry: nothing joins the staging
			}
			return fmt.Errorf("read back the exported vault bundle: %w", err)
		}
		if err := os.WriteFile(filepath.Join(staging, vaultBundleName), data, 0o600); err != nil {
			return fmt.Errorf("stage the exported vault bundle: %w", err)
		}
	}
	return nil
}

// moveVault invokes the vault mover's export at the given handoff path.
func (d *Driver) moveVault(ctx context.Context, sandboxUID, bundlePath string) error {
	d.mu.RLock()
	mover := d.vaultMover
	d.mu.RUnlock()
	if mover == nil {
		return nil //nolint:nilerr // no mover wired: vault transfer disabled
	}
	if err := mover.ExportVault(ctx, sandboxUID, bundlePath); err != nil {
		return fmt.Errorf("export egress vault for the checkpoint bundle: %w", err)
	}
	return nil
}

// maxVaultBundleBytes bounds the vault bundle file buffered for provenance
// verification. The vault is decoupled from the binding-input cap, so the
// bound is its own (generous) limit; nothing larger is ever legitimate.
const maxVaultBundleBytes = 4 << 20

// importRestoreVault delivers a restored checkpoint's vault bundle file to
// egress — before the data plane becomes ready. Only a resumed checkpoint
// carries the file (golden-image caches never do); a nil mover is a no-op.
// The committed manifest's egressVault entry is the enforced provenance:
// an authorized-but-missing file, a read failure, a marker/size/digest
// mismatch, or an import failure all degrade the subject's vault
// fail-closed and observably — mirroring the corrupt vault file posture at
// registration. The Handler reads the bundle file itself; the driver only
// verifies it and hands over the path.
func (d *Driver) importRestoreVault(ctx context.Context, spec fastletapi.SandboxSpec, restoreRef, vmstatePath, sandboxUID string) {
	if spec.Restore == nil {
		return
	}
	d.mu.RLock()
	mover := d.vaultMover
	d.mu.RUnlock()
	if mover == nil {
		return
	}
	bundleDir := filepath.Dir(vmstatePath)
	vaultPath := filepath.Join(bundleDir, vaultBundleName)

	marker, markerPresent, markerErr := vaultManifestMarker(bundleDir)
	if markerErr != nil {
		klog.ErrorS(markerErr, "read the checkpoint manifest for the vault provenance failed; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
		return
	}

	info, statErr := os.Stat(vaultPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if markerPresent {
				// The manifest authorizes a vault but the bundle file is
				// gone (partial cache damage): never a silent skip.
				klog.ErrorS(errVaultBundleMissing, "checkpoint manifest authorizes a vault but the bundle file is missing; the restored Sandbox vault degrades",
					"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
			}
			return //nolint:nilerr // no manifest-listed vault: nothing to import
		}
		klog.ErrorS(statErr, "read bundled egress vault failed; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
		return //nolint:nilerr // already logged
	}
	if !markerPresent {
		klog.ErrorS(errVaultBundleUnlisted, "bundled vault file is not listed by the checkpoint manifest; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
		return //nolint:nilerr // unlisted credential bytes are never delivered
	}
	// Stat-first bounds: the whole file is never buffered before the size
	// contract holds, so a corrupted/replaced file cannot grow the driver's
	// memory arbitrarily.
	if info.Size() != marker.SizeBytes || info.Size() > maxVaultBundleBytes {
		klog.ErrorS(errVaultBundleMismatch, "bundled vault size mismatches the manifest entry; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath,
			"stagedBytes", info.Size(), "manifestBytes", marker.SizeBytes)
		return //nolint:nilerr // already logged
	}
	vaultData, readErr := os.ReadFile(vaultPath)
	if readErr != nil {
		klog.ErrorS(readErr, "read bundled egress vault failed; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
		return //nolint:nilerr // already logged
	}
	if marker.SHA256 != artifacts.SHA256Of(vaultData) {
		klog.ErrorS(errVaultBundleMismatch, "bundled vault digest mismatches the manifest entry; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
		return //nolint:nilerr // unverified credential bytes are never delivered
	}
	// Hand the verified bundle through the shared egress bundle directory
	// when configured: the Handler reads the path in its own mount
	// namespace, where the restored staging is not mounted. Every handoff
	// failure is the standard import-failure posture: the restored
	// Sandbox's vault degrades, observably, and the resume proceeds.
	if root := d.egressBundleRoot(); root != "" {
		shared := filepath.Join(root, "restore-"+sandboxUID+".json")
		if err := os.MkdirAll(root, 0o700); err != nil {
			klog.ErrorS(err, "prepare the egress bundle handoff directory failed; the restored Sandbox vault degrades",
				"sandboxID", sandboxUID, "checkpoint", restoreRef)
			return //nolint:nilerr // degradation is the import-failure posture
		}
		if err := os.WriteFile(shared, vaultData, 0o600); err != nil {
			klog.ErrorS(err, "stage the restored vault bundle for egress failed; the restored Sandbox vault degrades",
				"sandboxID", sandboxUID, "checkpoint", restoreRef)
			return //nolint:nilerr // degradation is the import-failure posture
		}
		defer os.Remove(shared)
		vaultPath = shared
	}
	if importErr := mover.ImportVault(ctx, sandboxUID, vaultPath); importErr != nil {
		klog.ErrorS(importErr, "egress vault import failed; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef)
	}
}

// vaultMarker is the manifest's egressVault provenance entry.
type vaultMarker struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// sentinel degradation causes of the restore import; the log line carries
// the specifics.
var (
	errVaultBundleMissing  = errors.New("vault bundle file missing")
	errVaultBundleUnlisted = errors.New("vault bundle file not listed by the manifest")
	errVaultBundleMismatch = errors.New("vault bundle does not match the manifest entry")
)

// vaultManifestMarker parses the committed checkpoint manifest's egressVault
// entry. A manifest without the marker authorizes nothing.
func vaultManifestMarker(bundleDir string) (*vaultMarker, bool, error) {
	payload, err := os.ReadFile(filepath.Join(bundleDir, snapshotManifestName))
	if err != nil {
		return nil, false, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	var document struct {
		EgressVault *vaultMarker `json:"egressVault"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, false, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	return document.EgressVault, document.EgressVault != nil, nil
}

// assembleSnapshotManifest builds the restore-compatible manifest of the
// dumped set. lineage, machine (vcpu/memory), guestNetwork, and
// compatibility ride forward from the SOURCE manifest verbatim — the dumped
// vmstate carries the source snapshot's CPU state, so its compatibility
// identity is the source's. Only files, machine.rootfs, format, and
// validation describe this dump. Returns the total logical artifact size.
// A staged vault bundle file joins files, SHA256SUMS, and the egressVault
// manifest marker.
func assembleSnapshotManifest(stateRoot, staging, sandboxDir string, actionBindings []runtimecontract.SnapshotActionBinding) (int64, error) {
	state, err := loadState(sandboxDir)
	if err != nil {
		return 0, err
	}
	// The lineage manifest of the dump: the golden image manifest of a fresh
	// boot, or the cached checkpoint manifest when the Sandbox was itself
	// resumed from a checkpoint (the checkpoint carries every lineage field
	// forward, and the original image may not exist on this node at all).
	sourceRef, err := restoreReference(state.Config.Spec)
	if err != nil {
		return 0, err
	}
	document, err := readSourceManifest(stateRoot, sourceRef)
	if err != nil {
		return 0, err
	}

	bundleFiles := []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName}
	// Mirror the publish side (PublishImage): only absence means "no vault",
	// any other stat failure fails the assembly — a transient error here
	// would otherwise produce a manifest that omits the vault while the
	// publish still uploads the orphaned file.
	if _, err := os.Stat(filepath.Join(staging, vaultBundleName)); err == nil {
		bundleFiles = append(bundleFiles, vaultBundleName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("stat staged %s: %w", vaultBundleName, err)
	}

	cache := map[string]string{}
	files := map[string]any{}
	var sizeBytes, rootfsSize int64
	for _, name := range bundleFiles {
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
	if err := artifacts.WriteSHA256SUMS(staging, bundleFiles, cache); err != nil {
		return 0, err
	}

	document["schemaVersion"] = 1
	document["runtime"] = "firecracker"
	// The lineage object rides forward from the source manifest; only the
	// image reference is rewritten to the one this Sandbox booted from (the
	// imageDigest stays the original build's anchor).
	lineage, ok := document["lineage"].(map[string]any)
	if !ok {
		lineage = map[string]any{}
		document["lineage"] = lineage
	}
	lineage["image"] = state.Config.Spec.Image
	// compatibility rides forward untouched — see assembleSnapshotManifest.
	document["files"] = files
	// machine.rootfs reflects this dump's actual rootfs size; vcpu/memory
	// ride forward from the source manifest untouched.
	if machine, ok := document["machine"].(map[string]any); ok {
		machine["rootfs"] = fmt.Sprintf("%dGi", artifacts.SizeGiB(rootfsSize))
	}
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
	// The egress vault marker describes THIS dump: a source checkpoint's
	// inherited marker must never ride forward — it would claim vault state
	// this bundle does not carry and publish the previous subject's vault
	// fingerprint into shared, world-readable template manifests.
	delete(document, "egressVault")
	if vaultEntry, ok := files[vaultBundleName]; ok {
		document["egressVault"] = vaultEntry
	}

	manifestBytes, err := artifacts.MarshalManifest(document)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(staging, snapshotManifestName), manifestBytes, 0o644); err != nil { //nolint:gosec // published snapshot manifest, world-readable by design
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
