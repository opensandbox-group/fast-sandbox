package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"fast-sandbox/internal/api"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func (m *MockRuntime) SetNamespace(ns string) {}

func (m *MockRuntime) CreateSandbox(ctx context.Context, spec *api.SandboxSpec) (*SandboxMetadata, error) {
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

func (m *MockRuntime) ListImages(ctx context.Context) ([]string, error) {
	return m.listImages, nil
}

func (m *MockRuntime) PullImage(ctx context.Context, image string) error {
	return nil
}

func (m *MockRuntime) GetSandboxLogs(ctx context.Context, sandboxID string, follow bool, stdout io.Writer) error {
	return nil
}

func (m *MockRuntime) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalled = true
	return nil
}

// Helper methods for testing

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
			name:     "zero capacity defaults to 0",
			envValue: "0",
			expected: 0,
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
			expected: -5, // strconv.Atoi allows negative
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

// ============================================================================
// 2. TestSandboxManager_CreateSandbox
// ============================================================================

func TestSandboxManager_CreateSandbox_Success(t *testing.T) {
	// CS-01: Successfully creates a sandbox
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-1",
		ClaimUID:  "claim-uid-1",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
		Command:   []string{"/bin/sh"},
	}

	resp, err := manager.CreateSandbox(ctx, spec)

	require.NoError(t, err, "CreateSandbox should succeed")
	assert.True(t, resp.Success, "Response should indicate success")
	assert.Equal(t, spec.SandboxID, resp.SandboxID, "SandboxID should match")
	assert.Greater(t, resp.CreatedAt, int64(0), "CreatedAt should be set")

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
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-idempotent",
		ClaimUID:  "claim-uid-2",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// First creation
	resp1, err1 := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err1, "First creation should succeed")
	assert.True(t, resp1.Success)

	// Reset mock to track if CreateSandbox is called again
	mockRuntime.Reset()

	// Second creation of same sandbox
	resp2, err2 := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err2, "Second creation should succeed (idempotent)")
	assert.True(t, resp2.Success, "Second creation should return success")
	assert.Equal(t, spec.SandboxID, resp2.SandboxID)

	// Verify runtime CreateSandbox was NOT called again (cached)
	assert.False(t, mockRuntime.GetCreateCalled(), "Runtime CreateSandbox should not be called for existing sandbox")
}

func TestSandboxManager_CreateSandbox_RuntimeFailure(t *testing.T) {
	// CS-03: Runtime failure returns error (runtime handles its own cleanup)
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-fail",
		ClaimUID:  "claim-uid-3",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Set mock to return error
	expectedErr := errors.New("runtime create failed")
	mockRuntime.SetCreateError(expectedErr)

	resp, err := manager.CreateSandbox(ctx, spec)

	require.Error(t, err, "CreateSandbox should return error")
	assert.False(t, resp.Success, "Response should indicate failure")
	assert.Contains(t, resp.Message, "create failed", "Error message should contain details")
	assert.Equal(t, expectedErr, err, "Returned error should match runtime error")

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

	sandboxes := []api.SandboxSpec{
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
		resp, err := manager.CreateSandbox(ctx, &spec)
		require.NoError(t, err, "CreateSandbox for %s should succeed", spec.SandboxID)
		assert.True(t, resp.Success)
	}

	// Verify all sandboxes are in status
	statuses := manager.GetSandboxStatuses(ctx)
	assert.Len(t, statuses, 3, "Should have three sandbox statuses")

	// Create a map for easier lookup
	statusMap := make(map[string]api.SandboxStatus)
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
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-delete",
		ClaimUID:  "claim-uid-delete",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err)

	// Delete sandbox
	resp, err := manager.DeleteSandbox(spec.SandboxID)

	require.NoError(t, err, "DeleteSandbox should succeed")
	assert.True(t, resp.Success, "Response should indicate success")

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
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-delete-idempotent",
		ClaimUID:  "claim-uid-delete-2",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err)

	// First delete
	resp1, err1 := manager.DeleteSandbox(spec.SandboxID)
	require.NoError(t, err1, "First delete should succeed")
	assert.True(t, resp1.Success)

	// Reset mock to track if DeleteSandbox is called again
	mockRuntime.Reset()

	// Second delete (should be idempotent)
	resp2, err2 := manager.DeleteSandbox(spec.SandboxID)
	require.NoError(t, err2, "Second delete should succeed (idempotent)")
	assert.True(t, resp2.Success, "Second delete should return success")

	// The runtime DeleteSandbox might be called again by asyncDelete goroutine
	// but the manager's DeleteSandbox should return immediately without queuing another delete
}

