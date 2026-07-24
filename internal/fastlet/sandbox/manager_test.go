package sandbox

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ============================================================================
// Mock Runtime
// ============================================================================

// MockRuntime is a mock implementation of the Runtime interface for testing.
type MockRuntime struct {
	mu             sync.Mutex
	sandboxes      map[string]*SandboxMetadata
	containers     map[string]string
	createError    error
	deleteError    error
	listImages     []string
	createCalled   bool
	deleteCalled   bool
	closeCalled    bool
	getStatusCalls map[string]int
}

// NewMockRuntime creates a new mock runtime for testing.
func NewMockRuntime() *MockRuntime {
	return &MockRuntime{
		sandboxes:      make(map[string]*SandboxMetadata),
		containers:     make(map[string]string),
		listImages:     []string{"alpine:latest", "nginx:latest"},
		getStatusCalls: make(map[string]int),
	}
}

func (m *MockRuntime) Initialize(ctx context.Context, socketPath string) error {
	return nil
}

func (m *MockRuntime) ProbeCapabilities(context.Context) CapabilityReport {
	return CapabilityReport{State: runtimecatalog.CapabilityReady}
}

func (m *MockRuntime) SetNamespace(ns string) {}

func (m *MockRuntime) EnsureSandbox(ctx context.Context, spec *fastletapi.SandboxSpec) (*SandboxMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalled = true

	if m.createError != nil {
		return nil, m.createError
	}

	metadata := &SandboxMetadata{
		SandboxSpec: *spec,
		ContainerID: "container-" + spec.SandboxID,
		PID:         1234,
		Phase:       "created",
		CreatedAt:   time.Now().Unix(),
	}
	m.sandboxes[spec.SandboxID] = metadata
	m.containers[spec.SandboxID] = metadata.ContainerID

	return metadata, nil
}

func (m *MockRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalled = true

	delete(m.sandboxes, sandboxID)
	delete(m.containers, sandboxID)
	return m.deleteError
}

func (m *MockRuntime) GetSandboxStatus(ctx context.Context, sandboxID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.getStatusCalls[sandboxID]++

	if sb, exists := m.sandboxes[sandboxID]; exists {
		return sb.Phase, nil
	}
	return "unknown", nil
}

func (m *MockRuntime) InspectSandbox(ctx context.Context, sandboxID string) (*SandboxMetadata, error) {
	status, err := m.GetSandboxStatus(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if sandbox, ok := m.sandboxes[sandboxID]; ok {
		copy := *sandbox
		copy.Phase = status
		return &copy, nil
	}
	return &SandboxMetadata{SandboxSpec: fastletapi.SandboxSpec{SandboxID: sandboxID}, Phase: status}, nil
}

func (m *MockRuntime) ListManagedSandboxes(context.Context) ([]*SandboxMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*SandboxMetadata, 0, len(m.sandboxes))
	for _, sandbox := range m.sandboxes {
		copy := *sandbox
		result = append(result, &copy)
	}
	return result, nil
}

func (m *MockRuntime) ListImages(ctx context.Context) ([]string, error) {
	return m.listImages, nil
}

func (m *MockRuntime) PullImage(ctx context.Context, image string) error {
	return nil
}

func (m *MockRuntime) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalled = true
	return nil
}

// Helper methods for testing

func ensureSandboxForTest(ctx context.Context, manager *SandboxManager, spec *fastletapi.SandboxSpec) (*fastletapi.CreateSandboxResponse, error) {
	return manager.CreateSandbox(ctx, &fastletapi.CreateSandboxRequest{
		Identity: fastletapi.SandboxIdentity{
			RequestID: "test-" + spec.SandboxID, SandboxUID: spec.SandboxID,
			InstanceGeneration: 1, RuntimeInstanceID: "runtime-" + spec.SandboxID,
			AssignmentAttempt: 1, FastletPodUID: manager.fastletPodUID,
		},
		Sandbox: *spec,
	})
}

func deleteSandboxForTest(manager *SandboxManager, sandboxID string) (*fastletapi.DeleteSandboxV2Response, error) {
	return manager.DeleteSandboxV2(&fastletapi.DeleteSandboxV2Request{Identity: fastletapi.SandboxIdentity{
		SandboxUID: sandboxID, InstanceGeneration: 1, RuntimeInstanceID: "runtime-" + sandboxID,
		AssignmentAttempt: 1, FastletPodUID: manager.fastletPodUID,
	}})
}

