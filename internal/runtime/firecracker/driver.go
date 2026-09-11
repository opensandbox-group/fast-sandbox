package firecracker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	infracontract "fast-sandbox/internal/infra/contract"
	"fast-sandbox/internal/nodecleanup"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

// bootPollInterval is the VM state polling interval after start/resume.
const bootPollInterval = 250 * time.Millisecond

// Driver boots one Firecracker microVM on demand per Sandbox create request.
// The VM runs in the Fastlet Pod; nothing is pre-warmed.
type Driver struct {
	mu             sync.RWMutex
	profile        runtimecatalog.RuntimeProfile
	config         runtimecatalog.FirecrackerConfig
	namespace      string
	podUID         string
	initialized    bool
	runner         fastletnetwork.CommandRunner
	launcher       ProcessRunner
	newClient      func(socketPath string) *Client
	stat           func(string) (os.FileInfo, error)
	killProcess    func(pid int) error
	probeProcess   func(pid int) error
	waitSocket     func(ctx context.Context, socketPath string, timeout time.Duration) error
	networkManager *fastletnetwork.Manager
	infraMgr       *fastletinfra.Manager
	prepareInfra   func(ctx context.Context, config *fastletapi.RuntimeSandboxConfig) (fastletinfra.PreparedInstance, error)
	processes      map[string]Process
	// agentSocket, newAgentClient, and agentClient wire the node-level
	// runtime-agent (agent_wiring.go): an empty socket means local mode, a
	// nil newAgentClient disables the agent client, and agentClient is the
	// lazily built cached client.
	agentSocket    string
	newAgentClient func(socketPath string) (AgentClient, error)
	agentClient    AgentClient
	// sandboxLeases records the runtime-agent lease of each Sandbox that
	// requested one (populated by LeaseDevices; empty in the native stage).
	sandboxLeases map[string]string
	// imageGCInterval is the period of the independent cache GC loop; it is a
	// field (not a constant) so tests can shorten it.
	imageGCInterval time.Duration
	// imageCacheLimitBytes is the cache size the GC enforces by evicting
	// unreferenced images in least-frequently-used order.
	imageCacheLimitBytes int64
	gcStop               chan struct{}
	gcTrigger            chan struct{}
	// imageUseCount records how often each cached image was pulled or booted,
	// in memory: the GC evicts by this instead of filesystem timestamps.
	// Guarded by mu.
	imageUseCount map[string]int64
	// imageDeliveries tracks the asynchronous artifact delivery of images
	// requested by DeliverImage (image_delivery.go). The map itself is
	// guarded by mu; each tracker has its own mutex.
	imageDeliveries map[string]*imageDelivery
	// imageDeliveryAttemptTimeout and imageDeliveryFailureWindow tune the
	// background delivery; zero values select the package defaults.
	imageDeliveryAttemptTimeout time.Duration
	imageDeliveryFailureWindow  time.Duration
	// nodeCleanup is the node janitor client for residual VMM cleanup after
	// DeleteSandbox (set by fastlet when the profile requires it; nil in
	// local/host mode).
	nodeCleanup nodecleanup.RuntimeProcessCleaner
	// snapshotCapacityWait overrides the GC self-heal window of the
	// snapshot staging capacity gate (tests shorten it; 0 selects the
	// default).
	snapshotCapacityWait time.Duration
}

// defaultImageGCInterval bounds the image cache by usage without coupling GC
// to Sandbox lifecycle events.
const defaultImageGCInterval = time.Hour

// New validates the runtime profile configuration and returns the driver.
func New(profile runtimecatalog.RuntimeProfile) (*Driver, error) {
	if profile.Firecracker == nil {
		return nil, fmt.Errorf("firecracker runtime profile %q has no private configuration", profile.Name)
	}
	return &Driver{
		profile: profile, config: *profile.Firecracker,
		runner: fastletnetwork.ExecRunner{}, launcher: ExecProcessRunner{},
		newClient: NewClient, stat: os.Stat, killProcess: killPID, probeProcess: pidAlive,
		waitSocket: waitForAPISocket, processes: make(map[string]Process),
		imageGCInterval:      defaultImageGCInterval,
		imageCacheLimitBytes: defaultImageCacheLimitBytes,
	}, nil
}

// Initialize validates the boot configuration, prepares the StateRoot, and
// starts the independent image cache GC loop.
func (d *Driver) Initialize(_ context.Context, _ string) error {
	d.mu.Lock()
	if d.initialized {
		d.mu.Unlock()
		return nil
	}
	if err := validateConfig(d.config); err != nil {
		d.mu.Unlock()
		return err
	}
	if err := os.MkdirAll(d.config.StateRoot, 0o750); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("prepare Firecracker StateRoot: %w", err)
	}
	d.initialized = true
	interval := d.imageGCInterval
	d.gcStop = make(chan struct{})
	d.gcTrigger = make(chan struct{}, 1)
	stop := d.gcStop
	// Cache entries already present at startup start with one recorded use so
	// the LFU eviction does not single them out before any pull or boot.
	d.imageUseCount = make(map[string]int64)
	if cached, err := listCachedImages(d.config.StateRoot); err == nil {
		for _, digest := range cached {
			d.imageUseCount[digest] = 1
		}
	}
	d.mu.Unlock()
	go d.imageGCLoop(interval, stop)
	return nil
}

// touchImage records a use of a cached image for the LFU eviction order. It
// is called when an image is pulled or booted.
func (d *Driver) touchImage(image string) {
	d.mu.Lock()
	if d.imageUseCount != nil {
		d.imageUseCount[imageKey(image)]++
	}
	d.mu.Unlock()
}

// TriggerImageGC requests an out-of-band collection from the independent GC
// loop. It is non-blocking; a collection already in flight coalesces the
// request. The loop remains independent of Sandbox lifecycle events.
func (d *Driver) TriggerImageGC() {
	d.mu.RLock()
	trigger := d.gcTrigger
	d.mu.RUnlock()
	if trigger == nil {
		return
	}
	select {
	case trigger <- struct{}{}:
	default:
	}
}

// imageGCLoop periodically collects unreferenced cached images. It runs
// independently of Sandbox lifecycle events and stops when Close closes the
// stop channel.
func (d *Driver) imageGCLoop(interval time.Duration, stop <-chan struct{}) {
	d.gcImageCache()
	for {
		d.mu.RLock()
		trigger := d.gcTrigger
		d.mu.RUnlock()
		select {
		case <-stop:
			return
		case <-trigger:
			d.gcImageCache()
		case <-time.After(interval):
			d.gcImageCache()
		}
	}
}

