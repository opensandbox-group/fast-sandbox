package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	"fast-sandbox/pkg/util/idgen"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type SandboxManagerConfig struct {
	Capacity           int
	RuntimeProfileHash string
	ResourceProfile    *apiv1alpha1.SandboxResourceProfile
	FastletPodUID      string
	ReservationTTL     time.Duration
	Clock              Clock
	TokenGenerator     func() (string, error)
	RecoverOnStart     bool
	CacheEpoch         string
	WarmImages         []string
	RoutePublisher     RoutePublisher
}

type SandboxManager struct {
	mu                  sync.RWMutex
	runtime             Runtime
	capacity            int
	runtimeProfileHash  string
	resourceProfile     *apiv1alpha1.SandboxResourceProfile
	resourceProfileHash string
	fastletPodUID       string
	reservationTTL      time.Duration
	clock               Clock
	tokenGenerator      func() (string, error)
	recovering          bool
	runtimeReady        bool
	routeReady          bool
	draining            bool
	drainReason         string
	reservations        map[string]*reservation
	requestReservations map[string]string
	cacheTracker        *fastletcache.Tracker
	cacheProtection     *fastletcache.ProtectionIndex
	warmImages          []string
	routePublisher      RoutePublisher
	heartbeatSequence   atomic.Uint64
	// sandboxes  sandboxID -> metadata
	sandboxes map[string]*SandboxMetadata
}