func (m *MockRuntime) SetCreateError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createError = err
}

func (m *MockRuntime) SetDeleteError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteError = err
}

func (m *MockRuntime) SetListImages(images []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listImages = images
}

func (m *MockRuntime) HasSandbox(sandboxID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.sandboxes[sandboxID]
	return exists
}

func (m *MockRuntime) GetCreateCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalled
}

func (m *MockRuntime) GetDeleteCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteCalled
}

func (m *MockRuntime) GetCloseCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCalled
}

func (m *MockRuntime) GetStatusCallCount(sandboxID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getStatusCalls[sandboxID]
}

func (m *MockRuntime) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxes = make(map[string]*SandboxMetadata)
	m.containers = make(map[string]string)
	m.createError = nil
	m.deleteError = nil
	m.createCalled = false
	m.deleteCalled = false
	m.closeCalled = false
	m.getStatusCalls = make(map[string]int)
}

// ============================================================================
// 1. TestNewSandboxManager
// ============================================================================

func TestNewSandboxManager(t *testing.T) {
	// NSM-01: Constructor creates manager with default capacity (5)
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	require.NotNil(t, manager, "Manager should not be nil")
	assert.Equal(t, 5, manager.GetCapacity(), "Default capacity should be 5")
	assert.NotNil(t, manager.runtime, "Runtime should be set")
}

func TestNewSandboxManager_CustomCapacity(t *testing.T) {
	// NSM-02: Constructor reads capacity from FASTLET_CAPACITY env var
	// Save and restore original env value
	originalValue := os.Getenv("FASTLET_CAPACITY")
	defer func() {
		if originalValue != "" {
			os.Setenv("FASTLET_CAPACITY", originalValue)
		} else {
			os.Unsetenv("FASTLET_CAPACITY")
		}
	}()

	testCases := []struct {
		name     string
		envValue string
		expected int
	}{
		{
			name:     "capacity of 10",
			envValue: "10",
			expected: 10,
		},
		{
			name:     "capacity of 100",
			envValue: "100",
			expected: 100,
		},
		{
			name:     "capacity of 1",
			envValue: "1",
			expected: 1,
		},
		{
			name:     "zero capacity defaults to 5",
			envValue: "0",
			expected: 5,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("FASTLET_CAPACITY", tc.envValue)
			mockRuntime := NewMockRuntime()
			manager := NewSandboxManager(mockRuntime)

			assert.Equal(t, tc.expected, manager.GetCapacity(), "Capacity should match env var")
		})
	}
}

func TestNewSandboxManager_InvalidCapacity(t *testing.T) {
	// NSM-03: Constructor handles invalid FASTLET_CAPACITY values gracefully
	originalValue := os.Getenv("FASTLET_CAPACITY")
	defer func() {
		if originalValue != "" {
			os.Setenv("FASTLET_CAPACITY", originalValue)
		} else {
			os.Unsetenv("FASTLET_CAPACITY")
		}
	}()

	testCases := []struct {
		name     string
		envValue string
		expected int
	}{
		{
			name:     "non-numeric value defaults to 5",
			envValue: "invalid",
			expected: 5,
		},
		{
			name:     "negative value",
			envValue: "-5",
			expected: 5,
		},
		{
			name:     "empty string defaults to 5",
			envValue: "",
			expected: 5,
		},
		{
			name:     "value with spaces",
			envValue: " 10 ",
			expected: 5, // strconv.Atoi fails with spaces
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("FASTLET_CAPACITY", tc.envValue)
			mockRuntime := NewMockRuntime()
			manager := NewSandboxManager(mockRuntime)

			assert.Equal(t, tc.expected, manager.GetCapacity())
		})
	}
}