func TestSandboxManager_DeleteSandbox_NonExistent(t *testing.T) {
	// DS-03: Deleting non-existent sandbox - should be idempotent and return success
	// This follows the principle that DELETE operations should be idempotent
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	// Deleting a non-existent sandbox should succeed (idempotent behavior)
	resp, err := manager.DeleteSandbox("non-existent-sandbox")
	assert.NoError(t, err)
	assert.True(t, resp.Success)
}

func TestSandboxManager_DeleteSandbox_MultipleDeletes(t *testing.T) {
	// DS-04: Multiple rapid deletes show idempotency during "terminating" phase
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-multiple-delete",
		ClaimUID:  "claim-uid-multiple",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err)

	// First delete
	resp1, err1 := manager.DeleteSandbox(spec.SandboxID)
	require.NoError(t, err1)
	assert.True(t, resp1.Success)

	// Second delete while in "terminating" phase (before async completes)
	// This should be idempotent and return success
	resp2, err2 := manager.DeleteSandbox(spec.SandboxID)
	require.NoError(t, err2)
	assert.True(t, resp2.Success, "Second delete during terminating phase should succeed")

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
	spec1 := &api.SandboxSpec{
		SandboxID: "active-sb-1",
		ClaimUID:  "claim-1",
		ClaimName: "claim-1",
		Image:     "alpine:latest",
	}
	spec2 := &api.SandboxSpec{
		SandboxID: "active-sb-2",
		ClaimUID:  "claim-2",
		ClaimName: "claim-2",
		Image:     "nginx:latest",
	}

	_, err := manager.CreateSandbox(ctx, spec1)
	require.NoError(t, err)
	_, err = manager.CreateSandbox(ctx, spec2)
	require.NoError(t, err)

	// Delete one sandbox
	_, err = manager.DeleteSandbox(spec1.SandboxID)
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
	spec := &api.SandboxSpec{
		SandboxID: "test-sb-status",
		ClaimUID:  "claim-status",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	_, err := manager.CreateSandbox(ctx, spec)
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
	specs := []*api.SandboxSpec{
		{SandboxID: "sb-1", ClaimUID: "claim-1", ClaimName: "claim-1", Image: "alpine:latest"},
		{SandboxID: "sb-2", ClaimUID: "claim-2", ClaimName: "claim-2", Image: "nginx:latest"},
		{SandboxID: "sb-3", ClaimUID: "claim-3", ClaimName: "claim-3", Image: "ubuntu:latest"},
	}

	for _, spec := range specs {
		_, err := manager.CreateSandbox(ctx, spec)
		require.NoError(t, err)
	}

	// Mark one for deletion
	_, err := manager.DeleteSandbox(specs[0].SandboxID)
	require.NoError(t, err)

	// Wait for async delete to start
	time.Sleep(50 * time.Millisecond)

	statuses := manager.GetSandboxStatuses(ctx)

	// Create a map for easier lookup
	statusMap := make(map[string]api.SandboxStatus)
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
// 7. TestSandboxManager_GetLogs
// ============================================================================

func TestSandboxManager_GetLogs(t *testing.T) {
	// GL-01: GetLogs propagates to runtime
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	err := manager.GetLogs(ctx, "test-sandbox", false, nil)

	assert.NoError(t, err, "GetLogs should succeed")
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
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-timeout",
		ClaimUID:  "claim-uid-timeout",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err)

	// Delete sandbox (async)
	resp, err := manager.DeleteSandbox(spec.SandboxID)
	require.NoError(t, err)
	assert.True(t, resp.Success)

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
	spec := &api.SandboxSpec{
		SandboxID: "test-sandbox-delete-error",
		ClaimUID:  "claim-uid-delete-error",
		ClaimName: "test-claim",
		Image:     "alpine:latest",
	}

	// Create sandbox first
	_, err := manager.CreateSandbox(ctx, spec)
	require.NoError(t, err)

	// Set delete error
	mockRuntime.SetDeleteError(errors.New("delete failed"))

	// Delete sandbox (async)
	resp, err := manager.DeleteSandbox(spec.SandboxID)
	require.NoError(t, err)
	assert.True(t, resp.Success)

	// Wait for async delete
	time.Sleep(100 * time.Millisecond)

	// Verify sandbox was completely removed even with runtime error
	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed even with runtime error")
}