// gcImageCache drops cached rootfs images that no managed Sandbox references
// and that have been idle beyond the grace period. Failures are logged and
// never fail the surrounding operation.
func (d *Driver) gcImageCache() {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	// Snapshot the use counts under the lock: garbageCollectImages runs
	// outside it while PullImage and boot (touchImage) keep writing the
	// map, so the iteration must not touch the live map.
	useCount := make(map[string]int64, len(d.imageUseCount))
	for digest, uses := range d.imageUseCount {
		useCount[digest] = uses
	}
	limitBytes := d.imageCacheLimitBytes
	d.mu.RUnlock()
	if limitBytes <= 0 {
		limitBytes = defaultImageCacheLimitBytes
	}
	removed, err := garbageCollectImages(stateRoot, limitBytes, useCount)
	if err != nil {
		klog.V(2).InfoS("firecracker image cache GC skipped", "err", err)
		return
	}
	if len(removed) > 0 {
		klog.InfoS("firecracker image cache GC removed unreferenced images", "digests", removed)
	}
}

func validateConfig(config runtimecatalog.FirecrackerConfig) error {
	if config.BinaryPath == "" || config.KernelPath == "" || config.RootfsPath == "" || config.StateRoot == "" {
		return fmt.Errorf("%w: firecracker binary, kernel, rootfs, and state root are required", ErrInvalidConfig)
	}
	if config.DefaultVCPUs < 1 || config.DefaultMemory == "" || config.BootTimeoutSeconds < 1 {
		return fmt.Errorf("%w: firecracker boot profile requires vCPUs, memory, and boot timeout", ErrInvalidConfig)
	}
	return nil
}

// SetNamespace records the Fastlet namespace that owns the managed Sandboxes.
func (d *Driver) SetNamespace(namespace string) {
	d.mu.Lock()
	d.namespace = namespace
	d.mu.Unlock()
}

// SetNetworkManager wires the Fastlet-owned slot manager. Each slot carries
// the pod-side IP and the guest tap prepared by GuestVMNetNSDriver.
func (d *Driver) SetNetworkManager(manager *fastletnetwork.Manager) {
	d.mu.Lock()
	d.networkManager = manager
	d.mu.Unlock()
}

// RuntimeResourceAvailable implements runtimecontract.ResourceAdmission:
// admission is gated on a clean network slot. Released slots are replaced by
// Replenish asynchronously (netns + rules take ~15 ms), so burst creates
// during that window are rejected BEFORE side effects (the control plane
// retries) instead of failing mid-EnsureSandbox with ErrNoCleanSlot.
func (d *Driver) RuntimeResourceAvailable() bool {
	d.mu.RLock()
	manager := d.networkManager
	d.mu.RUnlock()
	return manager != nil && manager.Snapshot().Clean > 0
}

// SetInfraManager wires the prepared Infra Component plan. Artifacts are
// copied into the per-instance guest rootfs before boot (GuestCopy delivery).
func (d *Driver) SetInfraManager(manager *fastletinfra.Manager) {
	d.mu.Lock()
	d.infraMgr = manager
	d.prepareInfra = manager.PrepareInstance
	d.mu.Unlock()
}

// SetNodeCleanupClient wires the node janitor for residual VMM cleanup.
// Fastlet calls it when the runtime profile requires node process cleanup
// (ResidualProcessFirecracker); local/host runs leave it nil.
func (d *Driver) SetNodeCleanupClient(client nodecleanup.RuntimeProcessCleaner) {
	d.mu.Lock()
	d.nodeCleanup = client
	d.mu.Unlock()
}

// ensureResidualProcessAbsent asks the node janitor to terminate any
// firecracker VMM of this Sandbox that survived the driver's own teardown
// (identified by --id <sandboxID>). Best-effort: the driver already stopped
// the VM, so a cleanup failure is logged, not fatal.
func (d *Driver) ensureResidualProcessAbsent(ctx context.Context, sandboxID string) {
	d.mu.RLock()
	client := d.nodeCleanup
	d.mu.RUnlock()
	if client == nil {
		return
	}
	if err := client.EnsureRuntimeProcessesAbsent(ctx, runtimecatalog.ResidualProcessFirecracker, truncatedSandboxID(sandboxID)); err != nil {
		klog.V(2).InfoS("firecracker residual process cleanup skipped", "sandboxId", sandboxID, "err", err)
	}
}

// ProbeCapabilities reports host dependencies (KVM, tap device, binary,
// kernel) and the runtime-agent health when one is configured. The profile
// gate keeps the runtime fail-closed until the KVM E2E suite passes.
func (d *Driver) ProbeCapabilities(ctx context.Context) CapabilityReport {
	d.mu.RLock()
	profile := d.profile
	config := d.config
	stat := d.stat
	d.mu.RUnlock()

	report := CapabilityReport{Runtime: profile.Name, ProfileHash: profile.ProfileHash, State: runtimecatalog.CapabilityReady}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityUnsupported {
		report.State = runtimecatalog.CapabilityUnsupported
		report.Reason = profile.Capabilities.Reason
		report.Message = "firecracker runtime profile is registered but its production capability gate is not enabled"
		return report
	}
	checks := []struct {
		path   string
		reason string
	}{
		{"/dev/kvm", "KVMUnavailable"},
		{"/dev/net/tun", "TapDeviceUnavailable"},
		{config.BinaryPath, "RuntimeBinaryUnavailable"},
		{config.KernelPath, "RuntimeKernelUnavailable"},
	}
	if config.JailerPath != "" {
		checks = append(checks, struct {
			path   string
			reason string
		}{config.JailerPath, "RuntimeJailerUnavailable"})
	}
	for _, check := range checks {
		if _, err := stat(check.path); err != nil {
			report.Missing = append(report.Missing, check.path)
			if report.Reason == "" {
				report.Reason = check.reason
			}
		}
	}
	if len(report.Missing) > 0 {
		report.State = runtimecatalog.CapabilityDegraded
		report.Message = fmt.Sprintf("firecracker runtime dependencies are unavailable: %v", report.Missing)
		return report
	}
	agent, err := d.agentClientOrNil()
	if err != nil {
		report.State = runtimecatalog.CapabilityDegraded
		report.Reason = "AgentUnavailable"
		report.Message = fmt.Sprintf("firecracker runtime-agent client error: %v", err)
		return report
	}
	if agent != nil {
		healthCtx, cancel := context.WithTimeout(ctx, agentHealthTimeout)
		defer cancel()
		if err := agent.Health(healthCtx); err != nil {
			report.State = runtimecatalog.CapabilityDegraded
			report.Reason = "AgentUnavailable"
			report.Message = err.Error()
			return report
		}
	}
	report.Reason = "RuntimeDriverReady"
	report.Message = "firecracker runtime host dependencies are ready"
	return report
}

