package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	"fast-sandbox/pkg/util/idgen"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type SandboxManagerConfig struct {
	Capacity           int
	RuntimeName        apiv1alpha2.RuntimeName
	RuntimeProfileHash string
	ResourceProfile    *apiv1alpha2.SandboxResourceProfile
	FastletPodUID      string
	Clock              Clock
	RecoverOnStart     bool
	CacheEpoch         string
	WarmImages         []string
	RoutePublisher     RoutePublisher
	InfraRevision      string
	InfraManager       *fastletinfra.Manager
	RegistryProvider   registryconfig.Provider
	ActionManager      *fastletaction.Manager
}

type SandboxManager struct {
	mu                  sync.RWMutex
	runtime             RuntimeDriver
	runtimeName         string
	capacity            int
	runtimeProfileHash  string
	resourceProfile     *apiv1alpha2.SandboxResourceProfile
	resourceProfileHash string
	infraRevision       string
	infraManager        *fastletinfra.Manager
	actionManager       *fastletaction.Manager
	infraReady          bool
	infraMessage        string
	fastletPodUID       string
	clock               Clock
	recovering          bool
	runtimeReady        bool
	routeReady          bool
	draining            bool
	drainReason         string
	tombstones          map[string]fastletapi.SandboxIdentity
	diagnostics         map[string][]fastletapi.SandboxDiagnosticEvent
	diagnosticOrder     []string
	cacheTracker        *fastletcache.Tracker
	cacheProtection     *fastletcache.ProtectionIndex
	warmImages          []string
	warmImageStates     map[string]fastletapi.WarmImageState
	registryProvider    registryconfig.Provider
	routePublisher      RoutePublisher
	dataPlaneWorkers    map[string]dataPlaneWorker
	// imageBootWorkers drives cold Sandboxes from the image-pending phase to
	// a committed runtime: once the async artifact delivery reports
	// Delivered, the worker boots the runtime (EnsureSandbox) and commits it
	// exactly like the synchronous create path.
	imageBootWorkers map[string]imageBootWorker
	// runtimeMessages carries the per-Sandbox runtime observation message
	// (e.g. image delivery progress and terminal create failures) projected
	// onto SandboxStatus.Runtime.Message.
	runtimeMessages   map[string]string
	readinessChanged  chan struct{}
	heartbeatSequence atomic.Uint64
	// sandboxes  sandboxID -> metadata
	sandboxes map[string]*SandboxMetadata
	// snapshots tracks in-memory snapshot tasks keyed by the SandboxSnapshot
	// UID (snapshot.go). Tasks are ephemeral and never survive a restart.
	snapshots map[string]*snapshotTask
}

func NewSandboxManager(runtime RuntimeDriver) *SandboxManager {
	manager, _ := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: capacityFromEnvironment()})
	return manager
}

