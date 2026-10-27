package firecracker

// snapshot_driver.go implements the optional Snapshotter contract
// (runtimecontract.Snapshotter) for the live firecracker driver: pause the
// running microVM, dump the writable instance rootfs plus the vmstate/memory
// pair, resume the VM on every path, assemble a restore-compatible manifest
// (byte-format shared with the golden-image builder via internal/artifacts),
// and publish the set. Native publication requires the node runtime-agent
// and its store credentials; OCI publication uses the Fastlet registry credentials.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
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

// Driver implements the optional snapshot extension.
var _ runtimecontract.Snapshotter = (*Driver)(nil)

// snapshotMu serializes the pause/dump/resume window across all Sandboxes of
// this driver: only one VM on the node is ever paused at a time. Manifest
// assembly and publication run outside the lock.
var snapshotMu sync.Mutex

// CreateSnapshot snapshots a running Sandbox in place and publishes the
// artifact set. Native template snapshots publish a name index; OCI templates
// publish an immutable registry reference for later creates; checkpoint snapshots publish an instance-private
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
	plan.chunked = d.agentTransferChunked()
	plan.sandboxDir = filepath.Join(plan.stateRoot, sandboxStateDir, plan.sandboxID)
	ociSnapshot, err := prepareOCISnapshot(plan)
	if err != nil {
		return nil, err
	}
	if !ociSnapshot && kind == fastletapi.SnapshotKindTemplate && input.TemplateName == "" {
		return nil, fmt.Errorf("%w: templateName is required for a native template snapshot", ErrInvalidConfig)
	}
	plan.staging = snapshotStagingPath(plan.stateRoot, d.podUID, plan.snapshotID, ociSnapshot)
	if err := d.cleanupSnapshotStaging(ctx, plan.staging); err != nil {
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

	// Both snapshot kinds carry the egress-owned state files (vault,
	// recorded policy) — the snapshot must be a COMPLETE capture:
	//   - Checkpoints export with the subject SEALED: the exports run
	//     inside the pause flow, strictly before the control plane releases
	//     the runtime, and the import re-owns the record at restore. An
	//     export failure fails the checkpoint, so the egress-local files
	//     survive for the retry.
	//   - Templates export SEAL-FREE (HandlerStatus.TemplateStateExport):
	//     a template's completion path never imports, so a sealed export
	//     would lock the still-running source out of its state CRUD — the
	//     record is copied verbatim and the source keeps mutating. A clone
	//     boots with the template's state (landed for its first
	//     registration, re-stamped to the clone's fence). The vault record
	//     rides encrypted at rest; key scoping is the clone-isolation
	//     boundary.
	if kind == fastletapi.SnapshotKindCheckpoint {
		if err := d.stageCheckpointVault(ctx, input.SandboxID, plan.staging); err != nil {
			_ = os.RemoveAll(plan.staging)
			return nil, err
		}
		if err := d.stageCheckpointPolicy(ctx, input.SandboxID, plan.staging); err != nil {
			_ = os.RemoveAll(plan.staging)
			return nil, err
		}
	} else if err := d.stageTemplateState(ctx, input.SandboxID, plan.staging); err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}

	assembleStarted := time.Now()
	sizeBytes, err := d.assemblePlanManifest(&plan, kind)
	if err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}
	klog.InfoS("firecracker checkpoint manifest assembled",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "kind", kind,
		"sizeBytes", sizeBytes, "staging", plan.staging, "assemble", time.Since(assembleStarted).String())

	if ociSnapshot {
		return d.publishOCISnapshot(ctx, plan, sizeBytes, kind, input.OnPublishing)
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
	d.retainPublishedSet(&plan, kind, input.TemplateName, result, outcome)
	klog.InfoS("firecracker snapshot published",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "kind", kind, "templateName", input.TemplateName,
		"manifestRef", outcome.ManifestRef, "sizeBytes", sizeBytes, "publish", time.Since(publishStarted).String())
	return result, nil
}

// assemblePlanManifest writes the snapshot manifest of the staged set: the
// original sequential assembly, or in chunked transfer mode the parallel one
// reusing the digests the spill copy computed.
func (d *Driver) assemblePlanManifest(plan *dumpPlan, kind fastletapi.SnapshotKind) (int64, error) {
	if plan.chunked {
		return assembleSnapshotManifestWith(plan.stateRoot, d.ociRoot(), plan.staging, plan.sandboxDir, kind, plan.knownSums, true)
	}
	return assembleSnapshotManifest(plan.stateRoot, d.ociRoot(), plan.staging, plan.sandboxDir, kind)
}

