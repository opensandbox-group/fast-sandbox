package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/pkg/util/idgen"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type SandboxManagerConfig struct {
	Capacity           int
	RuntimeName        apiv1alpha1.RuntimeName
	RuntimeProfileHash string
	ResourceProfile    *apiv1alpha1.SandboxResourceProfile
	FastletPodUID      string
	Clock              Clock
	RecoverOnStart     bool
	CacheEpoch         string
	WarmImages         []string
	RoutePublisher     RoutePublisher
	InfraProfile       string
	InfraProfileHash   string
	InfraManager       *fastletinfra.Manager
}

type SandboxManager struct {
	mu                  sync.RWMutex
	runtime             RuntimeDriver
	runtimeName         string
	capacity            int
	runtimeProfileHash  string
	resourceProfile     *apiv1alpha1.SandboxResourceProfile
	resourceProfileHash string
	infraProfile        string
	infraProfileHash    string
	infraManager        *fastletinfra.Manager
	infraReady          bool
	infraMessage        string
	fastletPodUID       string
	clock               Clock
	recovering          bool
	runtimeReady        bool
	routeReady          bool
	draining            bool
	drainReason         string
	tombstones          map[string]api.SandboxIdentity
	diagnostics         map[string][]api.SandboxDiagnosticEvent
	diagnosticOrder     []string
	cacheTracker        *fastletcache.Tracker
	cacheProtection     *fastletcache.ProtectionIndex
	warmImages          []string
	routePublisher      RoutePublisher
	dataPlaneWorkers    map[string]dataPlaneWorker
	heartbeatSequence   atomic.Uint64
	// sandboxes  sandboxID -> metadata
	sandboxes map[string]*SandboxMetadata
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
	var profile *apiv1alpha1.SandboxResourceProfile
	resourceHash := ""
	if config.ResourceProfile != nil {
		if err := apiv1alpha1.ValidateSandboxResourceProfile(*config.ResourceProfile); err != nil {
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
	for _, image := range config.WarmImages {
		protection.Protect(image, fastletcache.ProtectWarm)
	}
	if config.InfraManager != nil {
		if config.InfraProfile != "" && config.InfraManager.ProfileName() != config.InfraProfile {
			return nil, fmt.Errorf("InfraProfile %s does not match manager profile %s", config.InfraProfile, config.InfraManager.ProfileName())
		}
		if config.InfraProfileHash != "" && config.InfraManager.ProfileHash() != config.InfraProfileHash {
			return nil, fmt.Errorf("InfraProfile hash %s does not match manager hash %s", config.InfraProfileHash, config.InfraManager.ProfileHash())
		}
	}
	return &SandboxManager{
		runtime: runtime, runtimeName: string(config.RuntimeName), capacity: config.Capacity,
		runtimeProfileHash: config.RuntimeProfileHash,
		resourceProfile:    profile, resourceProfileHash: resourceHash,
		infraProfile: config.InfraProfile, infraProfileHash: config.InfraProfileHash,
		infraManager: config.InfraManager, infraReady: config.InfraManager == nil,
		fastletPodUID: config.FastletPodUID,
		clock:         config.Clock,
		recovering:    config.RecoverOnStart, runtimeReady: !config.RecoverOnStart,
		routeReady:      config.RoutePublisher == nil || !config.RecoverOnStart,
		tombstones:      make(map[string]api.SandboxIdentity),
		diagnostics:     make(map[string][]api.SandboxDiagnosticEvent),
		cacheTracker:    fastletcache.NewTracker(cacheSource, config.CacheEpoch, fastletcache.DefaultMaxInventory),
		cacheProtection: protection, warmImages: append([]string(nil), config.WarmImages...),
		routePublisher:   config.RoutePublisher,
		dataPlaneWorkers: make(map[string]dataPlaneWorker),
		sandboxes:        make(map[string]*SandboxMetadata),
	}, nil
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
				mu.Lock()
				result = errors.Join(result, fmt.Errorf("warm image %s: %w", image, err))
				mu.Unlock()
			} else {
				recordWarmImagePull(nil)
			}
		}()
	}
	group.Wait()
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