func NewSandboxManagerWithConfig(runtime RuntimeDriver, config SandboxManagerConfig) (*SandboxManager, error) {
	if runtime == nil {
		return nil, ErrRuntimeNotInitialized
	}
	if config.Capacity <= 0 {
		return nil, fmt.Errorf("%w: capacity must be greater than zero", ErrInvalidConfig)
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.CacheEpoch == "" {
		var err error
		config.CacheEpoch, err = idgen.GenerateRequestID()
		if err != nil {
			return nil, fmt.Errorf("generate cache epoch: %w", err)
		}
	}
	var profile *apiv1alpha2.SandboxResourceProfile
	resourceHash := ""
	if config.ResourceProfile != nil {
		if err := apiv1alpha2.ValidateSandboxResourceProfile(*config.ResourceProfile); err != nil {
			return nil, err
		}
		copy := *config.ResourceProfile
		profile = &copy
		resourceHash = copy.Hash()
	}
	var cacheSource fastletcache.ImageSource
	if source, ok := runtime.(RuntimeArtifactCache); ok {
		cacheSource = source
	}
	protection := fastletcache.NewProtectionIndex(config.Clock.Now)
	warmImageStates := make(map[string]fastletapi.WarmImageState, len(config.WarmImages))
	for _, image := range config.WarmImages {
		protection.Protect(image, fastletcache.ProtectWarm)
		warmImageStates[image] = fastletapi.WarmImageState{Image: image, State: "Pulling"}
	}
	if config.InfraManager != nil {
		if config.InfraRevision != "" && config.InfraManager.Revision() != config.InfraRevision {
			return nil, fmt.Errorf("Infra revision %s does not match manager revision %s", config.InfraRevision, config.InfraManager.Revision())
		}
	}
	manager := &SandboxManager{
		runtime: runtime, runtimeName: string(config.RuntimeName), capacity: config.Capacity,
		runtimeProfileHash: config.RuntimeProfileHash,
		resourceProfile:    profile, resourceProfileHash: resourceHash,
		infraRevision: config.InfraRevision,
		infraManager:  config.InfraManager, infraReady: config.InfraManager == nil,
		actionManager: config.ActionManager,
		fastletPodUID: config.FastletPodUID,
		clock:         config.Clock,
		recovering:    config.RecoverOnStart, runtimeReady: !config.RecoverOnStart,
		routeReady:      config.RoutePublisher == nil || !config.RecoverOnStart,
		tombstones:      make(map[string]fastletapi.SandboxIdentity),
		diagnostics:     make(map[string][]fastletapi.SandboxDiagnosticEvent),
		cacheTracker:    fastletcache.NewTracker(cacheSource, config.CacheEpoch, fastletcache.DefaultMaxInventory),
		cacheProtection: protection, warmImages: append([]string(nil), config.WarmImages...),
		warmImageStates:  warmImageStates,
		registryProvider: config.RegistryProvider,
		readinessChanged: make(chan struct{}),
		routePublisher:   config.RoutePublisher,
		dataPlaneWorkers: make(map[string]dataPlaneWorker),
		imageBootWorkers: make(map[string]imageBootWorker),
		runtimeMessages:  make(map[string]string),
		sandboxes:        make(map[string]*SandboxMetadata),
		snapshots:        make(map[string]*snapshotTask),
	}
	if manager.actionManager != nil {
		manager.actionManager.SetChangeNotifier(manager.actionStateChanged)
	}
	return manager, nil
}

func (m *SandboxManager) RegistryRevision() string {
	if m.registryProvider == nil {
		return ""
	}
	return m.registryProvider.Revision()
}

func (m *SandboxManager) WarmCache(ctx context.Context) error {
	if len(m.warmImages) == 0 {
		return nil
	}
	cache, ok := m.runtime.(RuntimeArtifactCache)
	if !ok {
		return ErrUnsupportedRuntime
	}
	semaphore := make(chan struct{}, 2)
	var group sync.WaitGroup
	var mu sync.Mutex
	var result error
	for _, image := range m.warmImages {
		image := image
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				mu.Lock()
				result = errors.Join(result, ctx.Err())
				mu.Unlock()
				return
			}
			if err := cache.PullImage(ctx, image); err != nil {
				recordWarmImagePull(err)
				m.setWarmImageState(image, "Failed", err.Error())
				mu.Lock()
				result = errors.Join(result, fmt.Errorf("warm image %s: %w", image, err))
				mu.Unlock()
			} else {
				recordWarmImagePull(nil)
				m.setWarmImageState(image, "Cached", "")
			}
		}()
	}
	group.Wait()
	return result
}

func (m *SandboxManager) setWarmImageState(image, state, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(message) > 512 {
		message = message[:512]
	}
	m.warmImageStates[image] = fastletapi.WarmImageState{Image: image, State: state, Message: message}
}

func (m *SandboxManager) WarmImageStates() []fastletapi.WarmImageState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]fastletapi.WarmImageState, 0, len(m.warmImages))
	for _, image := range m.warmImages {
		state, found := m.warmImageStates[image]
		if !found {
			state = fastletapi.WarmImageState{Image: image, State: "Pulling"}
		}
		result = append(result, state)
	}
	return result
}