// EnsureSandbox boots one Firecracker microVM on demand. The call is
// idempotent and emits an OTel span tree correlated by the Sandbox identity.
func (d *Driver) EnsureSandbox(ctx context.Context, input *fastletapi.EnsureSandboxInput) (_ *SandboxMetadata, resultErr error) {
	if input == nil {
		return nil, fmt.Errorf("%w: Firecracker Sandbox input is required", ErrInvalidConfig)
	}
	config := &input.Sandbox
	identity := config.Identity
	spec := config.Spec
	if identity.SandboxUID == "" || identity.FastletPodUID == "" ||
		identity.InstanceGeneration <= 0 || identity.RuntimeInstanceID == "" || identity.AssignmentAttempt <= 0 {
		return nil, fmt.Errorf("%w: complete Firecracker Sandbox identity is required", ErrInvalidConfig)
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: input.RequestID, Namespace: identity.Namespace, SandboxName: identity.Name,
		SandboxUID: identity.SandboxUID, FastletPodUID: identity.FastletPodUID,
		InstanceGeneration: identity.InstanceGeneration, AssignmentAttempt: identity.AssignmentAttempt,
	})
	ctx, createSpan := observability.Start(ctx, "fastlet.firecracker.create")
	infraPrepared := false
	defer func() {
		observability.End(createSpan, resultErr)
		if resultErr != nil && infraPrepared && d.infraMgr != nil {
			_ = d.infraMgr.RemoveInstance(config)
		}
	}()

	d.mu.RLock()
	stateRoot := d.config.StateRoot
	manager := d.networkManager
	d.mu.RUnlock()
	if manager == nil {
		return nil, fmt.Errorf("%w: firecracker requires the Fastlet network manager", ErrNetworkUnavailable)
	}

	// Image readiness: a create whose image is not yet cached never blocks
	// on the node-side pull. Fastlet routes cold creates through the
	// driver's asynchronous delivery (DeliverImage): the artifact set is
	// pulled in the background and the Sandbox is booted once the commit
	// point appears in the local cache. The check covers the COMPLETE
	// restore set (rootfs + vmstate + memory) and runs BEFORE the network
	// slot is acquired: a partially delivered cache reports
	// ErrImageNotReady here instead of burning a slot on the restore-path
	// checks below, so a parked boot worker polling during the delivery
	// window cannot exhaust the slot pool. Local mode (no agent socket)
	// keeps the pre-warmed behavior: a still-missing image reports
	// ErrImageNotReady and the create fails exactly as before.
	if err := verifyRestorableImage(stateRoot, spec.Image); err != nil {
		return nil, err
	}

	directory, err := ensureSandboxDir(stateRoot, identity.SandboxUID)
	if err != nil {
		return nil, err
	}

	if existing, err := loadState(directory); err == nil {
		if alive, probeErr := d.probeVM(ctx, existing); probeErr == nil && alive {
			if runtimecontract.SameRuntimeIdentity(existing.Config.Identity, identity) {
				if err := validateExistingRuntimeProfile(existingMetadata(existing), config); err != nil {
					return nil, err
				}
				return existingMetadata(existing), nil
			}
			klog.InfoS("Replacing stale Firecracker runtime owned by a previous Sandbox instance",
				"sandbox", identity.SandboxUID,
				"existingRuntimeInstanceID", existing.Config.Identity.RuntimeInstanceID,
				"requestedRuntimeInstanceID", identity.RuntimeInstanceID,
				"existingAssignmentAttempt", existing.Config.Identity.AssignmentAttempt,
				"requestedAssignmentAttempt", identity.AssignmentAttempt)
		}
		if err := d.cleanupStale(ctx, directory, existing); err != nil {
			return nil, err
		}
		// The stale state directory was removed; recreate it for the fresh boot.
		directory, err = ensureSandboxDir(stateRoot, identity.SandboxUID)
		if err != nil {
			return nil, err
		}
	}

	owner := d.networkOwner(config)
	createStarted := time.Now()
	acquireCtx, acquireSpan := observability.Start(ctx, "fastlet.firecracker.acquire")
	slot, err := manager.Acquire(acquireCtx, owner)
	observability.End(acquireSpan, err)
	if err != nil {
		snapshot := manager.Snapshot()
		klog.ErrorS(err, "firecracker network slot acquire failed",
			"sandboxId", identity.SandboxUID,
			"generation", identity.InstanceGeneration, "attempt", identity.AssignmentAttempt,
			"capacity", snapshot.Capacity, "clean", snapshot.Clean,
			"bound", snapshot.Bound, "destroying", snapshot.Destroying)
		return nil, fmt.Errorf("%w: acquire Firecracker network slot: %v", ErrNetworkUnavailable, err)
	}
	acquireDur := time.Since(createStarted)
	releaseSlot := func() {
		_ = manager.Release(context.Background(), owner)
	}

	// The per-restore guest data plane: every slot netns translates its
	// slot IP to the SAME baked guest address (clone model). The address
	// comes from the cached manifest guestNetwork; the BakedGuestIP
	// convention is the fallback for hand-seeded caches. Slots are prepared
	// before the image is known, so the NAT rules are applied now.
	guestIP, guestMTU, err := resolveBakedGuestIP(stateRoot, spec.Image, slot)
	if err != nil {
		releaseSlot()
		return nil, err
	}
	if guestMTU > 0 && guestMTU != slot.MTU {
		// The guest MTU is frozen in the snapshot while the host side (netns
		// eth0, guest tap) carries the slot MTU. A mismatch relies on
		// PMTUD for every large transfer: oversized guest frames are dropped
		// or fragmented until an ICMP needfrag recovers the path, which
		// surfaces as intermittent network IO stalls.
		klog.Warningf("baked guest MTU %d differs from slot MTU %d (sandbox %s, image %s); align the template or FAST_SANDBOX_NETWORK_MTU to avoid PMTU-dependent stalls",
			guestMTU, slot.MTU, identity.SandboxUID, spec.Image)
	}
	if err := manager.ApplyGuest(ctx, owner, guestIP); err != nil {
		releaseSlot()
		return nil, fmt.Errorf("%w: apply Firecracker guest data plane: %v", ErrNetworkUnavailable, err)
	}

	rootfsStarted := time.Now()
	_, rootfsSpan := observability.Start(ctx, "fastlet.firecracker.rootfs")
	vmstatePath, memoryPath, err := resolveRestoreSnapshotFiles(stateRoot, spec.Image)
	// The machine tuple of the golden snapshot is baked in the vmstate
	// (v1.16 restores it from the snapshot); the manifest values are only
	// validated here, not applied via the API (any machine-config call
	// before snapshot/load is rejected).
	if err == nil {
		err = validateRestoreMachineConfig(spec, d.config, stateRoot, spec.Image)
	}
	var instanceRootfs, jailRoot, apiAddress string
	if err == nil {
		instanceRootfs, jailRoot, apiAddress, err = d.prepareInstance(
			stateRoot, identity.SandboxUID, spec.Image, directory, vmstatePath, memoryPath,
		)
	}
	observability.End(rootfsSpan, err)
	if err != nil {
		releaseSlot()
		return nil, err
	}
	// A failed Create must not leak the jail and per-sandbox state
	// directories created above: every failure between here and the
	// successful return removes them (the VMM is killed on each failure
	// path before this defer runs, which removeJailRoot requires).
	createdComplete := false
	defer func() {
		if createdComplete {
			return
		}
		if jailRoot != "" {
			d.removeJailRoot(identity.SandboxUID)
		}
		_ = removeSandboxDir(directory)
		klog.InfoS("firecracker Create cleanup removed partial sandbox",
			"sandboxId", identity.SandboxUID, "jailRoot", jailRoot)
	}()
	d.touchImage(spec.Image)
	rootfsDur := time.Since(rootfsStarted)
	rootfsMiB := float64(0)
	if info, statErr := os.Stat(instanceRootfs); statErr == nil {
		rootfsMiB = float64(info.Size()) / (1024 * 1024)
	}

	// Infra Components: prepare the instance plan and GuestCopy the artifacts
	// into the per-instance rootfs before boot.
	var infraServices []infracontract.ServiceEndpoint
	var infraDiagnostics []infracontract.ComponentDiagnostic
	infraDur := time.Duration(0)
	if d.infraMgr != nil {
		infraStarted := time.Now()
		infraCtx, infraSpan := observability.Start(ctx, "fastlet.firecracker.infra")
		instance, prepareErr := d.prepareInfra(infraCtx, config)
		if prepareErr == nil {
			prepareErr = deliverGuestInfra(infraCtx, d.runner, instanceRootfs, instance)
		}
		observability.End(infraSpan, prepareErr)
		if prepareErr != nil {
			_ = d.infraMgr.RemoveInstance(config)
			releaseSlot()
			return nil, fmt.Errorf("%w: prepare Infra Components: %v", ErrInfraUnavailable, prepareErr)
		}
		// Infra delivery is the only per-instance rootfs mutation left:
		// the guest DNS resolver is no longer injected here — the template
		// bakes /etc/resolv.conf next to the guest network constants it
		// targets (see cmd/sandboxtemplate-builder convert.go), so egress
		// pools inherit the gateway resolver without pre-boot image writes.
		infraServices = instance.Services
		infraDiagnostics = instance.Diagnostics
		infraPrepared = true
		infraDur = time.Since(infraStarted)
	}

	// Restore-only startup: the golden snapshot carries the guest network
	// configuration (the preparation VM's static IP is baked into the guest
	// state), and the NIC host tap is replaced per instance via the load
	// request's network_overrides. Nothing else is injected into the rootfs.
	state := &SandboxState{
		Config: *config,
		Allocation: fastletapi.RuntimeAllocation{Network: fastletapi.NetworkAllocation{
			SlotID: slot.ID, NamespacePath: slot.HostNetNSPath, IP: slot.IP, Gateway: slot.Gateway,
			DNSPath: slot.DNSPath, PrivateCIDR: slot.PrivateCIDR, HostVeth: slot.HostVeth,
		}},
		Phase:      PhaseStarting,
		APIAddress: apiAddress, CreatedAt: time.Now().Unix(),
		InfraServices: infraServices, InfraDiagnostics: infraDiagnostics,
	}
	if err := saveState(directory, state); err != nil {
		releaseSlot()
		return nil, err
	}

	// The process log lands next to the instance rootfs: the jail root in
	// jailer mode, the state directory in direct mode.
	logDir := directory
	if jailRoot != "" {
		logDir = jailRoot
	}

	launchCtx, launchSpan := observability.Start(ctx, "fastlet.firecracker.launch")
	launchStarted := time.Now()
	process, err := d.launchVM(launchCtx, launchConfig{
		BinaryPath: d.config.BinaryPath,
		SandboxID:  identity.SandboxUID,
		APIAddress: apiAddress,
		WorkingDir: directory,
		LogPath:    filepath.Join(logDir, processLogName),
	}, slot)
	if err == nil {
		state.PID = process.PID()
		d.rememberProcess(identity.SandboxUID, process)
		if saveErr := saveState(directory, state); saveErr != nil {
			d.killAndForget(identity.SandboxUID, process.PID())
			releaseSlot()
			observability.End(launchSpan, saveErr)
			return nil, saveErr
		}
		err = d.waitSocket(launchCtx, state.APIAddress, firecrackerSocketWaitTimeout)
		if err != nil {
			detail := readProcessLog(logDir)
			d.killAndForget(identity.SandboxUID, process.PID())
			releaseSlot()
			observability.End(launchSpan, err)
			return nil, fmt.Errorf("%w: firecracker API socket did not appear: %v%s", ErrRuntimeNotInitialized, err, detail)
		}
	}
	observability.End(launchSpan, err)
	if err != nil {
		releaseSlot()
		return nil, err
	}
	launchDur := time.Since(launchStarted)

	client := d.newClient(state.APIAddress)
	defer client.Close()
	configureStarted := time.Now()
	configureCtx, configureSpan := observability.Start(ctx, "fastlet.firecracker.configure")
	err = configureRestoreVM(configureCtx, client, slot, vmstatePath, memoryPath, jailRoot != "")
	observability.End(configureSpan, err)
	if err != nil {
		d.killAndForget(identity.SandboxUID, process.PID())
		releaseSlot()
		return nil, err
	}
	configureDur := time.Since(configureStarted)

	bootStarted := time.Now()
	bootCtx, bootSpan := observability.Start(ctx, "fastlet.firecracker.resume")
	polls, err := resumeVM(bootCtx, client, d.config.BootTimeoutSeconds)
	observability.End(bootSpan, err)
	if err != nil {
		d.killAndForget(identity.SandboxUID, process.PID())
		releaseSlot()
		return nil, err
	}
	bootDur := time.Since(bootStarted)

	state.Phase = PhaseRunning
	state.StageDurations = map[string]time.Duration{
		"acquire": acquireDur, "rootfs": rootfsDur, "infra": infraDur,
		"launch": launchDur, "configure": configureDur, "boot": bootDur,
	}
	if err := saveState(directory, state); err != nil {
		d.killAndForget(identity.SandboxUID, process.PID())
		releaseSlot()
		return nil, err
	}
	klog.InfoS("firecracker sandbox created",
		"sandboxId", identity.SandboxUID,
		"total", time.Since(createStarted).String(),
		"acquire", acquireDur.String(),
		"rootfs", rootfsDur.String(), "rootfsMiB", fmt.Sprintf("%.1f", rootfsMiB),
		"infra", infraDur.String(),
		"launch", launchDur.String(),
		"configure", configureDur.String(),
		"boot", bootDur.String(), "vmStatePolls", polls,
	)
	createdComplete = true
	return existingMetadata(state), nil
}