func (m *SandboxManager) InfraStatus() (string, string, bool, []string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifacts := []string(nil)
	if m.infraManager != nil && m.infraReady {
		artifacts = m.infraManager.ArtifactReferences()
	}
	return m.infraProfile, m.infraProfileHash, m.infraReady, artifacts, m.infraMessage
}

func (m *SandboxManager) PlanCacheEviction(candidates []string) []string {
	return m.cacheProtection.PlanEviction(candidates)
}

func (m *SandboxManager) CacheSnapshot(ctx context.Context, cursor api.CacheCursor) (api.CacheSnapshot, error) {
	return m.cacheTracker.Snapshot(ctx, cursor)
}

func (m *SandboxManager) NextHeartbeatSequence() uint64 {
	return m.heartbeatSequence.Add(1)
}

func (m *SandboxManager) ResourceProfileHash() string {
	return m.resourceProfileHash
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

func (m *SandboxManager) validateProfiles(spec *api.SandboxSpec) error {
	if m.runtimeProfileHash != "" && spec.RuntimeProfileHash != m.runtimeProfileHash {
		return fmt.Errorf("%w: runtime profile hash %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.RuntimeProfileHash, m.runtimeProfileHash)
	}
	if m.resourceProfile == nil {
		return m.validateInfraProfile(spec)
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
	return m.validateInfraProfile(spec)
}

func (m *SandboxManager) validateInfraProfile(spec *api.SandboxSpec) error {
	if m.infraProfile != "" && spec.InfraProfile != m.infraProfile {
		return fmt.Errorf("%w: InfraProfile %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.InfraProfile, m.infraProfile)
	}
	if m.infraProfileHash != "" && spec.InfraProfileHash != m.infraProfileHash {
		return fmt.Errorf("%w: InfraProfile hash %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.InfraProfileHash, m.infraProfileHash)
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
		m.recordTombstoneLocked(api.SandboxIdentity{
			SandboxUID: sandboxID, InstanceGeneration: expected.InstanceGeneration,
			RuntimeInstanceID: expected.RuntimeInstanceID, AssignmentAttempt: expected.AssignmentAttempt,
			FastletPodUID: expected.FastletPodUID,
		})
		delete(m.sandboxes, sandboxID)
		m.cacheProtection.Unprotect(expected.Image, fastletcache.ProtectActive)
		m.cacheProtection.ProtectHotUntil(expected.Image, m.clock.Now().Add(time.Hour))
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

func (m *SandboxManager) GetSandboxStatuses(ctx context.Context) []api.SandboxStatus {
	m.mu.RLock()
	snapshots := make(map[string]SandboxMetadata, len(m.sandboxes))
	for sandboxID, metadata := range m.sandboxes {
		snapshots[sandboxID] = *metadata
	}
	m.mu.RUnlock()

	result := make([]api.SandboxStatus, 0, len(snapshots))
	for sandboxID, meta := range snapshots {
		runtimeStatus := "unknown"
		if inspected, err := m.runtime.InspectSandbox(ctx, sandboxID); err == nil {
			runtimeStatus = inspected.Phase
		}
		result = append(result, api.SandboxStatus{
			SandboxID:          sandboxID,
			ClaimUID:           meta.ClaimUID,
			InstanceGeneration: meta.InstanceGeneration,
			RuntimeInstanceID:  meta.RuntimeInstanceID,
			AssignmentAttempt:  meta.AssignmentAttempt,
			Phase:              meta.Phase,
			Message:            runtimeStatus,
			InfraDiagnostics:   apiInfraDiagnostics(meta.InfraDiagnostics),
			CreatedAt:          meta.CreatedAt,
		})
	}

	return result
}

func (m *SandboxManager) RuntimeDiagnostics(ctx context.Context) api.RuntimeDiagnostics {
	report := m.runtime.ProbeCapabilities(ctx)
	infraProfile, infraHash, infraReady, _, infraMessage := m.InfraStatus()
	infraState := "Preparing"
	if infraReady {
		infraState = "Ready"
	}
	return api.RuntimeDiagnostics{
		RuntimeProfileHash: m.runtimeProfileHash,
		InfraProfile:       infraProfile, InfraProfileHash: infraHash, InfraState: infraState, InfraMessage: infraMessage,
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