// retainPublishedSet decides what happens to the staging directory of a
// published set. A checkpoint keeps the set in the node-local image cache: a
// resume that still lands on this node restores locally (~ms) instead of
// pulling the full set back from the store (~16s per 3.5GiB). The commit is
// best-effort — the store copy is authoritative, and any miss (cache evicted
// by the image GC, or a resume on another node) falls back to the normal
// pull path. Template snapshots discard staging, unless the agent runs the
// chunked transfer mode: then the set is cached under its template name, the
// reference a later create resolves (issue #57).
func (d *Driver) retainPublishedSet(plan *dumpPlan, kind fastletapi.SnapshotKind, templateName string, result *runtimecontract.SnapshotResult, outcome PublishOutcome) {
	switch {
	case kind == fastletapi.SnapshotKindCheckpoint:
		if cacheErr := d.commitCheckpointCache(result, plan.staging); cacheErr != nil {
			klog.ErrorS(cacheErr, "checkpoint local cache commit failed; a resume will pull from the store",
				"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "manifestRef", outcome.ManifestRef)
		}
	case outcome.TransferFormat == agentTransferFormatChunked:
		if cacheErr := d.commitSnapshotCache(templateName, plan.staging); cacheErr != nil {
			klog.ErrorS(cacheErr, "template snapshot local cache commit failed; a create will pull from the store",
				"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "templateName", templateName)
		}
	default:
		_ = os.RemoveAll(plan.staging)
	}
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
	return d.commitSnapshotCache(artifacts.CheckpointReference(result.ArtifactDigest), staging)
}

// commitSnapshotCache moves a published artifact set from its staging
// directory into the node-local image cache under reference (the checkpoint
// reference, or the template name of a template snapshot); see
// commitCheckpointCache. An existing complete entry is kept, matching the
// pull path, which never refreshes a committed cache entry.
func (d *Driver) commitSnapshotCache(reference, staging string) error {
	if reference == "" {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("%w: snapshot cache reference is empty", ErrInvalidConfig)
	}
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
	// The vault and policy files are optional bundle members; they ride
	// into the cache so the restore path finds them next to the snapshot
	// files. Mirror the assembly/publish stat contract: only absence skips
	// a file — dropping it on a transient error would corrupt the
	// node-local cache asymmetrically against the published set.
	for _, bundleName := range []string{vaultBundleName, policyBundleName} {
		if _, err := os.Stat(filepath.Join(staging, bundleName)); err == nil {
			mappings[bundleName] = bundleName
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = os.RemoveAll(tmpDir)
			_ = os.RemoveAll(staging)
			return fmt.Errorf("stat staged %s for the cache commit: %w", bundleName, err)
		}
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
func (d *Driver) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	if err := validateSandboxID(snapshotID); err != nil {
		return fmt.Errorf("%w: invalid snapshot id %q", ErrInvalidConfig, snapshotID)
	}
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	staging := filepath.Join(stateRoot, snapshotStagingDir, snapshotID)
	if err := d.cleanupSnapshotStaging(ctx, staging); err != nil {
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
	if state.Config.Limits.Memory == "" {
		return 0, fmt.Errorf("sandbox spec carries no memory size")
	}
	mib, err := parseMemMiB(state.Config.Limits.Memory)
	if err != nil {
		return 0, err
	}
	rootfs := filepath.Join(plan.sandboxDir, instanceRootfsName)
	if plan.jailed {
		rootfs = filepath.Join(plan.jailRoot(), rootfsImageName)
	}
	if source, e := restoreReference(state.Config.Desired); e == nil && strings.HasPrefix(source, "oci://") {
		rootfs = filepath.Join(filepath.Dir(rootfs), "rootfs.dev")
	}
	info, err := d.stat(rootfs)
	if err != nil {
		return 0, err
	}
	rootBytes := info.Size()
	if source, e := restoreReference(state.Config.Desired); e == nil && strings.HasPrefix(source, "oci://") {
		// Frozen upper extents, snapfiles and compressed output are staged;
		// remote parent data is never materialized in local scratch space.
		home := filepath.Join(d.ociRoot(), "sandboxes", plan.sandboxID, "upper")
		var upperBytes int64
		for _, name := range []string{"writable_data", "writable_index"} {
			info, e := os.Stat(filepath.Join(home, name))
			if e != nil {
				return 0, e
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				upperBytes += stat.Blocks * 512
			} else {
				upperBytes += info.Size()
			}
		}
		return 3*(upperBytes+(int64(mib)<<20)) + (512 << 20), nil
	}
	// vmstate is small; the margin covers filesystem overhead.
	return rootBytes + int64(mib)<<20 + (64 << 20) + int64(mib)<<20/8, nil
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
	// chunked mirrors the runtime-agent's chunked transfer mode: the spill
	// copy hashes as it goes and the manifest assembly hashes in parallel.
	chunked bool
	// knownSums carries staged-file digests computed while the dump was
	// relocated (path -> sha256), so the manifest assembly skips re-reading
	// those files.
	knownSums map[string]string
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
func (d *Driver) dumpRunningSandbox(ctx context.Context, plan *dumpPlan) error { //nolint:gocognit,maintidx // pre-existing pause/dump/resume window; refactor tracked separately
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
	spillDir := d.spillDirFor(plan.snapshotID, state.Config.Limits.Memory)
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

	sourceRef, err := restoreReference(state.Config.Desired)
	if err != nil {
		return err
	}
	ociRoot := strings.HasPrefix(sourceRef, "oci://")
	if ociRoot {
		rootfs = filepath.Join(filepath.Dir(rootfs), "rootfs.dev")
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
	if !ociRoot {
		if err := reflinkOnly(rootfs, stagedRootfs); err == nil {
			rootfsCloned = true
		}
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
		if !rootfsCloned && !ociRoot {
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
		if ociRoot {
			copyStarted := time.Now()
			if err := captureOCIUpper(d.ociRoot(), plan.sandboxID, plan.staging); err != nil {
				return fmt.Errorf("freeze OverlayBD private upper: %w", err)
			}
			plan.rootfsCopy = time.Since(copyStarted)
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
		if err := relocateSpilledFile(plan, filepath.Join(spillDir, dumped), filepath.Join(plan.staging, staged)); err != nil {
			_ = os.RemoveAll(spillDir)
			return fmt.Errorf("move spilled %s into the staging directory: %w", dumped, err)
		}
	}
	plan.spillMove = time.Since(moveStarted)
	_ = os.RemoveAll(spillDir)
	return nil
}

// vaultBundleName / policyBundleName are the staged state bundle files of a
// snapshot set. The names are owned by the shared protocol package: the
// publisher uploads and the pull layer materializes the same names, so a
// rename anywhere must go through there.
const (
	vaultBundleName  = agentprotocol.VaultBundleName
	policyBundleName = agentprotocol.PolicyBundleName
)

// stageCheckpointVault exports the subject's egress vault into the checkpoint
// staging directory (no-op without a mover). The Handler copies the file
// itself: a successful export with no file at the target simply means the
// subject held no vault; any transfer failure fails the checkpoint so the
// egress-local file survives for the retry.
func (d *Driver) stageCheckpointVault(ctx context.Context, sandboxUID, staging string) error {
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return nil //nolint:nilerr // no mover wired: state transfers disabled
	}
	if err := mover.ExportState(ctx, sandboxUID, runtimecontract.StateKindVault, filepath.Join(staging, vaultBundleName)); err != nil {
		return fmt.Errorf("export egress vault for the checkpoint bundle: %w", err)
	}
	return nil
}

// stageCheckpointPolicy exports the subject's egress-recorded network policy
// into the checkpoint staging directory (no-op without a mover). Checkpoint
// snapshots only — the export seals the subject on the egress side, and a
// template's completion path never imports, so a template export would
// leave the still-running source without policy CRUD. Same failure posture
// as the vault: a transfer failure fails the checkpoint so the egress-local
// file survives for the retry.
func (d *Driver) stageCheckpointPolicy(ctx context.Context, sandboxUID, staging string) error {
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return nil //nolint:nilerr // no mover wired: state transfers disabled
	}
	if err := mover.ExportState(ctx, sandboxUID, runtimecontract.StateKindPolicy, filepath.Join(staging, policyBundleName)); err != nil {
		return fmt.Errorf("export egress policy for the snapshot bundle: %w", err)
	}
	return nil
}

// stageTemplateState exports the subject's egress state files (vault,
// recorded policy) into the template staging directory WITHOUT sealing the
// subject — the HandlerStatus.TemplateStateExport semantics, so the
// still-running source keeps full relay CRUD (a template's completion path
// never imports). A silent no-op without a mover or when the mover carries
// no template-carry capability. A Handler WITHOUT the capability is a LOUD
// failure (the mover surfaces it): a snapshot must be a complete capture,
// and publishing a state-less template as if it were the source's state is
// exactly the regression to avoid. Any transfer failure fails the template
// with the handler's reason.
func (d *Driver) stageTemplateState(ctx context.Context, sandboxUID, staging string) error {
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return nil //nolint:nilerr // no mover wired: state transfers disabled
	}
	carrier, ok := mover.(interface {
		ExportTemplateState(ctx context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error
	})
	if !ok {
		return nil //nolint:nilerr // mover without the template-carry capability
	}
	for _, state := range []struct {
		kind  runtimecontract.StateKind
		name  string
		stage func() error
	}{
		{runtimecontract.StateKindVault, "vault", func() error {
			return carrier.ExportTemplateState(ctx, sandboxUID, runtimecontract.StateKindVault, filepath.Join(staging, vaultBundleName))
		}},
		{runtimecontract.StateKindPolicy, "policy", func() error {
			return carrier.ExportTemplateState(ctx, sandboxUID, runtimecontract.StateKindPolicy, filepath.Join(staging, policyBundleName))
		}},
	} {
		if err := state.stage(); err != nil {
			return fmt.Errorf("export egress %s for the template bundle: %w", state.name, err)
		}
	}
	return nil
}

// importTemplateCarry lands a template-borne state record as the fresh
// subject's seed, for a fresh boot whose template bundle carries state files
// (the clone boots with the template's state, re-stamped to the clone's
// fence at its first registration). Resume restores take the restore-import
// path instead; the landing is a silent no-op without a mover, without the
// mover's template-carry capability, or when the template bundle carries no
// state. Provenance is manifest-enforced exactly like the restore import
// (sha256 + size, stat-first bounded); every rejected posture degrades the
// clone to the CRD seed and never fails the create.
func (d *Driver) importTemplateCarry(ctx context.Context, spec fastletapi.SandboxDesiredSpec, restoreRef, bundleDir, sandboxUID string) {
	if spec.Restore != nil {
		return //nolint:nilerr // restores take the restore-import path
	}
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return
	}
	carrier, ok := mover.(interface {
		ImportTemplateState(ctx context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error
	})
	if !ok {
		return //nolint:nilerr // mover without the template-carry capability
	}
	for _, state := range []struct {
		kind      runtimecontract.StateKind
		bundle    string
		maxBytes  int64
		marker    func(string) (*fileMarker, bool, error)
		sentinelM error
		sentinelU error
	}{
		{runtimecontract.StateKindVault, vaultBundleName, maxVaultBundleBytes, vaultManifestMarker, errVaultBundleMissing, errVaultBundleUnlisted},
		{runtimecontract.StateKindPolicy, policyBundleName, maxPolicyBundleBytes, policyManifestMarker, errPolicyBundleMissing, errPolicyBundleUnlisted},
	} {
		bundlePath := filepath.Join(bundleDir, state.bundle)
		marker, markerPresent, markerErr := state.marker(bundleDir)
		if markerErr != nil {
			klog.ErrorS(markerErr, "read the template manifest for the "+string(state.kind)+" provenance failed; the clone degrades to the CRD seed",
				"sandboxID", sandboxUID, "template", restoreRef)
			continue
		}
		importRestoreBundle(ctx, importPlan{
			sandboxUID: sandboxUID,
			sharedDir:  d.stateImportDir(restoreRef, sandboxUID),
			restoreRef: restoreRef, path: bundlePath, marker: marker, markerPresent: markerPresent,
			maxBytes: state.maxBytes,
			subject:  "template " + string(state.kind),
			degrade:  "the clone degrades to the CRD seed",
			importIt: func(path string) error {
				return carrier.ImportTemplateState(ctx, sandboxUID, state.kind, path)
			},
			sentinelMissing:  state.sentinelM,
			sentinelUnlisted: state.sentinelU,
			sentinelMismatch: errPolicyBundleMismatch,
		})
	}
}

// maxVaultBundleBytes bounds the vault bundle file buffered for provenance
// verification. The vault is decoupled from the binding-input cap, so the
// bound is its own (generous) limit; nothing larger is ever legitimate.
const maxVaultBundleBytes = 4 << 20

// maxPolicyBundleBytes bounds the policy bundle file buffered for provenance
// verification (≈1 MiB): the egress caps recorded policy files at
// MaxEncodedPolicyFileBytes (≈68 KiB), so nothing larger is ever legitimate.
const maxPolicyBundleBytes = 1 << 20

// importRestoreVault delivers a restored checkpoint's vault bundle file to
// egress — before the data plane becomes ready. Only a resumed checkpoint
// carries the file (golden-image caches never do); a nil mover is a no-op.
// The committed manifest's platform.stateFiles entry is the provenance:
// an authorized-but-missing file, a read failure, a marker/size/digest
// mismatch, or an import failure all degrade the subject's vault
// fail-closed and observably — mirroring the corrupt vault file posture at
// registration. The Handler reads the bundle file itself; the driver only
// verifies it and hands over the path.
func (d *Driver) importRestoreVault(ctx context.Context, spec fastletapi.SandboxDesiredSpec, restoreRef, vmstatePath, sandboxUID string) {
	if spec.Restore == nil {
		return
	}
	d.mu.RLock()
	mover := d.stateMover
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

	importRestoreBundle(ctx, importPlan{
		sandboxUID: sandboxUID,
		sharedDir:  d.stateImportDir(restoreRef, sandboxUID),
		restoreRef: restoreRef, path: vaultPath, marker: marker, markerPresent: markerPresent,
		maxBytes: maxVaultBundleBytes,
		subject:  "vault",
		degrade:  "the restored Sandbox vault degrades",
		importIt: func(path string) error {
			return mover.ImportState(ctx, sandboxUID, runtimecontract.StateKindVault, path)
		},
		sentinelMissing:  errVaultBundleMissing,
		sentinelUnlisted: errVaultBundleUnlisted,
		sentinelMismatch: errVaultBundleMismatch,
	})
}

// importRestorePolicy delivers a restored checkpoint's recorded-policy bundle
// file to egress — before the data plane becomes ready, so the imported
// record is authoritative over the SET_BINDING replay that seeds the
// bootstrap binding input. Only a resumed checkpoint carries the file; a nil
// mover is a no-op. The committed manifest's policy.json files entry is the
// enforced provenance (sha256 + size, stat-first bounded): anything
// unverified is never delivered, and the restored subject keeps the
// bootstrap binding policy — the pre-transfer semantics, observably.
func (d *Driver) importRestorePolicy(ctx context.Context, spec fastletapi.SandboxDesiredSpec, restoreRef, vmstatePath, sandboxUID string) {
	if spec.Restore == nil {
		return
	}
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return
	}
	bundleDir := filepath.Dir(vmstatePath)
	policyPath := filepath.Join(bundleDir, policyBundleName)

	marker, markerPresent, markerErr := policyManifestMarker(bundleDir)
	if markerErr != nil {
		klog.ErrorS(markerErr, "read the snapshot manifest for the policy provenance failed; the restored Sandbox keeps the bootstrap binding policy",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", policyPath)
		return
	}

	importRestoreBundle(ctx, importPlan{
		sandboxUID: sandboxUID,
		sharedDir:  d.stateImportDir(restoreRef, sandboxUID),
		restoreRef: restoreRef, path: policyPath, marker: marker, markerPresent: markerPresent,
		maxBytes: maxPolicyBundleBytes,
		subject:  "policy",
		degrade:  "the restored Sandbox keeps the bootstrap binding policy",
		importIt: func(path string) error {
			return mover.ImportState(ctx, sandboxUID, runtimecontract.StateKindPolicy, path)
		},
		sentinelMissing:  errPolicyBundleMissing,
		sentinelUnlisted: errPolicyBundleUnlisted,
		sentinelMismatch: errPolicyBundleMismatch,
	})
}

// importPlan carries one restore-side state bundle delivery: the verified
// hand-over of a manifest-provenanced bundle file to the mover.
type importPlan struct {
	sharedDir     string // OCI mounts are container-private; handlers need a shared file.
	sandboxUID    string
	restoreRef    string
	path          string
	marker        *fileMarker
	markerPresent bool
	maxBytes      int64
	// subject names the state slice in log lines ("vault", "policy");
	// degrade names the observable consequence of a failed delivery.
	subject string
	degrade string
	// importIt performs the handler hand-over once provenance holds (the
	// caller's closure captures the mover and the state kind).
	importIt func(string) error
	// sentinels mark the degradation cause in the log line.
	sentinelMissing, sentinelUnlisted, sentinelMismatch error
}

// importRestoreBundle verifies one staged bundle file against its manifest
// provenance and hands it to the mover. Every rejected posture (missing
// file, unlisted file, size/digest mismatch, read or import failure) is a
// logged degradation that never fails the restore: the control plane has
// already resumed the Sandbox, and the subject's state on the handler
// degrades fail-closed (vault) or to the binding-input fallback (policy) —
// both observable.
func importRestoreBundle(ctx context.Context, plan importPlan) {
	info, statErr := os.Stat(plan.path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if plan.markerPresent {
				// The manifest authorizes state but the bundle file is
				// gone (partial cache damage): never a silent skip.
				klog.ErrorS(plan.sentinelMissing, "snapshot manifest authorizes "+plan.subject+" but the bundle file is missing; "+plan.degrade,
					"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
			}
			return //nolint:nilerr // no manifest-listed state: nothing to import
		}
		klog.ErrorS(statErr, "read bundled egress "+plan.subject+" failed; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // already logged
	}
	if !plan.markerPresent {
		klog.ErrorS(plan.sentinelUnlisted, "bundled "+plan.subject+" file is not listed by the snapshot manifest; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // unverified state bytes are never delivered
	}
	// Stat-first bounds: the whole file is never buffered before the size
	// contract holds, so a corrupted/replaced file cannot grow the driver's
	// memory arbitrarily.
	if info.Size() != plan.marker.SizeBytes || info.Size() > plan.maxBytes {
		klog.ErrorS(plan.sentinelMismatch, "bundled "+plan.subject+" size mismatches the manifest entry; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path,
			"stagedBytes", info.Size(), "manifestBytes", plan.marker.SizeBytes)
		return //nolint:nilerr // already logged
	}
	bundleData, readErr := os.ReadFile(plan.path)
	if readErr != nil {
		klog.ErrorS(readErr, "read bundled egress "+plan.subject+" failed; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // already logged
	}
	if plan.marker.SHA256 != artifacts.SHA256Of(bundleData) {
		klog.ErrorS(plan.sentinelMismatch, "bundled "+plan.subject+" digest mismatches the manifest entry; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // unverified state bytes are never delivered
	}
	if importErr := deliverStateBundle(plan, bundleData); importErr != nil {
		klog.ErrorS(importErr, "egress "+plan.subject+" import failed; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef)
	}
}

// stateImportDir selects an ordinary shared directory for OCI state handoff.
// Native bundles already live on the shared filesystem.
func (d *Driver) stateImportDir(reference, sandboxUID string) string {
	if !strings.HasPrefix(reference, ocitemplate.Prefix) {
		return ""
	}
	return filepath.Join(d.config.StateRoot, sandboxStateDir, sandboxUID)
}

func deliverStateBundle(plan importPlan, verified []byte) error {
	if plan.sharedDir == "" {
		return plan.importIt(plan.path)
	}
	staging, err := os.MkdirTemp(plan.sharedDir, "state-import-")
	if err != nil {
		return fmt.Errorf("stage verified state for handler: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			klog.ErrorS(err, "remove temporary state import directory failed", "directory", staging)
		}
	}()
	path := filepath.Join(staging, filepath.Base(plan.path))
	if err := os.WriteFile(path, verified, 0600); err != nil {
		return fmt.Errorf("write verified state for handler: %w", err)
	}
	return plan.importIt(path)
}

// fileMarker is a manifest provenance entry: a sha256 + size pair that
// authorizes exactly one bundle file content.
type fileMarker = artifacts.ManifestFile

// sentinel degradation causes of the restore import; the log line carries
// the specifics.
var (
	errVaultBundleMissing  = errors.New("vault bundle file missing")
	errVaultBundleUnlisted = errors.New("vault bundle file not listed by the manifest")
	errVaultBundleMismatch = errors.New("vault bundle does not match the manifest entry")

	errPolicyBundleMissing  = errors.New("policy bundle file missing")
	errPolicyBundleUnlisted = errors.New("policy bundle file not listed by the manifest")
	errPolicyBundleMismatch = errors.New("policy bundle does not match the manifest entry")
)

// vaultManifestMarker resolves vault provenance from platform.stateFiles.
func vaultManifestMarker(bundleDir string) (*fileMarker, bool, error) {
	return stateFileMarker(bundleDir, vaultBundleName)
}

func policyManifestMarker(bundleDir string) (*fileMarker, bool, error) {
	return stateFileMarker(bundleDir, policyBundleName)
}

func stateFileMarker(bundleDir, name string) (*fileMarker, bool, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, snapshotManifestName))
	if err != nil {
		return nil, false, err
	}
	document, err := artifacts.DecodeVMManifest(raw)
	if err != nil {
		return nil, false, err
	}
	if document.Platform == nil {
		return nil, false, nil
	}
	entry, exists := document.Platform.StateFiles[name]
	if !exists {
		return nil, false, nil
	}
	return &entry, true, nil
}

// stagedBundleFiles validates the optional state bundle files of the staged
// set and returns the full bundle file list. Mirrors the publish side
// (PublishImage): only absence skips a file, any other stat failure fails
// the assembly — a transient error here would otherwise produce a manifest
// that omits a staged file while the publish still uploads the orphaned
// bytes. The state files ride the bundle OPAQUELY: the manifest only
// references them (files + SHA256SUMS), the driver never parses their
// content, and the egress import re-stamps them at restore.
func stagedBundleFiles(staging string) ([]string, error) {
	bundleFiles := []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName}
	for _, bundleName := range []string{vaultBundleName, policyBundleName} {
		if _, statErr := os.Stat(filepath.Join(staging, bundleName)); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			return nil, fmt.Errorf("stat staged %s: %w", bundleName, statErr)
		}
		bundleFiles = append(bundleFiles, bundleName)
	}
	return bundleFiles, nil
}

func manifestSection(document map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	section := map[string]json.RawMessage{}
	if value := document[name]; len(value) > 0 {
		if err := json.Unmarshal(value, &section); err != nil {
			return nil, err
		}
	}
	if section == nil {
		section = map[string]json.RawMessage{}
	}
	return section, nil
}

type snapshotManifestEntries struct {
	Files, StateFiles     map[string]artifacts.ManifestFile
	RootfsSize, SizeBytes int64
	Cache                 map[string]string
	Names                 []string
}

// collectSnapshotManifestEntries describes the staged files of a snapshot
// set. With parallel (chunked transfer mode) known digests (path -> sha256) are
// reused and the remaining files are hashed concurrently before the entries
// are built; otherwise each file is hashed in turn as it is described.
func collectSnapshotManifestEntries(staging string, isOCI bool, known map[string]string, parallel bool) (snapshotManifestEntries, error) {
	bundleFiles, err := stagedBundleFiles(staging)
	if err != nil {
		return snapshotManifestEntries{}, err
	}

	if isOCI {
		bundleFiles = bundleFiles[1:]
	}
	files, stateFiles := map[string]artifacts.ManifestFile{}, map[string]artifacts.ManifestFile{}
	var sizeBytes, rootfsSize int64
	cache := map[string]string{}
	if parallel {
		if cache, err = hashStagedFiles(staging, bundleFiles, known); err != nil {
			return snapshotManifestEntries{}, err
		}
	}
	for _, name := range bundleFiles {
		file, err := artifacts.FileEntry(filepath.Join(staging, name), cache)
		if err != nil {
			return snapshotManifestEntries{}, err
		}
		sizeBytes += file.SizeBytes
		switch name {
		case vaultBundleName, policyBundleName:
			stateFiles[name] = file
		default:
			if !isOCI {
				files[name] = file
			}
		}
		if name == publishedRootfsName {
			rootfsSize = file.SizeBytes
		}
	}
	if isOCI {
		upper, err := ocitemplate.ReadRootfsUpper(staging)
		if err != nil {
			return snapshotManifestEntries{}, err
		}
		rootfsSize = upper.VirtualSizeBytes
		sizeBytes += rootfsSize
	}
	return snapshotManifestEntries{Files: files, StateFiles: stateFiles, RootfsSize: rootfsSize, SizeBytes: sizeBytes, Cache: cache, Names: bundleFiles}, nil
}

func assembleSnapshotManifest(stateRoot, ociRoot, staging, sandboxDir string, kind fastletapi.SnapshotKind) (int64, error) {
	return assembleSnapshotManifestWith(stateRoot, ociRoot, staging, sandboxDir, kind, nil, false)
}

// assembleSnapshotManifestWith is assembleSnapshotManifest with the
// chunked-mode hashing (see collectSnapshotManifestEntries): the set's
// wall time is its largest file, not the sum of all of them.
func assembleSnapshotManifestWith(stateRoot, ociRoot, staging, sandboxDir string, kind fastletapi.SnapshotKind, known map[string]string, parallel bool) (int64, error) {
	state, err := loadState(sandboxDir)
	if err != nil {
		return 0, err
	}
	// The lineage manifest of the dump: the golden image manifest of a fresh
	// boot, or the cached checkpoint manifest when the Sandbox was itself
	// resumed from a checkpoint (the checkpoint carries every lineage field
	// forward, and the original image may not exist on this node at all).
	sourceRef, err := restoreReference(state.Config.Desired)
	if err != nil {
		return 0, err
	}
	raw, err := readSourceManifest(stateRoot, ociRoot, sourceRef, state.Config.Identity.SandboxUID)
	if err != nil {
		return 0, err
	}

	var document map[string]json.RawMessage
	if err = json.Unmarshal(raw, &document); err != nil {
		return 0, err
	}
	isOCI := strings.HasPrefix(sourceRef, ocitemplate.Prefix)
	entries, err := collectSnapshotManifestEntries(staging, isOCI, known, parallel)
	if err != nil {
		return 0, err
	}
	rootfsSize := entries.RootfsSize
	files, stateFiles := entries.Files, entries.StateFiles
	// Modify only the actual disk capacity, preserving unknown machine fields.
	machine, err := manifestSection(document, "machine")
	if err != nil {
		return 0, err
	}
	if rootfsSize > 0 {
		machine["rootfsSizeMiB"], err = json.Marshal((rootfsSize + (1 << 20) - 1) >> 20)
		if err != nil {
			return 0, err
		}
	}
	document["machine"], err = json.Marshal(machine)
	if err != nil {
		return 0, err
	}
	// imageDigest continues to anchor the original build, not the latest dump.
	lineage, err := manifestSection(document, "lineage")
	if err != nil {
		return 0, err
	}
	image, err := json.Marshal(state.Config.Desired.Image)
	if err != nil {
		return 0, err
	}
	lineage["image"] = image
	document["lineage"], err = json.Marshal(lineage)
	if err != nil {
		return 0, err
	}
	platform, err := manifestSection(document, "platform")
	if err != nil {
		return 0, err
	}
	delete(platform, "checkpoint")
	if kind == fastletapi.SnapshotKindCheckpoint {
		platform["checkpoint"], err = json.Marshal(artifacts.CheckpointOwner{SandboxUID: state.Config.Identity.SandboxUID, Namespace: state.Config.Identity.Namespace})
		if err != nil {
			return 0, err
		}
	}
	delete(platform, "stateFiles")
	if len(stateFiles) > 0 {
		platform["stateFiles"], err = json.Marshal(stateFiles)
		if err != nil {
			return 0, err
		}
	}
	document["platform"], err = json.Marshal(platform)
	if err != nil {
		return 0, err
	}
	delete(document, "files")
	if !isOCI {
		document["files"], err = json.Marshal(files)
		if err != nil {
			return 0, err
		}
	}
	if !isOCI {
		if err = artifacts.WriteSHA256SUMS(staging, entries.Names, entries.Cache); err != nil {
			return 0, err
		}
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return 0, err
	}
	if _, err = artifacts.DecodeVMManifest(payload); err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(staging, snapshotManifestName), append(payload, '\n'), 0644); err != nil { //nolint:gosec // published metadata, readable by the runtime-agent
		return 0, err
	}
	return entries.SizeBytes, nil
}

// agentTransferFormatChunked is the runtime-agent's chunked transfer mode
// (agent.TransferFormatChunked; the driver does not import the agent).
const agentTransferFormatChunked = "chunked"

// agentTransferChunked reports whether the runtime-agent last reported the
// chunked transfer mode. An agent client that cannot report it, or an agent
// that predates the setting, means legacy.
func (d *Driver) agentTransferChunked() bool {
	client, err := d.agentClientOrNil()
	if err != nil || client == nil {
		return false
	}
	reporter, ok := client.(transferFormatReporter)
	return ok && reporter.TransferFormat() == agentTransferFormatChunked
}

// relocateSpilledFile copies one spilled dump file into the staging
// directory. In chunked mode the copy hashes as it goes (the bytes are read
// from tmpfs anyway) and leaves all-zero blocks as holes, so the manifest
// assembly neither re-reads guest memory nor the publish re-scans zeros.
func relocateSpilledFile(plan *dumpPlan, source, target string) error {
	if !plan.chunked {
		return copyFile(source, target)
	}
	sum, err := copyFileSparseHashed(source, target)
	if err != nil {
		return err
	}
	if plan.knownSums == nil {
		plan.knownSums = map[string]string{}
	}
	plan.knownSums[target] = sum
	return nil
}

// hashStagedFiles returns the checksum cache of the named staged files
// (path -> sha256): known digests are taken as-is, the rest are hashed
// concurrently.
func hashStagedFiles(staging string, names []string, known map[string]string) (map[string]string, error) {
	cache := map[string]string{}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	for _, name := range names {
		path := filepath.Join(staging, name)
		if sum, ok := known[path]; ok {
			cache[path] = sum
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sum, err := artifacts.SHA256File(path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("checksum %s: %w", name, err)
				}
				return
			}
			cache[path] = sum
		}()
	}
	wg.Wait()
	return cache, firstErr
}

// copyFileSparseHashed copies source to target and returns the sha256 of the
// content. All-zero 64 KiB blocks are seeked over instead of written, so the
// target is sparse while its logical content (and digest) is unchanged.
func copyFileSparseHashed(source, target string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	buffer := make([]byte, 4<<20)
	var size int64
	for {
		n, readErr := io.ReadFull(input, buffer)
		block := buffer[:n]
		_, _ = digest.Write(block)
		for len(block) > 0 {
			step := min(len(block), 64<<10)
			if artifacts.IsZero(block[:step]) {
				_, err = output.Seek(int64(step), io.SeekCurrent)
			} else {
				_, err = output.Write(block[:step])
			}
			if err != nil {
				_ = output.Close()
				return "", err
			}
			block = block[step:]
		}
		size += int64(n)
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			_ = output.Close()
			return "", readErr
		}
	}
	// A trailing hole has no write behind it: set the logical size.
	if err := output.Truncate(size); err != nil {
		_ = output.Close()
		return "", err
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func readSourceManifest(stateRoot, ociRoot, image, sandboxID string) ([]byte, error) {
	var payload []byte
	var err error
	if strings.HasPrefix(image, ocitemplate.Prefix) {
		payload, err = readPreparedOCIManifest(ociRoot, sandboxID)
	} else {
		payload, err = os.ReadFile(cachedManifestPath(stateRoot, image))
	}
	if err != nil {
		return nil, fmt.Errorf("read source image manifest: %w", err)
	}
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if document.GuestNetwork == nil {
		return nil, fmt.Errorf("%w: source image manifest carries no guestNetwork", ErrInvalidConfig)
	}
	return payload, nil
}

// OCI pack paths are Pod-private so cleanup cannot affect a replacement Pod.
func snapshotStagingPath(stateRoot, podUID, snapshotID string, oci bool) string {
	if oci {
		return filepath.Join(stateRoot, snapshotStagingDir, podUID, snapshotID)
	}
	return filepath.Join(stateRoot, snapshotStagingDir, snapshotID)
}