// InspectSandbox returns the metadata of a managed microVM. An unreachable
// VM is reported with Phase Stopped instead of an error so Fastlet can
// reconcile the real state.
func (d *Driver) InspectSandbox(ctx context.Context, sandboxID string) (_ *SandboxMetadata, resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxUID: sandboxID})
	ctx, span := observability.Start(ctx, "fastlet.firecracker.inspect")
	defer func() { observability.End(span, resultErr) }()
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	directory, err := sandboxDir(stateRoot, sandboxID)
	if err != nil {
		return nil, err
	}
	state, err := loadState(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrSandboxNotFound
		}
		return nil, err
	}
	if alive, probeErr := d.probeVM(ctx, state); probeErr == nil && !alive {
		state.Phase = PhaseStopped
	}
	return existingMetadata(state), nil
}

// DeleteSandbox stops the microVM, releases its network slot, and removes the
// Sandbox state. The call is idempotent.
func (d *Driver) DeleteSandbox(ctx context.Context, sandboxID string) (resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxUID: sandboxID})
	ctx, span := observability.Start(ctx, "fastlet.firecracker.delete")
	defer func() { observability.End(span, resultErr) }()
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	manager := d.networkManager
	d.mu.RUnlock()
	directory, err := sandboxDir(stateRoot, sandboxID)
	if err != nil {
		return err
	}
	state, err := loadState(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	d.killAndForget(sandboxID, state.PID)
	if manager != nil && state.Config.Identity.SandboxUID != "" {
		// Release is reached unconditionally: a slot stuck in Destroying by an
		// earlier failed release must be retried here, and Release matches
		// Destroying leftovers itself (the old Lookup-only-Bound guard made
		// such slots unreachable until a Fastlet restart).
		if err := manager.Release(ctx, d.networkOwner(&state.Config)); err != nil {
			klog.ErrorS(err, "DeleteSandbox: network slot release failed; sandbox state retained for delete retry",
				"sandboxId", sandboxID, "stateDirectory", directory)
			return fmt.Errorf("release network slot of sandbox %s: %w", sandboxID, err)
		}
	}
	d.mu.RLock()
	infraMgr := d.infraMgr
	d.mu.RUnlock()
	if infraMgr != nil {
		_ = infraMgr.RemoveSandboxInstances(sandboxID)
	}
	d.releaseAgentSandbox(ctx, sandboxID, state.Config.Spec.Image)
	d.removeJailRoot(sandboxID)
	d.ensureResidualProcessAbsent(ctx, sandboxID)
	_ = removeSandboxDir(directory)
	klog.Infof("firecracker sandbox %s deleted", sandboxID)
	return nil
}