func TestSandboxManagerRejectsProfileOverrides(t *testing.T) {
	profile := apiv1alpha1.SandboxResourceProfile{
		CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
	}
	manager, err := NewSandboxManagerWithConfig(NewMockRuntime(), SandboxManagerConfig{
		Capacity: 5, RuntimeProfileHash: "runtime-hash", ResourceProfile: &profile,
	})
	require.NoError(t, err)
	valid := &fastletapi.SandboxSpec{
		SandboxID: "sandbox-a", CPU: "500m", Memory: "256Mi", PIDs: 128,
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: profile.Hash(),
	}
	require.NoError(t, manager.validateProfiles(valid))

	tests := map[string]func(*fastletapi.SandboxSpec){
		"runtime hash":    func(spec *fastletapi.SandboxSpec) { spec.RuntimeProfileHash = "other" },
		"resource hash":   func(spec *fastletapi.SandboxSpec) { spec.ResourceProfileHash = "other" },
		"cpu override":    func(spec *fastletapi.SandboxSpec) { spec.CPU = "1" },
		"memory override": func(spec *fastletapi.SandboxSpec) { spec.Memory = "1Gi" },
		"pids override":   func(spec *fastletapi.SandboxSpec) { spec.PIDs = 256 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := *valid
			mutate(&candidate)
			require.ErrorIs(t, manager.validateProfiles(&candidate), ErrSandboxProfileMismatch)
		})
	}
}

// ============================================================================
// 2. TestSandboxManager_CreateSandbox
// ============================================================================

func TestSandboxManager_CreateSandbox_Success(t *testing.T) {
	// CS-01: Successfully creates a sandbox
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-1",
		ClaimUID:  "claim-uid-1",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
		Command:   []string{"/bin/sh"},
	}

	resp, err := ensureSandboxForTest(ctx, manager, spec)

	require.NoError(t, err, "CreateSandbox should succeed")
	assert.True(t, resp.Accepted, "Response should indicate acceptance")
	require.NotNil(t, resp.Sandbox)
	assert.Equal(t, spec.SandboxID, resp.Sandbox.SandboxID, "SandboxID should match")
	assert.Greater(t, resp.Sandbox.CreatedAt, int64(0), "CreatedAt should be set")

	// Verify sandbox is in manager's cache
	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1, "Should have one sandbox status")
	assert.Equal(t, spec.SandboxID, statuses[0].SandboxID)
	assert.Equal(t, "running", statuses[0].Phase)
	assert.Equal(t, spec.ClaimUID, statuses[0].ClaimUID)
}

func TestSandboxManager_CreateSandbox_Idempotent(t *testing.T) {
	// CS-02: Creating an existing sandbox returns success (idempotent)
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-idempotent",
		ClaimUID:  "claim-uid-2",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// First creation
	resp1, err1 := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err1, "First creation should succeed")
	assert.True(t, resp1.Accepted)

	// Reset mock to track if CreateSandbox is called again
	mockRuntime.Reset()

	// Second creation of same sandbox
	resp2, err2 := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err2, "Second creation should succeed (idempotent)")
	assert.True(t, resp2.Accepted, "Second creation should be accepted")
	require.NotNil(t, resp2.Sandbox)
	assert.Equal(t, spec.SandboxID, resp2.Sandbox.SandboxID)

	// Verify runtime CreateSandbox was NOT called again (cached)
	assert.False(t, mockRuntime.GetCreateCalled(), "Runtime CreateSandbox should not be called for existing sandbox")
}

func TestSandboxManager_CreateSandbox_RuntimeFailure(t *testing.T) {
	// CS-03: Runtime failure returns error (runtime handles its own cleanup)
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-fail",
		ClaimUID:  "claim-uid-3",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Set mock to return error
	expectedErr := errors.New("runtime create failed")
	mockRuntime.SetCreateError(expectedErr)

	resp, err := ensureSandboxForTest(ctx, manager, spec)

	require.Error(t, err, "CreateSandbox should return error")
	assert.False(t, resp.Accepted, "Response should indicate failure")
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "create failed", "Error message should contain details")
	require.ErrorIs(t, err, expectedErr, "Structured Fastlet error should preserve the runtime cause")

	// Wait for any potential async cleanup
	time.Sleep(100 * time.Millisecond)

	// Verify sandbox was not added to the manager's cache
	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Failed sandbox should not be in cache")

	// Note: The runtime is responsible for its own cleanup on create failure,
	// the manager doesn't call asyncDelete to avoid race conditions
}