func NewSandboxManager(runtime Runtime) *SandboxManager {
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
	if config.ReservationTTL <= 0 {
		config.ReservationTTL = 15 * time.Second
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.TokenGenerator == nil {
		config.TokenGenerator = generateReservationToken
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
		effective, err := (apiv1alpha1.SandboxPoolSpec{SandboxResources: *config.ResourceProfile}).EffectiveSandboxResources()
		if err != nil {
			return nil, err
		}
		profile = &effective
		resourceHash = effective.Hash()
	}
	var cacheSource fastletcache.ImageSource
	if source, ok := runtime.(RuntimeArtifactCache); ok {
		cacheSource = source
	}
	protection := fastletcache.NewProtectionIndex(config.Clock.Now)
	for _, image := range config.WarmImages {
		protection.Protect(image, fastletcache.ProtectWarm)
	}
	return &SandboxManager{
		runtime: runtime, capacity: config.Capacity,
		runtimeProfileHash: config.RuntimeProfileHash,
		resourceProfile:    profile, resourceProfileHash: resourceHash,
		fastletPodUID:  config.FastletPodUID,
		reservationTTL: config.ReservationTTL, clock: config.Clock, tokenGenerator: config.TokenGenerator,
		recovering: config.RecoverOnStart, runtimeReady: !config.RecoverOnStart,
		routeReady:   config.RoutePublisher == nil || !config.RecoverOnStart,
		reservations: make(map[string]*reservation), requestReservations: make(map[string]string),
		cacheTracker:    fastletcache.NewTracker(cacheSource, config.CacheEpoch, fastletcache.DefaultMaxInventory),
		cacheProtection: protection, warmImages: append([]string(nil), config.WarmImages...),
		routePublisher: config.RoutePublisher,
		sandboxes:      make(map[string]*SandboxMetadata),
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
				mu.Lock()
				result = errors.Join(result, fmt.Errorf("warm image %s: %w", image, err))
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	return result
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
	if capStr := os.Getenv("FASTLET_CAPACITY"); capStr == "" {
		capStr = os.Getenv("AGENT_CAPACITY")
		if capStr != "" {
			if v, err := strconv.Atoi(capStr); err == nil && v > 0 {
				capVal = v
			}
		}
	} else {
		if v, err := strconv.Atoi(capStr); err == nil && v > 0 {
			capVal = v
		}
	}
	return capVal
}

func (m *SandboxManager) CreateSandbox(ctx context.Context, spec *api.SandboxSpec) (*api.CreateSandboxResponse, error) {
	identity := api.SandboxIdentity{
		RequestID: spec.RequestID, SandboxUID: spec.SandboxID,
		InstanceGeneration: spec.InstanceGeneration, AssignmentAttempt: spec.AssignmentAttempt,
		RouteGeneration: spec.RouteGeneration,
		FastletPodUID:   spec.FastletPodUID,
	}
	if identity.InstanceGeneration <= 0 {
		identity.InstanceGeneration = 1
	}
	if identity.AssignmentAttempt <= 0 {
		identity.AssignmentAttempt = 1
	}
	if identity.RouteGeneration <= 0 {
		identity.RouteGeneration = 1
	}
	if identity.FastletPodUID == "" {
		identity.FastletPodUID = m.fastletPodUID
	}
	response, err := m.EnsureSandboxV2(ctx, &api.EnsureSandboxRequest{Identity: identity, Sandbox: *spec})
	if err != nil {
		message := err.Error()
		if response != nil && response.Error != nil {
			message = response.Error.Message
			if response.Error.Cause != nil {
				err = response.Error.Cause
			}
		}
		return &api.CreateSandboxResponse{Success: false, Message: message, SandboxID: spec.SandboxID}, err
	}
	createdAt := m.clock.Now().Unix()
	if response.Sandbox != nil && response.Sandbox.CreatedAt > 0 {
		createdAt = response.Sandbox.CreatedAt
	}
	return &api.CreateSandboxResponse{Success: true, SandboxID: spec.SandboxID, CreatedAt: createdAt}, nil
}

func (m *SandboxManager) validateProfiles(spec *api.SandboxSpec) error {
	if m.runtimeProfileHash != "" && spec.RuntimeProfileHash != m.runtimeProfileHash {
		return fmt.Errorf("%w: runtime profile hash %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.RuntimeProfileHash, m.runtimeProfileHash)
	}
	if m.resourceProfile == nil {
		return nil
	}
	if spec.ResourceProfileHash != m.resourceProfileHash {
		return fmt.Errorf("%w: resource profile hash %q does not match Fastlet profile %q", ErrSandboxProfileMismatch, spec.ResourceProfileHash, m.resourceProfileHash)
	}
	cpu, err := resource.ParseQuantity(spec.CPU)
	if err != nil || cpu.Cmp(m.resourceProfile.CPU) != 0 {
		return fmt.Errorf("%w: cpu %q does not match %s", ErrSandboxProfileMismatch, spec.CPU, m.resourceProfile.CPU.String())
	}
	memory, err := resource.ParseQuantity(spec.Memory)
	if err != nil || memory.Cmp(m.resourceProfile.Memory) != 0 {
		return fmt.Errorf("%w: memory %q does not match %s", ErrSandboxProfileMismatch, spec.Memory, m.resourceProfile.Memory.String())
	}
	if spec.PIDs != m.resourceProfile.PIDs {
		return fmt.Errorf("%w: pids %d does not match %d", ErrSandboxProfileMismatch, spec.PIDs, m.resourceProfile.PIDs)
	}
	return nil
}

func (m *SandboxManager) DeleteSandbox(sandboxID string) (*api.DeleteSandboxResponse, error) {
	klog.InfoS("[DEBUG-FASTLET] DeleteSandbox ENTER", "sandboxID", sandboxID)
	m.mu.Lock()
	sandbox, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.Unlock()
		klog.InfoS("[DEBUG-FASTLET] DeleteSandbox: sandbox not found, idempotent success", "sandboxID", sandboxID)
		return &api.DeleteSandboxResponse{
			Success: true,
		}, nil
	}
	if sandbox.Phase == "terminating" {
		m.mu.Unlock()
		klog.InfoS("[DEBUG-FASTLET] DeleteSandbox: already terminating, idempotent", "sandboxID", sandboxID)
		return &api.DeleteSandboxResponse{
			Success: true,
		}, nil
	}
	if sandbox.Phase == "creating" {
		sandbox.Phase = "terminating"
		m.mu.Unlock()
		klog.InfoS("DeleteSandbox: creation cancellation recorded", "sandboxID", sandboxID)
		return &api.DeleteSandboxResponse{Success: true}, nil
	}
	sandbox.Phase = "terminating"
	m.mu.Unlock()
	klog.InfoS("[DEBUG-FASTLET] DeleteSandbox: marked terminating, starting asyncDelete", "sandboxID", sandboxID)
	go m.asyncDelete(sandboxID, sandbox)
	return &api.DeleteSandboxResponse{
		Success: true,
	}, nil
}

func (m *SandboxManager) asyncDelete(sandboxID string, expected *SandboxMetadata) {
	klog.InfoS("[DEBUG-FASTLET] asyncDelete ENTER", "sandboxID", sandboxID)
	const gracefulTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), gracefulTimeout+5*time.Second)
	defer cancel()
	if err := m.removeRoute(ctx, expected); err != nil {
		m.mu.Lock()
		if m.sandboxes[sandboxID] == expected {
			expected.Phase = "delete-failed"
		}
		m.mu.Unlock()
		klog.ErrorS(err, "Fastlet Proxy route removal failed; runtime retained", "sandboxID", sandboxID)
		return
	}
	klog.InfoS("[DEBUG-FASTLET] asyncDelete: calling runtime.DeleteSandbox", "sandboxID", sandboxID)
	err := m.runtime.DeleteSandbox(ctx, sandboxID)
	klog.InfoS("[DEBUG-FASTLET] asyncDelete: runtime.DeleteSandbox completed",
		"sandboxID", sandboxID,
		"err", err)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.sandboxes[sandboxID] == expected {
			expected.Phase = "delete-failed"
		}
		klog.ErrorS(err, "Runtime deletion failed; retaining admission capacity for retry", "sandboxID", sandboxID)
		return
	}
	// A delayed delete from an old generation must never erase a newer
	// manager entry for the same logical Sandbox.
	if m.sandboxes[sandboxID] == expected {
		delete(m.sandboxes, sandboxID)
		m.cacheProtection.Unprotect(expected.Image, fastletcache.ProtectActive)
		m.cacheProtection.ProtectHotUntil(expected.Image, m.clock.Now().Add(time.Hour))
	}
	klog.InfoS("[DEBUG-FASTLET] asyncDelete: DONE, sandbox removed from sandboxes",
		"sandboxID", sandboxID)
}

func (m *SandboxManager) GetLogs(ctx context.Context, sandboxID string, follow bool, w io.Writer) error {
	reader, ok := m.runtime.(RuntimeLogReader)
	if !ok {
		return ErrUnsupportedRuntime
	}
	return reader.GetSandboxLogs(ctx, sandboxID, follow, w)
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
			AssignmentAttempt:  meta.AssignmentAttempt,
			Phase:              meta.Phase,
			Message:            runtimeStatus,
			CreatedAt:          meta.CreatedAt,
		})
	}

	return result
}

func (m *SandboxManager) RuntimeDiagnostics(ctx context.Context) api.RuntimeDiagnostics {
	report := m.runtime.ProbeCapabilities(ctx)
	return api.RuntimeDiagnostics{
		RuntimeProfileHash: m.runtimeProfileHash,
		State:              string(report.State),
		Reason:             report.Reason,
		Message:            report.Message,
	}
}

func (m *SandboxManager) FastletPodUID() string {
	return m.fastletPodUID
}

func (m *SandboxManager) Close() error {
	return m.runtime.Close()
}