// ListManagedSandboxes returns the Sandboxes managed by this Fastlet in the
// configured namespace.
func (d *Driver) ListManagedSandboxes(_ context.Context) ([]*SandboxMetadata, error) {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	namespace := d.namespace
	d.mu.RUnlock()
	directories, err := listSandboxDirs(stateRoot)
	if err != nil {
		return nil, err
	}
	managed := make([]*SandboxMetadata, 0, len(directories))
	for _, directory := range directories {
		state, err := loadState(directory)
		if err != nil || (namespace != "" && state.Config.Identity.Namespace != namespace) {
			continue
		}
		managed = append(managed, existingMetadata(state))
	}
	return managed, nil
}

// RecoverRuntimeResources cleans up VMs that died with a Fastlet restart:
// Firecracker processes are pod-local, so stale records are removed and
// their slots released; surviving VMs are returned.
func (d *Driver) RecoverRuntimeResources(ctx context.Context, managed []*SandboxMetadata) error {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	manager := d.networkManager
	d.mu.RUnlock()
	directories, err := listSandboxDirs(stateRoot)
	if err != nil {
		return err
	}
	for _, directory := range directories {
		state, err := loadState(directory)
		if err != nil {
			continue
		}
		alive, probeErr := d.probeVM(ctx, state)
		if probeErr != nil || !alive {
			if manager != nil {
				sandboxUID := state.Config.Identity.SandboxUID
				if slot, exists := manager.Lookup(sandboxUID); exists {
					_ = slot
					_ = manager.Release(ctx, d.networkOwner(&state.Config))
				}
			}
			d.killAndForget(state.Config.Identity.SandboxUID, state.PID)
			d.removeJailRoot(state.Config.Identity.SandboxUID)
			_ = removeSandboxDir(directory)
			continue
		}
		// A Fastlet crash during a snapshot dump leaves the VM paused (the
		// Snapshotter contract only covers in-process failure paths). The
		// surviving VMM is resumed here so recovery never strands a guest.
		d.resumePausedVM(ctx, state)
	}
	return nil
}