func TestSandboxManager_CreateSandbox_MultipleSandboxes(t *testing.T) {
	// CS-04: Creating multiple sandboxes works correctly
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()

	sandboxes := []fastletapi.SandboxSpec{
		{
			SandboxID: "sb-1",
			ClaimUID:  "claim-1",
			ClaimName: "claim-1",
			Image:     "alpine:latest",
		},
		{
			SandboxID: "sb-2",
			ClaimUID:  "claim-2",
			ClaimName: "claim-2",
			Image:     "nginx:latest",
		},
		{
			SandboxID: "sb-3",
			ClaimUID:  "claim-3",
			ClaimName: "claim-3",
			Image:     "ubuntu:latest",
		},
	}

	for _, spec := range sandboxes {
		resp, err := ensureSandboxForTest(ctx, manager, &spec)
		require.NoError(t, err, "CreateSandbox for %s should succeed", spec.SandboxID)
		assert.True(t, resp.Accepted)
	}

	// Verify all sandboxes are in status
	statuses := manager.GetSandboxStatuses(ctx)
	assert.Len(t, statuses, 3, "Should have three sandbox statuses")

	// Create a map for easier lookup
	statusMap := make(map[string]fastletapi.SandboxStatus)
	for _, status := range statuses {
		statusMap[status.SandboxID] = status
	}

	for _, spec := range sandboxes {
		status, exists := statusMap[spec.SandboxID]
		assert.True(t, exists, "Sandbox %s should exist in statuses", spec.SandboxID)
		assert.Equal(t, spec.ClaimUID, status.ClaimUID)
		assert.Equal(t, "running", status.Phase)
	}
}

// ============================================================================
// 3. TestSandboxManager_DeleteSandbox
// ============================================================================

func TestSandboxManager_DeleteSandbox_Success(t *testing.T) {
	// DS-01: Successfully deletes a sandbox
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-delete",
		ClaimUID:  "claim-uid-delete",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// Delete sandbox
	resp, err := deleteSandboxForTest(manager, spec.SandboxID)

	require.NoError(t, err, "DeleteSandbox should succeed")
	assert.True(t, resp.Accepted, "Response should indicate acceptance")

	// Sandbox should be in terminating phase
	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1, "Should have one status")
	assert.Equal(t, spec.SandboxID, statuses[0].SandboxID)
	assert.Equal(t, "terminating", statuses[0].Phase)

	// Wait for async deletion to complete
	time.Sleep(100 * time.Millisecond)

	// Sandbox should be completely removed (direct deletion)
	statuses = manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed after async delete")
}

func TestSandboxManager_DeleteSandbox_Idempotent(t *testing.T) {
	// DS-02: Deleting already-terminating sandbox returns success (idempotent)
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-delete-idempotent",
		ClaimUID:  "claim-uid-delete-2",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// First delete
	resp1, err1 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err1, "First delete should succeed")
	assert.True(t, resp1.Accepted)

	// Reset mock to track if DeleteSandbox is called again
	mockRuntime.Reset()

	// Second delete (should be idempotent)
	resp2, err2 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err2, "Second delete should succeed (idempotent)")
	assert.True(t, resp2.Accepted, "Second delete should be accepted")

	// The runtime DeleteSandbox might be called again by asyncDelete goroutine
	// but the manager's DeleteSandbox should return immediately without queuing another delete
}

func TestSandboxManager_DeleteSandbox_NonExistent(t *testing.T) {
	// DS-03: Deleting non-existent sandbox - should be idempotent and return success
	// This follows the principle that DELETE operations should be idempotent
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	// Deleting a non-existent sandbox should succeed (idempotent behavior)
	resp, err := deleteSandboxForTest(manager, "non-existent-sandbox")
	assert.NoError(t, err)
	assert.True(t, resp.Accepted)
}

func TestSandboxManager_DeleteSandbox_MultipleDeletes(t *testing.T) {
	// DS-04: Multiple rapid deletes show idempotency during "terminating" phase
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-multiple-delete",
		ClaimUID:  "claim-uid-multiple",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// First delete
	resp1, err1 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err1)
	assert.True(t, resp1.Accepted)

	// Second delete while in "terminating" phase (before async completes)
	// This should be idempotent and return success
	resp2, err2 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err2)
	assert.True(t, resp2.Accepted, "Second delete during terminating phase should be accepted")

	// Wait for async delete to complete
	time.Sleep(100 * time.Millisecond)

	// Verify sandbox was completely removed (direct deletion)
	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed after async delete")
}