// PrepareInfra resolves and verifies the selected profile independently from
// ordinary warmImages. Kubernetes Pod readiness may become true before this
// completes; Registry hard-filtering and Fastlet admission use InfraReady.
func (m *SandboxManager) PrepareInfra(ctx context.Context) error {
	if m.infraManager == nil {
		m.mu.Lock()
		m.infraReady = true
		m.infraMessage = ""
		m.mu.Unlock()
		return nil
	}
	if err := m.infraManager.Prepare(ctx); err != nil {
		m.mu.Lock()
		m.infraReady = false
		m.infraMessage = err.Error()
		m.mu.Unlock()
		return err
	}
	for _, reference := range m.infraManager.ArtifactReferences() {
		m.cacheProtection.Protect(reference, fastletcache.ProtectInfra)
	}
	if err := m.ReconcilePendingInfra(ctx); err != nil {
		m.mu.Lock()
		m.infraReady = false
		m.infraMessage = err.Error()
		m.mu.Unlock()
		return err
	}
	if err := m.ReconcileProxyRoutes(ctx); err != nil {
		m.mu.Lock()
		m.infraReady = false
		m.infraMessage = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.infraReady = true
	m.infraMessage = ""
	m.mu.Unlock()
	return nil
}

func (m *SandboxManager) InfraStatus() (string, bool, []string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifacts := []string(nil)
	if m.infraManager != nil && m.infraReady {
		artifacts = m.infraManager.ArtifactReferences()
	}
	return m.infraRevision, m.infraReady, artifacts, m.infraMessage
}

func (m *SandboxManager) PlanCacheEviction(candidates []string) []string {
	return m.cacheProtection.PlanEviction(candidates)
}

func (m *SandboxManager) CacheSnapshot(ctx context.Context, cursor fastletapi.CacheCursor) (fastletapi.CacheSnapshot, error) {
	return m.cacheTracker.Snapshot(ctx, cursor)
}

func (m *SandboxManager) NextHeartbeatSequence() uint64 {
	return m.heartbeatSequence.Add(1)
}

func (m *SandboxManager) ResourceProfileHash() string {
	return m.resourceProfileHash
}

func (m *SandboxManager) RuntimeProfileHash() string {
	return m.runtimeProfileHash
}

func capacityFromEnvironment() int {
	capVal := 5
	if capStr := os.Getenv("FASTLET_CAPACITY"); capStr != "" {
		if v, err := strconv.Atoi(capStr); err == nil && v > 0 {
			capVal = v
		}
	}
	return capVal
}

func (m *SandboxManager) validateProfiles(spec *fastletapi.SandboxSpec) error {
	if m.runtimeProfileHash != "" && spec.RuntimeProfileHash != m.runtimeProfileHash {
		return fmt.Errorf("%w: runtime profile hash %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.RuntimeProfileHash, m.runtimeProfileHash)
	}
	if m.resourceProfile == nil {
		return m.validateInfraRevision(spec)
	}
	if spec.ResourceProfileHash != m.resourceProfileHash {
		return fmt.Errorf("%w: resource profile hash %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.ResourceProfileHash, m.resourceProfileHash)
	}
	if spec.CPU != "" {
		cpu, err := resource.ParseQuantity(spec.CPU)
		if err != nil || cpu.Cmp(m.resourceProfile.CPU) != 0 {
			return fmt.Errorf("%w: cpu %q does not match %s", ErrSandboxProfileMismatch, spec.CPU, m.resourceProfile.CPU.String())
		}
	}
	if spec.Memory != "" {
		memory, err := resource.ParseQuantity(spec.Memory)
		if err != nil || memory.Cmp(m.resourceProfile.Memory) != 0 {
			return fmt.Errorf("%w: memory %q does not match %s", ErrSandboxProfileMismatch, spec.Memory, m.resourceProfile.Memory.String())
		}
	}
	if spec.PIDs != 0 && spec.PIDs != m.resourceProfile.PIDs {
		return fmt.Errorf("%w: pids %d does not match %d", ErrSandboxProfileMismatch, spec.PIDs, m.resourceProfile.PIDs)
	}
	// The Fastlet profile is authoritative. The control plane sends only the
	// profile identity; runtime-enforced values are injected atomically here.
	spec.CPU = m.resourceProfile.CPU.String()
	spec.Memory = m.resourceProfile.Memory.String()
	spec.PIDs = m.resourceProfile.PIDs
	return m.validateInfraRevision(spec)
}

func (m *SandboxManager) validateInfraRevision(spec *fastletapi.SandboxSpec) error {
	if m.infraRevision != "" && spec.InfraRevision != m.infraRevision {
		return fmt.Errorf("%w: Infra revision %q does not match Fastlet revision %q", ErrSandboxProfileMismatch, spec.InfraRevision, m.infraRevision)
	}
	return nil
}

func (m *SandboxManager) beginDelete(sandboxID string) {
	m.mu.Lock()
	sandbox, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.Unlock()
		return
	}
	if sandbox.Phase == "terminating" {
		m.mu.Unlock()
		return
	}
	if sandbox.Phase == "creating" {
		sandbox.Phase = "terminating"
		m.recordDiagnosticLocked(sandboxID, "info", "fastlet", "terminating", "creation cancellation recorded")
		m.mu.Unlock()
		klog.InfoS("DeleteSandbox: creation cancellation recorded", "sandboxID", sandboxID)
		return
	}
	m.cancelDataPlaneReconcileLocked(sandbox)
	sandbox.Phase = "terminating"
	m.recordDiagnosticLocked(sandboxID, "info", "fastlet", "terminating", "runtime deletion started")
	m.mu.Unlock()
	klog.InfoS("Sandbox deletion started", "sandboxID", sandboxID)
	go m.asyncDelete(sandboxID, sandbox)
}

func (m *SandboxManager) asyncDelete(sandboxID string, expected *SandboxMetadata) {
	const gracefulTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), gracefulTimeout+5*time.Second)
	defer cancel()
	if err := m.removeRoute(ctx, expected); err != nil {
		m.mu.Lock()
		if m.sandboxes[sandboxID] == expected {
			expected.Phase = "delete-failed"
			m.recordDiagnosticLocked(sandboxID, "error", "route", "delete-failed", err.Error())
		}
		m.mu.Unlock()
		klog.ErrorS(err, "Fastlet Proxy route removal failed; runtime retained", "sandboxID", sandboxID)
		return
	}
	err := m.runtime.DeleteSandbox(ctx, sandboxID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.sandboxes[sandboxID] == expected {
			expected.Phase = "delete-failed"
			m.recordDiagnosticLocked(sandboxID, "error", "runtime", "delete-failed", err.Error())
		}
		klog.ErrorS(err, "Runtime deletion failed; retaining admission capacity for retry", "sandboxID", sandboxID)
		return
	}
	// A delayed delete from an old generation must never erase a newer
	// manager entry for the same logical Sandbox.
	if m.sandboxes[sandboxID] == expected {
		identity := expected.Config.Identity
		m.recordTombstoneLocked(fastletapi.SandboxIdentity{
			SandboxUID: sandboxID, InstanceGeneration: identity.InstanceGeneration,
			RuntimeInstanceID: identity.RuntimeInstanceID, AssignmentAttempt: identity.AssignmentAttempt,
			FastletPodUID: identity.FastletPodUID,
		})
		delete(m.sandboxes, sandboxID)
		delete(m.runtimeMessages, sandboxID)
		m.cacheProtection.Unprotect(expected.Config.Spec.Image, fastletcache.ProtectActive)
		m.cacheProtection.ProtectHotUntil(expected.Config.Spec.Image, m.clock.Now().Add(time.Hour))
		m.recordDiagnosticLocked(sandboxID, "info", "fastlet", "deleted", "proxy route and runtime resources were deleted")
		klog.InfoS("Sandbox deletion completed", "sandboxID", sandboxID)
	}
}