// resumePausedVM resumes a live but paused microVM (snapshot-dump crash
// recovery). The VM state is polled first: resuming a Running VM is a
// Firecracker API error, so only a confirmed Paused state is resumed.
func (d *Driver) resumePausedVM(ctx context.Context, state *SandboxState) {
	if state == nil || state.APIAddress == "" {
		return
	}
	client := d.newClient(state.APIAddress)
	defer client.Close()
	vmState, err := client.VMState(ctx)
	if err != nil || vmState != "Paused" {
		return
	}
	if _, err := resumeVM(ctx, client, d.bootTimeoutOrDefault()); err != nil {
		klog.ErrorS(err, "Resume paused microVM after crash recovery failed; Sandbox remains paused",
			"sandboxId", state.Config.Identity.SandboxUID)
		return
	}
	klog.InfoS("Resumed paused microVM after crash recovery", "sandboxId", state.Config.Identity.SandboxUID)
}

// bootTimeoutOrDefault returns the configured boot (resume) poll timeout.
func (d *Driver) bootTimeoutOrDefault() int32 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.config.BootTimeoutSeconds > 0 {
		return d.config.BootTimeoutSeconds
	}
	return 60
}

// GetAccessDescriptor returns the pod-side DirectIP descriptor of the Sandbox.
func (d *Driver) GetAccessDescriptor(sandboxID string) (dataplane.AccessDescriptor, error) {
	d.mu.RLock()
	manager := d.networkManager
	d.mu.RUnlock()
	if manager == nil {
		return dataplane.AccessDescriptor{}, ErrNetworkUnavailable
	}
	slot, exists := manager.Lookup(sandboxID)
	if !exists {
		return dataplane.AccessDescriptor{}, ErrSandboxNotFound
	}
	if err := slot.Access.Validate(); err != nil {
		return dataplane.AccessDescriptor{}, fmt.Errorf("%w: %v", ErrNetworkUnavailable, err)
	}
	return slot.Access, nil
}

// Close stops every managed microVM and releases driver state.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for sandboxID, process := range d.processes {
		_ = process.Kill()
		delete(d.processes, sandboxID)
	}
	if d.gcStop != nil {
		close(d.gcStop)
		d.gcStop = nil
	}
	d.agentClient = nil
	d.sandboxLeases = nil
	d.initialized = false
	return nil
}

// probeVM reports whether the Firecracker process and its API socket are
// alive; the PID check rejects stale records with recycled identifiers.
func (d *Driver) probeVM(ctx context.Context, state *SandboxState) (bool, error) {
	if state == nil || state.APIAddress == "" {
		return false, ErrInvalidConfig
	}
	d.mu.RLock()
	probeProcess := d.probeProcess
	d.mu.RUnlock()
	if state.PID > 0 {
		if err := probeProcess(state.PID); err != nil {
			return false, nil
		}
	}
	client := d.newClient(state.APIAddress)
	defer client.Close()
	if _, err := client.Version(ctx); err != nil {
		return false, nil
	}
	return true, nil
}

// firecrackerSocketWaitTimeout bounds the time between process launch and API
// socket readiness.
const firecrackerSocketWaitTimeout = 5 * time.Second

// launchVM starts the Firecracker process for the Sandbox. In jailer mode
// the jailer (--netns <slot netns> --chroot-base-dir) launches firecracker
// inside the per-clone network namespace and its chroot.
func (d *Driver) launchVM(ctx context.Context, plan launchConfig, slot *fastletnetwork.Slot) (Process, error) {
	d.mu.RLock()
	config := d.config
	launcher := d.launcher
	d.mu.RUnlock()
	if config.JailerPath != "" && slot != nil {
		plan.JailerPath = config.JailerPath
		plan.ChrootBase = filepath.Join(config.StateRoot, jailerChrootBaseDir)
		plan.NetNSPath = slot.NetNSPath
		// The jailer chroot fixes the working directory to the jail root;
		// the relative rootfs.img path baked in the vmstate resolves there.
		plan.WorkingDir = ""
	}
	return launch(ctx, launcher, plan)
}

// validateManifestGuestNetwork checks the baked guest network of a template
// against the slot data plane it is restored into. The guest address,
// gateway, and netmask are frozen in the snapshot while the runtime CIDR
// comes from the environment: on a mismatch the guest keeps a half-working
// data plane (its frozen gateway no longer terminates DNS, the baked
// address escapes the IPAM reservation and can shadow another slot), so the
// restore must fail loudly instead.
func validateManifestGuestNetwork(slot *fastletnetwork.Slot, network manifestGuestNetwork) error {
	prefix, err := netip.ParsePrefix(slot.PrivateCIDR)
	if err != nil {
		return fmt.Errorf("%w: parse slot private CIDR %q: %v", ErrInvalidConfig, slot.PrivateCIDR, err)
	}
	address, err := netip.ParseAddr(network.IP)
	if err != nil || !address.Is4() {
		return fmt.Errorf("%w: invalid baked guest IP %q", ErrInvalidConfig, network.IP)
	}
	if !prefix.Contains(address) {
		return fmt.Errorf("%w: baked guest IP %s is outside the private CIDR %s", ErrInvalidConfig, network.IP, slot.PrivateCIDR)
	}
	// The guest-VM IPAM reserves exactly gateway + 2; any other baked
	// address can be handed to another slot and shadowed by its netns.
	conventional, err := fastletnetwork.BakedGuestIP(slot)
	if err != nil {
		return fmt.Errorf("%w: derive reserved guest address: %v", ErrInvalidConfig, err)
	}
	if network.IP != conventional {
		return fmt.Errorf("%w: baked guest IP %s does not match the reserved address %s (gateway + 2); rebuild the template or align FAST_SANDBOX_NETWORK_CIDR", ErrInvalidConfig, network.IP, conventional)
	}
	if network.Gateway != "" && network.Gateway != slot.Gateway {
		return fmt.Errorf("%w: baked guest gateway %s does not match the runtime gateway %s (guest DNS and proxy-ARP termination break)", ErrInvalidConfig, network.Gateway, slot.Gateway)
	}
	if network.Netmask != "" {
		bits, ok := netmaskBits(network.Netmask)
		if !ok {
			return fmt.Errorf("%w: invalid baked guest netmask %q", ErrInvalidConfig, network.Netmask)
		}
		if bits != prefix.Bits() {
			return fmt.Errorf("%w: baked guest netmask %s (/%d) does not match the private CIDR %s (/%d)", ErrInvalidConfig, network.Netmask, bits, slot.PrivateCIDR, prefix.Bits())
		}
	}
	return nil
}