// ============================================================================
// 4. TestSandboxManager_GetSandboxStatuses
// ============================================================================

func TestSandboxManager_GetSandboxStatuses(t *testing.T) {
	// GS-01: Returns statuses for active sandboxes only (deleted ones are completely removed)
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()

	// Create two active sandboxes
	spec1 := &fastletapi.SandboxSpec{
		SandboxID: "active-sb-1",
		ClaimUID:  "claim-1",
		ClaimName: "claim-1",
		Image:     "alpine:latest",
	}
	spec2 := &fastletapi.SandboxSpec{
		SandboxID: "active-sb-2",
		ClaimUID:  "claim-2",
		ClaimName: "claim-2",
		Image:     "nginx:latest",
	}

	_, err := ensureSandboxForTest(ctx, manager, spec1)
	require.NoError(t, err)
	_, err = ensureSandboxForTest(ctx, manager, spec2)
	require.NoError(t, err)

	// Delete one sandbox
	_, err = deleteSandboxForTest(manager, spec1.SandboxID)
	require.NoError(t, err)

	// Wait for async delete to complete
	time.Sleep(100 * time.Millisecond)

	// Get statuses
	statuses := manager.GetSandboxStatuses(ctx)

	// Should have only the active sandbox (deleted one is completely removed)
	require.Len(t, statuses, 1, "Should have one status (only active)")

	// Check active sandbox is still running
	activeStatus := statuses[0]
	assert.Equal(t, spec2.SandboxID, activeStatus.SandboxID)
	assert.Equal(t, "running", activeStatus.Phase, "Active sandbox should be running")
	assert.Equal(t, spec2.ClaimUID, activeStatus.ClaimUID)
}

func TestSandboxManager_GetSandboxStatuses_Empty(t *testing.T) {
	// GS-02: Returns empty list when no sandboxes exist
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	statuses := manager.GetSandboxStatuses(ctx)

	assert.NotNil(t, statuses, "Statuses should not be nil")
	assert.Empty(t, statuses, "Statuses should be empty")
}

func TestSandboxManager_GetSandboxStatuses_RuntimeStatus(t *testing.T) {
	// GS-03: Includes runtime status in Message field
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sb-status",
		ClaimUID:  "claim-status",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1)

	// Message should contain runtime status
	assert.NotEmpty(t, statuses[0].Message, "Message should contain runtime status")

	// Verify GetSandboxStatus was called on runtime
	callCount := mockRuntime.GetStatusCallCount(spec.SandboxID)
	assert.Greater(t, callCount, 0, "GetSandboxStatus should be called on runtime")
}

func TestSandboxManager_GetSandboxStatuses_MultiplePhases(t *testing.T) {
	// GS-04: Reports different phases for different sandbox states
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()

	// Create sandboxes
	specs := []*fastletapi.SandboxSpec{
		{SandboxID: "sb-1", ClaimUID: "claim-1", ClaimName: "claim-1", Image: "alpine:latest"},
		{SandboxID: "sb-2", ClaimUID: "claim-2", ClaimName: "claim-2", Image: "nginx:latest"},
		{SandboxID: "sb-3", ClaimUID: "claim-3", ClaimName: "claim-3", Image: "ubuntu:latest"},
	}

	for _, spec := range specs {
		_, err := ensureSandboxForTest(ctx, manager, spec)
		require.NoError(t, err)
	}

	// Mark one for deletion
	_, err := deleteSandboxForTest(manager, specs[0].SandboxID)
	require.NoError(t, err)

	// Wait for async delete to start
	time.Sleep(50 * time.Millisecond)

	statuses := manager.GetSandboxStatuses(ctx)

	// Create a map for easier lookup
	statusMap := make(map[string]fastletapi.SandboxStatus)
	for _, status := range statuses {
		statusMap[status.SandboxID] = status
	}

	// First sandbox might be terminating or gone (async may have completed)
	if firstStatus, exists := statusMap[specs[0].SandboxID]; exists {
		assert.Equal(t, "terminating", firstStatus.Phase, "First sandbox should be terminating if still present")
	}

	// Other sandboxes should be running
	for i := 1; i < 3; i++ {
		status := statusMap[specs[i].SandboxID]
		assert.Equal(t, "running", status.Phase, "Sandbox %s should be running", specs[i].SandboxID)
	}
}

