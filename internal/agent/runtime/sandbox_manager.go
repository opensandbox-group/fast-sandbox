package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"fast-sandbox/internal/api"

	"k8s.io/klog/v2"
)

type SandboxManager struct {
	mu       sync.RWMutex
	runtime  Runtime
	capacity int
	// sandboxes  sandboxID -> metadata
	sandboxes map[string]*SandboxMetadata
}

func NewSandboxManager(runtime Runtime) *SandboxManager {
	capVal := 5
	if capStr := os.Getenv("AGENT_CAPACITY"); capStr != "" {
		if v, err := strconv.Atoi(capStr); err == nil {
			capVal = v
		}
	}
	return &SandboxManager{
		runtime:   runtime,
		capacity:  capVal,
		sandboxes: make(map[string]*SandboxMetadata),
	}
}

func (m *SandboxManager) CreateSandbox(ctx context.Context, spec *api.SandboxSpec) (*api.CreateSandboxResponse, error) {
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
	metadata, err := m.runtime.CreateSandbox(ctx, spec)
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

func (m *SandboxManager) DeleteSandbox(sandboxID string) (*api.DeleteSandboxResponse, error) {
	klog.InfoS("[DEBUG-AGENT] DeleteSandbox ENTER", "sandboxID", sandboxID)
	m.mu.Lock()
	sandbox, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.Unlock()
		klog.InfoS("[DEBUG-AGENT] DeleteSandbox: sandbox not found, idempotent success", "sandboxID", sandboxID)
		return &api.DeleteSandboxResponse{
			Success: true,
		}, nil
	}
	if sandbox.Phase == "terminating" {
		m.mu.Unlock()
		klog.InfoS("[DEBUG-AGENT] DeleteSandbox: already terminating, idempotent", "sandboxID", sandboxID)
		return &api.DeleteSandboxResponse{
			Success: true,
		}, nil
	}
	sandbox.Phase = "terminating"
	m.mu.Unlock()
	klog.InfoS("[DEBUG-AGENT] DeleteSandbox: marked terminating, starting asyncDelete", "sandboxID", sandboxID)
	go m.asyncDelete(sandboxID)
	return &api.DeleteSandboxResponse{
		Success: true,
	}, nil
}

func (m *SandboxManager) asyncDelete(sandboxID string) {
	klog.InfoS("[DEBUG-AGENT] asyncDelete ENTER", "sandboxID", sandboxID)
	const gracefulTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), gracefulTimeout+5*time.Second)
	defer cancel()
	klog.InfoS("[DEBUG-AGENT] asyncDelete: calling runtime.DeleteSandbox", "sandboxID", sandboxID)
	err := m.runtime.DeleteSandbox(ctx, sandboxID)
	klog.InfoS("[DEBUG-AGENT] asyncDelete: runtime.DeleteSandbox completed",
		"sandboxID", sandboxID,
		"err", err,
		"nextStep", "removing from sandboxes")
	m.mu.Lock()
	defer m.mu.Unlock()
	// Remove from sandboxes map after deletion completes
	delete(m.sandboxes, sandboxID)
	klog.InfoS("[DEBUG-AGENT] asyncDelete: DONE, sandbox removed from sandboxes",
		"sandboxID", sandboxID)
}

func (m *SandboxManager) GetLogs(ctx context.Context, sandboxID string, follow bool, w io.Writer) error {
	return m.runtime.GetSandboxLogs(ctx, sandboxID, follow, w)
}
func (m *SandboxManager) ListImages(ctx context.Context) ([]string, error) {
	return m.runtime.ListImages(ctx)
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
		runtimeStatus, _ := m.runtime.GetSandboxStatus(ctx, sandboxID)
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