// netmaskBits converts a dotted IPv4 netmask to its prefix length; it
// reports false for malformed or non-contiguous masks.
func netmaskBits(mask string) (int, bool) {
	address := net.ParseIP(mask)
	if address == nil || address.To4() == nil {
		return 0, false
	}
	ones, bits := net.IPMask(address.To4()).Size()
	if ones == 0 || bits != 32 {
		return 0, false
	}
	return ones, true
}

// resolveBakedGuestIP returns the baked guest address of the image and its
// recorded MTU (0 when unknown): the manifest guestNetwork is authoritative
// and validated against the slot data plane; the BakedGuestIP convention
// (gateway + 2, the builder/E2E prep baked address) is the fallback for
// hand-seeded caches without a manifest guest network (such caches carry no
// MTU and are inherently derived from the runtime CIDR).
func resolveBakedGuestIP(stateRoot, image string, slot *fastletnetwork.Slot) (string, int, error) {
	if guestNetwork, ok, err := readCachedManifestGuestNetwork(stateRoot, image); err != nil {
		return "", 0, fmt.Errorf("%w: read cached manifest guest network: %v", ErrImageNotReady, err)
	} else if ok {
		if err := validateManifestGuestNetwork(slot, guestNetwork); err != nil {
			return "", 0, err
		}
		return guestNetwork.IP, guestNetwork.MTU, nil
	}
	guestIP, err := fastletnetwork.BakedGuestIP(slot)
	if err != nil {
		return "", 0, fmt.Errorf("%w: derive baked guest IP: %v", ErrInvalidConfig, err)
	}
	return guestIP, 0, nil
}

// prepareInstance assembles the per-instance runtime assets: the writable
// instance rootfs and, in jailer mode, the jail root with the restore
// snapshot links. It returns the instance rootfs path, the jail root (empty
// in direct mode), and the host path of the firecracker API socket.
func (d *Driver) prepareInstance(stateRoot, sandboxID, image, stateDir, vmstatePath, memoryPath string) (instanceRootfs, jailRoot, apiAddress string, err error) {
	if d.config.JailerPath != "" {
		id := truncatedSandboxID(sandboxID)
		jailRoot = jailerRoot(filepath.Join(d.config.StateRoot, jailerChrootBaseDir), filepath.Base(d.config.BinaryPath), id)
		instanceRootfs = filepath.Join(jailRoot, rootfsImageName)
		apiAddress = filepath.Join(jailRoot, "api.sock")
		cached, resolveErr := resolveRootfsImage(stateRoot, image)
		if resolveErr != nil {
			return "", "", "", resolveErr
		}
		if prepareErr := prepareJailRoot(jailRoot, cached, vmstatePath, memoryPath); prepareErr != nil {
			return "", "", "", prepareErr
		}
		// Bind the snapshot spill root into the jail BEFORE the VMM starts:
		// the jailer clones its mount namespace at process start, so a dump
		// window bind would be invisible to the running VMM. A failed bind
		// only disables spilling for this sandbox (the dump falls back to
		// the staging directory).
		if spillRoot := d.snapshotSpillRoot(); spillRoot != "" {
			spillMount := filepath.Join(jailRoot, jailerSpillDirName)
			// The bind target must exist before MS_BIND (the jail root only
			// carries snapshots/ from prepareJailRoot).
			if err := os.MkdirAll(spillRoot, 0o750); err != nil {
				klog.InfoS("prepare snapshot spill root failed; spilling disabled", "spillRoot", spillRoot, "err", err)
			} else if err := os.MkdirAll(spillMount, 0o750); err != nil {
				klog.InfoS("create the jail spill mount point failed; spilling disabled", "err", err)
			} else if err := bindMount(spillRoot, spillMount); err != nil {
				klog.InfoS("bind the snapshot spill root into the jail root failed; spilling disabled", "err", err)
			}
		}
		return instanceRootfs, jailRoot, apiAddress, nil
	}
	instanceRootfs, err = prepareInstanceRootfs(stateRoot, image, stateDir)
	if err != nil {
		return "", "", "", err
	}
	return instanceRootfs, "", filepath.Join(stateDir, "api.sock"), nil
}

// removeJailRoot removes the jail directory of a Sandbox. The VMM must be
// stopped (kill) before the chroot and its snapshot links are deleted.
func (d *Driver) removeJailRoot(sandboxID string) {
	if d.config.JailerPath == "" {
		return
	}
	root := jailerRoot(filepath.Join(d.config.StateRoot, jailerChrootBaseDir), filepath.Base(d.config.BinaryPath), truncatedSandboxID(sandboxID))
	// Release the spill bind before the recursive removal: RemoveAll would
	// otherwise descend into the SHARED spill root and delete other
	// sandboxes' dumps.
	_ = unmountPath(filepath.Join(root, jailerSpillDirName))
	if err := os.RemoveAll(filepath.Dir(root)); err != nil {
		klog.V(2).InfoS("remove firecracker jail root failed", "sandboxId", sandboxID, "err", err)
	}
}

// waitForAPISocket polls until the Firecracker API Unix socket exists. The
// 20 ms poll keeps the launch latency near the VMM's own startup time
// (firecracker creates the socket within tens of milliseconds).
func waitForAPISocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s not ready within %s", socketPath, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// readProcessLog returns the tail of the firecracker process log, or an empty
// string when no log is available.
func readProcessLog(stateDir string) string {
	payload, err := os.ReadFile(filepath.Join(stateDir, processLogName))
	if err != nil {
		return ""
	}
	tail := payload
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	return "\nfirecracker process log:\n" + string(tail)
}