// ============================================================================
// 5. TestSandboxManager_GetCapacity
// ============================================================================

func TestSandboxManager_GetCapacity(t *testing.T) {
	// GC-01: Returns configured capacity
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	capacity := manager.GetCapacity()
	assert.Equal(t, 5, capacity, "Default capacity should be 5")
}

func TestSandboxManager_GetCapacity_Custom(t *testing.T) {
	// GC-02: Returns custom capacity when FASTLET_CAPACITY is set
	originalValue := os.Getenv("FASTLET_CAPACITY")
	defer func() {
		if originalValue != "" {
			os.Setenv("FASTLET_CAPACITY", originalValue)
		} else {
			os.Unsetenv("FASTLET_CAPACITY")
		}
	}()

	os.Setenv("FASTLET_CAPACITY", "20")
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	capacity := manager.GetCapacity()
	assert.Equal(t, 20, capacity, "Capacity should match FASTLET_CAPACITY env var")
}

// ============================================================================
// 6. TestSandboxManager_Close
// ============================================================================

func TestSandboxManager_Close(t *testing.T) {
	// CL-01: Close propagates to runtime
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	err := manager.Close()

	assert.NoError(t, err, "Close should succeed")
	assert.True(t, mockRuntime.GetCloseCalled(), "Runtime Close should be called")
}

func TestSandboxManager_Close_MultipleCalls(t *testing.T) {
	// CL-02: Multiple close calls are handled gracefully
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	// First close
	err1 := manager.Close()
	assert.NoError(t, err1)

	// Second close
	err2 := manager.Close()
	assert.NoError(t, err2)

	// Both should succeed
	assert.True(t, mockRuntime.GetCloseCalled(), "Runtime Close should be called")
}

// ============================================================================
// 8. TestSandboxManager_ListImages
// ============================================================================

func TestSandboxManager_ListImages(t *testing.T) {
	// LI-01: ListImages propagates to runtime
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	images, err := manager.ListImages(ctx)

	assert.NoError(t, err, "ListImages should succeed")
	assert.Equal(t, []string{"alpine:latest", "nginx:latest"}, images, "Should return mock images")
}

func TestSandboxManager_ListImages_CustomList(t *testing.T) {
	// LI-02: ListImages returns custom images from runtime
	mockRuntime := NewMockRuntime()
	customImages := []string{"custom:latest", "another:v1.0"}
	mockRuntime.SetListImages(customImages)

	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	images, err := manager.ListImages(ctx)

	assert.NoError(t, err, "ListImages should succeed")
	assert.Equal(t, customImages, images, "Should return custom images")
}

// ============================================================================
// 9. TestSandboxManager_AsyncDeleteBehavior
// ============================================================================

func TestSandboxManager_AsyncDelete_Timeout(t *testing.T) {
	// AD-01: Async delete handles context timeout gracefully
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-timeout",
		ClaimUID:  "claim-uid-timeout",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// Delete sandbox (async)
	resp, err := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err)
	assert.True(t, resp.Accepted)

	// Wait for async delete to complete (should complete within timeout)
	time.Sleep(200 * time.Millisecond)

	// Verify sandbox was completely removed (direct deletion)
	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed after async delete")
}

func TestSandboxManager_AsyncDelete_RuntimeError(t *testing.T) {
	// AD-02: Async delete handles runtime errors gracefully
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &fastletapi.SandboxSpec{
		SandboxID: "test-sandbox-delete-error",
		ClaimUID:  "claim-uid-delete-error",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// Set delete error
	mockRuntime.SetDeleteError(errors.New("delete failed"))

	// Delete sandbox (async)
	resp, err := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err)
	assert.True(t, resp.Accepted)

	// Wait for async delete
	time.Sleep(100 * time.Millisecond)

	// Failed runtime deletion must retain capacity and identity so a later
	// retry cannot over-admit while an orphan may still exist.
	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1)
	assert.Equal(t, "delete-failed", statuses[0].Phase)
	admission, _, _ := manager.State()
	assert.Equal(t, 1, admission.Used)

	mockRuntime.SetDeleteError(nil)
	_, err = deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0
	}, time.Second, 10*time.Millisecond)
}