func (m *SandboxManager) ListImages(ctx context.Context) ([]string, error) {
	cache, ok := m.runtime.(RuntimeArtifactCache)
	if !ok {
		return nil, ErrUnsupportedRuntime
	}
	return cache.ListImages(ctx)
}

func (m *SandboxManager) GetCapacity() int {
	return m.capacity
}

func (m *SandboxManager) GetSandboxStatuses(ctx context.Context) []fastletapi.SandboxStatus {
	m.mu.RLock()
	snapshots := make(map[string]SandboxMetadata, len(m.sandboxes))
	messages := make(map[string]string, len(m.sandboxes))
	for sandboxID, metadata := range m.sandboxes {
		snapshots[sandboxID] = *metadata
		if message := m.runtimeMessages[sandboxID]; message != "" && metadata.Phase != "running" {
			messages[sandboxID] = message
		}
	}
	proxyReady := m.routePublisher == nil || m.routeReady
	m.mu.RUnlock()

	result := make([]fastletapi.SandboxStatus, 0, len(snapshots))
	for sandboxID, meta := range snapshots {
		identity := meta.Config.Identity
		dataPlaneReady := proxyReady && routeReadyForPhase(meta.Phase)
		runtimeObservation, dataPlaneObservation := observationsForPhase(meta.Phase, dataPlaneReady)
		if message := messages[sandboxID]; message != "" {
			runtimeObservation.Message = message
		}
		if inspected, err := m.runtime.InspectSandbox(ctx, sandboxID); err == nil {
			runtimeObservation.Message = inspected.Phase
		}
		result = append(result, fastletapi.SandboxStatus{
			SandboxID:          sandboxID,
			InstanceGeneration: identity.InstanceGeneration,
			RuntimeInstanceID:  identity.RuntimeInstanceID,
			AssignmentAttempt:  identity.AssignmentAttempt,
			RouteGeneration:    identity.RouteGeneration,
			AcceptedGeneration: meta.AcceptedGeneration,
			AppliedGeneration:  meta.AppliedGeneration,
			Runtime:            runtimeObservation,
			DataPlane:          dataPlaneObservation,
			InfraComponents:    apiInfraDiagnostics(meta.InfraDiagnostics, meta.InfraServices, identity.RouteGeneration, dataPlaneReady),
			ActionBindings:     append([]fastletapi.ActionBindingStatus(nil), meta.ActionBindingStatuses...),
			CreatedAt:          meta.CreatedAt,
		})
	}

	return result
}

func (m *SandboxManager) RuntimeDiagnostics(ctx context.Context) fastletapi.RuntimeDiagnostics {
	report := m.runtime.ProbeCapabilities(ctx)
	infraRevision, infraReady, _, infraMessage := m.InfraStatus()
	infraState := "Preparing"
	if infraReady {
		infraState = "Ready"
	}
	return fastletapi.RuntimeDiagnostics{
		RuntimeProfileHash: m.runtimeProfileHash,
		InfraRevision:      infraRevision, InfraState: infraState, InfraMessage: infraMessage,
		State:   string(report.State),
		Reason:  report.Reason,
		Message: report.Message,
	}
}

func (m *SandboxManager) FastletPodUID() string {
	return m.fastletPodUID
}

func (m *SandboxManager) Close() error {
	return m.runtime.Close()
}