// resumeVM resumes a microVM restored from a snapshot and waits until it is
// Running. v1.16 does not allow InstanceStart after snapshot/load ("the
// requested operation is not supported after starting the microVM"); the
// restore leaves the VM Paused and PATCH /vm {"state":"Resumed"} resumes it.
func resumeVM(ctx context.Context, client *Client, timeoutSeconds int32) (int, error) {
	if err := client.Resume(ctx); err != nil {
		return 0, fmt.Errorf("resume Firecracker instance: %w", err)
	}
	return waitVMRunning(ctx, client, timeoutSeconds)
}

// waitVMRunning polls the VM state until it reports Running.
func waitVMRunning(ctx context.Context, client *Client, timeoutSeconds int32) (int, error) {
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	polls := 0
	for {
		state, err := client.VMState(ctx)
		if err != nil {
			return polls, fmt.Errorf("query Firecracker VM state: %w", err)
		}
		polls++
		if state == "Running" {
			return polls, nil
		}
		if time.Now().After(deadline) {
			return polls, fmt.Errorf("%w: Firecracker VM did not reach Running within %ds (state %q)", ErrRuntimeNotInitialized, timeoutSeconds, state)
		}
		select {
		case <-ctx.Done():
			return polls, ctx.Err()
		case <-time.After(bootPollInterval):
		}
	}
}

// resolveMachineConfig maps the Sandbox resource profile to Firecracker
// machine configuration, falling back to runtime defaults.
func resolveMachineConfig(spec fastletapi.SandboxSpec, config runtimecatalog.FirecrackerConfig) (MachineConfigRequest, error) {
	request := MachineConfigRequest{VCPUs: int(config.DefaultVCPUs), MemSizeMiB: defaultMemoryMiB(config.DefaultMemory)}
	if spec.CPU != "" {
		quantity, err := resource.ParseQuantity(spec.CPU)
		if err != nil {
			return MachineConfigRequest{}, fmt.Errorf("%w: invalid CPU %q", ErrInvalidConfig, spec.CPU)
		}
		millis := quantity.MilliValue()
		request.VCPUs = int(math.Ceil(float64(millis) / 1000.0))
		if request.VCPUs < 1 {
			return MachineConfigRequest{}, fmt.Errorf("%w: CPU %q yields no vCPU", ErrInvalidConfig, spec.CPU)
		}
	}
	if spec.Memory != "" {
		quantity, err := resource.ParseQuantity(spec.Memory)
		if err != nil {
			return MachineConfigRequest{}, fmt.Errorf("%w: invalid memory %q", ErrInvalidConfig, spec.Memory)
		}
		request.MemSizeMiB = int(math.Ceil(float64(quantity.Value()) / (1024.0 * 1024.0)))
		if request.MemSizeMiB < 1 {
			return MachineConfigRequest{}, fmt.Errorf("%w: memory %q yields no MiB", ErrInvalidConfig, spec.Memory)
		}
	}
	return request, nil
}

func defaultMemoryMiB(memory string) int {
	quantity, err := resource.ParseQuantity(memory)
	if err != nil {
		return 512
	}
	mib := int(math.Ceil(float64(quantity.Value()) / (1024.0 * 1024.0)))
	if mib < 1 {
		return 512
	}
	return mib
}

func (d *Driver) networkOwner(config *fastletapi.RuntimeSandboxConfig) fastletnetwork.Owner {
	identity := config.Identity
	generation := identity.InstanceGeneration
	if generation <= 0 {
		generation = 1
	}
	attempt := identity.AssignmentAttempt
	if attempt <= 0 {
		attempt = 1
	}
	return fastletnetwork.Owner{
		SandboxUID: identity.SandboxUID, SandboxName: identity.Name, SandboxNamespace: identity.Namespace,
		InstanceGeneration: generation, RuntimeInstanceID: identity.RuntimeInstanceID,
		AssignmentAttempt: attempt, ResidualProcess: runtimecatalog.ResidualProcessFirecracker,
	}
}

func existingMetadata(state *SandboxState) *SandboxMetadata {
	metadata := &SandboxMetadata{Config: state.Config, Allocation: state.Allocation}
	metadata.ContainerID = state.Config.Identity.SandboxUID
	metadata.PID = state.PID
	metadata.Phase = string(state.Phase)
	metadata.CreatedAt = state.CreatedAt
	metadata.UserProcessStartSource = fastletapi.UserProcessStartRuntimeDirect
	metadata.InfraServices = append(metadata.InfraServices, state.InfraServices...)
	metadata.InfraDiagnostics = append(metadata.InfraDiagnostics, state.InfraDiagnostics...)
	return metadata
}

func (d *Driver) rememberProcess(sandboxID string, process Process) {
	d.mu.Lock()
	d.processes[sandboxID] = process
	d.mu.Unlock()
}

// killAndForget stops the tracked process of a Sandbox; when no process
// handle exists (Fastlet restart), it falls back to the persisted PID.
func (d *Driver) killAndForget(sandboxID string, pid int) {
	d.mu.Lock()
	process, exists := d.processes[sandboxID]
	delete(d.processes, sandboxID)
	d.mu.Unlock()
	if exists {
		if process.Kill() == nil {
			// Wait for the VMM to exit before the network slot is released;
			// deleting the slot netns races a still-dying firecracker and
			// leaks the namespace (and its private address) on the bridge.
			done := make(chan struct{})
			go func() { _ = process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		return
	}
	if d.killProcess != nil {
		_ = d.killProcess(pid)
	}
}

// cleanupStale removes an obsolete VM, releases its network binding, and
// synchronously restores network capacity for the replacement Ensure call.
func (d *Driver) cleanupStale(ctx context.Context, directory string, state *SandboxState) error {
	d.mu.RLock()
	manager := d.networkManager
	d.mu.RUnlock()
	sandboxUID := state.Config.Identity.SandboxUID
	d.killAndForget(sandboxUID, state.PID)
	if manager != nil && sandboxUID != "" {
		if _, exists := manager.Lookup(sandboxUID); exists {
			if err := manager.Release(ctx, d.networkOwner(&state.Config)); err != nil {
				return fmt.Errorf("release stale Firecracker network slot: %w", err)
			}
			if err := manager.Replenish(ctx); err != nil {
				return fmt.Errorf("replenish Firecracker network slot: %w", err)
			}
		}
	}
	d.removeJailRoot(sandboxUID)
	return removeSandboxDir(directory)
}

func killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// pidAlive probes process existence with signal 0.
func pidAlive(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(0))
}

var _ runtimecontract.Driver = (*Driver)(nil)
