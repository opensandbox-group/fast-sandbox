package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type SandboxManagerConfig struct {
	Capacity           int
	RuntimeProfileHash string
	ResourceProfile    *apiv1alpha1.SandboxResourceProfile
}

type SandboxManager struct {
	mu                  sync.RWMutex
	runtime             Runtime
	capacity            int
	runtimeProfileHash  string
	resourceProfile     *apiv1alpha1.SandboxResourceProfile
	resourceProfileHash string
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
	return &SandboxManager{
		runtime: runtime, capacity: config.Capacity,
		runtimeProfileHash: config.RuntimeProfileHash,
		resourceProfile:    profile, resourceProfileHash: resourceHash,
		sandboxes: make(map[string]*SandboxMetadata),
	}, nil
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
	if err := m.validateProfiles(spec); err != nil {
		return &api.CreateSandboxResponse{Success: false, Message: err.Error()}, err
	}
	m.mu.Lock()
	if _, exists := m.sandboxes[spec.SandboxID]; exists {
		m.mu.Unlock()
		klog.InfoS("Sandbox already exists in cache, returning success (idempotent)", "sandbox", spec.SandboxID)
		return &api.CreateSandboxResponse{
			Success:   true,
			SandboxID: spec.SandboxID,
		}, nil
	}
	m.sandboxes[spec.SandboxID] = &SandboxMetadata{
		SandboxSpec: *spec,
		Phase:       "creating",
	}
	m.mu.Unlock()

	createdAt := time.Now().Unix()
	metadata, err := m.runtime.EnsureSandbox(ctx, spec)
	if err != nil {
		// Clean up the "creating" placeholder on failure
		m.mu.Lock()
		delete(m.sandboxes, spec.SandboxID)
		m.mu.Unlock()
		klog.ErrorS(err, "Failed to create sandbox", "sandbox", spec.SandboxID)
		return &api.CreateSandboxResponse{
			Success: false,
			Message: fmt.Sprintf("create failed: %v", err),
		}, err
	}
	metadata.Phase = "running"
	m.mu.Lock()
	m.sandboxes[spec.SandboxID] = metadata
	m.mu.Unlock()
	klog.InfoS("Created sandbox", "sandbox", spec.SandboxID, "image", spec.Image)
	return &api.CreateSandboxResponse{
		Success:   true,
		SandboxID: spec.SandboxID,
		CreatedAt: createdAt,
	}, nil
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
	sandbox.Phase = "terminating"
	m.mu.Unlock()
	klog.InfoS("[DEBUG-FASTLET] DeleteSandbox: marked terminating, starting asyncDelete", "sandboxID", sandboxID)
	go m.asyncDelete(sandboxID)
	return &api.DeleteSandboxResponse{
		Success: true,
	}, nil
}

func (m *SandboxManager) asyncDelete(sandboxID string) {
	klog.InfoS("[DEBUG-FASTLET] asyncDelete ENTER", "sandboxID", sandboxID)
	const gracefulTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), gracefulTimeout+5*time.Second)
	defer cancel()
	klog.InfoS("[DEBUG-FASTLET] asyncDelete: calling runtime.DeleteSandbox", "sandboxID", sandboxID)
	err := m.runtime.DeleteSandbox(ctx, sandboxID)
	klog.InfoS("[DEBUG-FASTLET] asyncDelete: runtime.DeleteSandbox completed",
		"sandboxID", sandboxID,
		"err", err,
		"nextStep", "removing from sandboxes")
	m.mu.Lock()
	defer m.mu.Unlock()
	// Remove from sandboxes map after deletion completes
	delete(m.sandboxes, sandboxID)
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
	defer m.mu.RUnlock()

	result := make([]api.SandboxStatus, 0)

	// Add active sandboxes
	for sandboxID, meta := range m.sandboxes {
		runtimeStatus := "unknown"
		if inspected, err := m.runtime.InspectSandbox(ctx, sandboxID); err == nil {
			runtimeStatus = inspected.Phase
		}
		result = append(result, api.SandboxStatus{
			SandboxID: sandboxID,
			ClaimUID:  meta.ClaimUID,
			Phase:     meta.Phase,
			Message:   runtimeStatus,
			CreatedAt: meta.CreatedAt,
		})
	}

	return result
}

func (m *SandboxManager) Close() error {
	return m.runtime.Close()
}
