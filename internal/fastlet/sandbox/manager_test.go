package sandbox

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

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

func (m *MockRuntime) EnsureSandbox(ctx context.Context, input *fastletapi.EnsureSandboxInput) (*SandboxMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalled = true

	if m.createError != nil {
		return nil, m.createError
	}

	config := input.Sandbox
	sandboxUID := config.Identity.SandboxUID
	metadata := &SandboxMetadata{
		Config:      config,
		ContainerID: "container-" + sandboxUID,
		PID:         1234,
		Phase:       "created",
		CreatedAt:   time.Now().Unix(),
	}
	m.sandboxes[sandboxUID] = metadata
	m.containers[sandboxUID] = metadata.ContainerID

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
	return &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{SandboxUID: sandboxID}}, Phase: status}, nil
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

type sandboxCreateFixture struct {
	fastletapi.SandboxSpec
	SandboxID          string
	ClaimNamespace     string
	ClaimName          string
	FastletPodUID      string
	InstanceGeneration int64
	RuntimeInstanceID  string
	AssignmentAttempt  int64
	RouteGeneration    int64
}

func ensureSandboxForTest(ctx context.Context, manager *SandboxManager, spec *sandboxCreateFixture) (*fastletapi.CreateSandboxResponse, error) {
	namespace := spec.ClaimNamespace
	if namespace == "" {
		namespace = "default"
	}
	name := spec.ClaimName
	if name == "" {
		name = spec.SandboxID
	}
	return manager.CreateSandbox(ctx, &fastletapi.CreateSandboxRequest{
		RequestID:      "test-" + spec.SandboxID,
		SpecGeneration: 1,
		Identity: fastletapi.SandboxIdentity{
			SandboxUID: spec.SandboxID, Namespace: namespace, Name: name,
			InstanceGeneration: 1, RuntimeInstanceID: "runtime-" + spec.SandboxID,
			AssignmentAttempt: 1, FastletPodUID: manager.fastletPodUID,
		},
		Sandbox: spec.SandboxSpec,
	})
}

func runtimeSpecForTest(sandboxID, claimName, image string) *sandboxCreateFixture {
	return &sandboxCreateFixture{
		SandboxSpec: fastletapi.SandboxSpec{Image: image},
		SandboxID:   sandboxID,
		ClaimName:   claimName,
	}
}

func deleteSandboxForTest(manager *SandboxManager, sandboxID string) (*fastletapi.DeleteSandboxResponse, error) {
	return manager.DeleteSandbox(&fastletapi.DeleteSandboxRequest{Identity: fastletapi.SandboxIdentity{
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

func TestNewSandboxManager(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	require.NotNil(t, manager, "Manager should not be nil")
	assert.Equal(t, 5, manager.GetCapacity(), "Default capacity should be 5")
	assert.NotNil(t, manager.runtime, "Runtime should be set")
}

func TestNewSandboxManager_CustomCapacity(t *testing.T) {
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
	profile := apiv1alpha2.SandboxResourceProfile{
		CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
	}
	manager, err := NewSandboxManagerWithConfig(NewMockRuntime(), SandboxManagerConfig{
		Capacity: 5, RuntimeProfileHash: "runtime-hash", ResourceProfile: &profile,
	})
	require.NoError(t, err)
	valid := &sandboxCreateFixture{
		SandboxSpec: fastletapi.SandboxSpec{
			CPU: "500m", Memory: "256Mi", PIDs: 128,
			RuntimeProfileHash: "runtime-hash", ResourceProfileHash: profile.Hash(),
		},
		SandboxID: "sandbox-a",
	}
	require.NoError(t, manager.validateProfiles(&valid.SandboxSpec))

	tests := map[string]func(*sandboxCreateFixture){
		"runtime hash":    func(spec *sandboxCreateFixture) { spec.RuntimeProfileHash = "other" },
		"resource hash":   func(spec *sandboxCreateFixture) { spec.ResourceProfileHash = "other" },
		"cpu override":    func(spec *sandboxCreateFixture) { spec.CPU = "1" },
		"memory override": func(spec *sandboxCreateFixture) { spec.Memory = "1Gi" },
		"pids override":   func(spec *sandboxCreateFixture) { spec.PIDs = 256 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := *valid
			mutate(&candidate)
			require.ErrorIs(t, manager.validateProfiles(&candidate.SandboxSpec), ErrSandboxProfileMismatch)
		})
	}
}

func TestSandboxManager_CreateSandbox_Success(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-1", "test-claim", "alpine:latest")
	spec.Command = []string{"/bin/sh"}

	resp, err := ensureSandboxForTest(ctx, manager, spec)

	require.NoError(t, err, "CreateSandbox should succeed")
	assert.Equal(t, fastletapi.CreateDispositionCreated, resp.Disposition)
	require.NotNil(t, resp.Sandbox)
	assert.Equal(t, spec.SandboxID, resp.Sandbox.SandboxID, "SandboxID should match")
	assert.Greater(t, resp.Sandbox.CreatedAt, int64(0), "CreatedAt should be set")

	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1, "Should have one sandbox status")
	assert.Equal(t, spec.SandboxID, statuses[0].SandboxID)
	assert.Equal(t, fastletapi.RuntimeStateReady, statuses[0].Runtime.State)
	assert.Equal(t, fastletapi.DataPlaneStateReady, statuses[0].DataPlane.State)
}

func TestSandboxManager_CreateSandbox_Idempotent(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-idempotent", "test-claim", "alpine:latest")

	// First creation
	resp1, err1 := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err1, "First creation should succeed")
	assert.Equal(t, fastletapi.CreateDispositionCreated, resp1.Disposition)

	// Reset mock to track if CreateSandbox is called again
	mockRuntime.Reset()

	// Second creation of same sandbox
	resp2, err2 := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err2, "Second creation should succeed (idempotent)")
	assert.Equal(t, fastletapi.CreateDispositionExisting, resp2.Disposition)
	require.NotNil(t, resp2.Sandbox)
	assert.Equal(t, spec.SandboxID, resp2.Sandbox.SandboxID)

	assert.False(t, mockRuntime.GetCreateCalled(), "Runtime CreateSandbox should not be called for existing sandbox")
}

func TestSandboxManager_CreateSandbox_RuntimeFailure(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-fail", "test-claim", "alpine:latest")

	expectedErr := errors.New("runtime create failed")
	mockRuntime.SetCreateError(expectedErr)

	resp, err := ensureSandboxForTest(ctx, manager, spec)

	require.Error(t, err, "CreateSandbox should return error")
	assert.Equal(t, fastletapi.CreateDispositionRejectedBeforeSideEffects, resp.Disposition)
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "create failed", "Error message should contain details")
	require.ErrorIs(t, err, expectedErr, "Structured Fastlet error should preserve the runtime cause")

	// Wait for any potential async cleanup
	time.Sleep(100 * time.Millisecond)

	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Failed sandbox should not be in cache")

	// Note: The runtime is responsible for its own cleanup on create failure,
	// the manager doesn't call asyncDelete to avoid race conditions
}

func TestSandboxManager_CreateSandbox_MultipleSandboxes(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()

	sandboxes := []sandboxCreateFixture{
		*runtimeSpecForTest("sb-1", "claim-1", "alpine:latest"),
		*runtimeSpecForTest("sb-2", "claim-2", "nginx:latest"),
		*runtimeSpecForTest("sb-3", "claim-3", "ubuntu:latest"),
	}

	for _, spec := range sandboxes {
		resp, err := ensureSandboxForTest(ctx, manager, &spec)
		require.NoError(t, err, "CreateSandbox for %s should succeed", spec.SandboxID)
		assert.Equal(t, fastletapi.CreateDispositionCreated, resp.Disposition)
	}

	statuses := manager.GetSandboxStatuses(ctx)
	assert.Len(t, statuses, 3, "Should have three sandbox statuses")

	statusMap := make(map[string]fastletapi.SandboxStatus)
	for _, status := range statuses {
		statusMap[status.SandboxID] = status
	}

	for _, spec := range sandboxes {
		status, exists := statusMap[spec.SandboxID]
		assert.True(t, exists, "Sandbox %s should exist in statuses", spec.SandboxID)
		assert.Equal(t, fastletapi.RuntimeStateReady, status.Runtime.State)
		assert.Equal(t, fastletapi.DataPlaneStateReady, status.DataPlane.State)
	}
}

func TestSandboxManager_DeleteSandbox_Success(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-delete", "test-claim", "alpine:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	resp, err := deleteSandboxForTest(manager, spec.SandboxID)

	require.NoError(t, err, "DeleteSandbox should succeed")
	assert.NotNil(t, resp)

	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1, "Should have one status")
	assert.Equal(t, spec.SandboxID, statuses[0].SandboxID)
	assert.Equal(t, fastletapi.RuntimeStateStopping, statuses[0].Runtime.State)
	assert.Equal(t, fastletapi.DataPlaneStateDraining, statuses[0].DataPlane.State)

	// Wait for async deletion to complete
	time.Sleep(100 * time.Millisecond)

	statuses = manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed after async delete")
}

func TestSandboxManager_DeleteSandbox_Idempotent(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-delete-idempotent", "test-claim", "alpine:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// First delete
	resp1, err1 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err1, "First delete should succeed")
	assert.NotNil(t, resp1)

	// Reset mock to track if DeleteSandbox is called again
	mockRuntime.Reset()

	// Second delete (should be idempotent)
	resp2, err2 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err2, "Second delete should succeed (idempotent)")
	assert.NotNil(t, resp2)

	// The runtime DeleteSandbox might be called again by asyncDelete goroutine
	// but the manager's DeleteSandbox should return immediately without queuing another delete
}

func TestSandboxManager_DeleteSandbox_NonExistent(t *testing.T) {
	// This follows the principle that DELETE operations should be idempotent
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	// Deleting a non-existent sandbox should succeed (idempotent behavior)
	resp, err := deleteSandboxForTest(manager, "non-existent-sandbox")
	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestSandboxManager_DeleteSandbox_MultipleDeletes(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-multiple-delete", "test-claim", "alpine:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	// First delete
	resp1, err1 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err1)
	assert.NotNil(t, resp1)

	// Second delete while in "terminating" phase (before async completes)
	// This should be idempotent and return success
	resp2, err2 := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err2)
	assert.NotNil(t, resp2)

	// Wait for async delete to complete
	time.Sleep(100 * time.Millisecond)

	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed after async delete")
}

func TestSandboxManager_GetSandboxStatuses(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()

	spec1 := runtimeSpecForTest("active-sb-1", "claim-1", "alpine:latest")
	spec2 := runtimeSpecForTest("active-sb-2", "claim-2", "nginx:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec1)
	require.NoError(t, err)
	_, err = ensureSandboxForTest(ctx, manager, spec2)
	require.NoError(t, err)

	_, err = deleteSandboxForTest(manager, spec1.SandboxID)
	require.NoError(t, err)

	// Wait for async delete to complete
	time.Sleep(100 * time.Millisecond)

	statuses := manager.GetSandboxStatuses(ctx)

	// Should have only the active sandbox (deleted one is completely removed)
	require.Len(t, statuses, 1, "Should have one status (only active)")

	activeStatus := statuses[0]
	assert.Equal(t, spec2.SandboxID, activeStatus.SandboxID)
	assert.Equal(t, fastletapi.RuntimeStateReady, activeStatus.Runtime.State, "Active sandbox should be running")
}

func TestSandboxManager_GetSandboxStatuses_Empty(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	statuses := manager.GetSandboxStatuses(ctx)

	assert.NotNil(t, statuses, "Statuses should not be nil")
	assert.Empty(t, statuses, "Statuses should be empty")
}

func TestSandboxManager_GetSandboxStatuses_RuntimeStatus(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sb-status", "test-claim", "alpine:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1)

	// Runtime observation carries the driver's diagnostic without flattening it
	// into an ambiguous top-level message.
	assert.NotEmpty(t, statuses[0].Runtime.Message, "runtime message should contain driver status")

	callCount := mockRuntime.GetStatusCallCount(spec.SandboxID)
	assert.Greater(t, callCount, 0, "GetSandboxStatus should be called on runtime")
}

func TestSandboxManager_GetSandboxStatuses_MultiplePhases(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()

	specs := []*sandboxCreateFixture{
		runtimeSpecForTest("sb-1", "claim-1", "alpine:latest"),
		runtimeSpecForTest("sb-2", "claim-2", "nginx:latest"),
		runtimeSpecForTest("sb-3", "claim-3", "ubuntu:latest"),
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

	statusMap := make(map[string]fastletapi.SandboxStatus)
	for _, status := range statuses {
		statusMap[status.SandboxID] = status
	}

	// First sandbox might be terminating or gone (async may have completed)
	if firstStatus, exists := statusMap[specs[0].SandboxID]; exists {
		assert.Equal(t, fastletapi.RuntimeStateStopping, firstStatus.Runtime.State, "First sandbox should be terminating if still present")
	}

	// Other sandboxes should be running
	for i := 1; i < 3; i++ {
		status := statusMap[specs[i].SandboxID]
		assert.Equal(t, fastletapi.RuntimeStateReady, status.Runtime.State, "Sandbox %s should be running", specs[i].SandboxID)
	}
}

func TestSandboxManager_GetCapacity(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	capacity := manager.GetCapacity()
	assert.Equal(t, 5, capacity, "Default capacity should be 5")
}

func TestSandboxManager_GetCapacity_Custom(t *testing.T) {
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

func TestSandboxManager_Close(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	err := manager.Close()

	assert.NoError(t, err, "Close should succeed")
	assert.True(t, mockRuntime.GetCloseCalled(), "Runtime Close should be called")
}

func TestSandboxManager_Close_MultipleCalls(t *testing.T) {
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

func TestSandboxManager_ListImages(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	images, err := manager.ListImages(ctx)

	assert.NoError(t, err, "ListImages should succeed")
	assert.Equal(t, []string{"alpine:latest", "nginx:latest"}, images, "Should return mock images")
}

func TestSandboxManager_ListImages_CustomList(t *testing.T) {
	mockRuntime := NewMockRuntime()
	customImages := []string{"custom:latest", "another:v1.0"}
	mockRuntime.SetListImages(customImages)

	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	images, err := manager.ListImages(ctx)

	assert.NoError(t, err, "ListImages should succeed")
	assert.Equal(t, customImages, images, "Should return custom images")
}

func TestSandboxManager_AsyncDelete_Timeout(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-timeout", "test-claim", "alpine:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	resp, err := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err)
	assert.NotNil(t, resp)

	// Wait for async delete to complete (should complete within timeout)
	time.Sleep(200 * time.Millisecond)

	statuses := manager.GetSandboxStatuses(ctx)
	assert.Empty(t, statuses, "Sandbox should be completely removed after async delete")
}

func TestSandboxManager_AsyncDelete_RuntimeError(t *testing.T) {
	mockRuntime := NewMockRuntime()
	manager := NewSandboxManager(mockRuntime)

	ctx := context.Background()
	spec := runtimeSpecForTest("test-sandbox-delete-error", "test-claim", "alpine:latest")

	_, err := ensureSandboxForTest(ctx, manager, spec)
	require.NoError(t, err)

	mockRuntime.SetDeleteError(errors.New("delete failed"))

	resp, err := deleteSandboxForTest(manager, spec.SandboxID)
	require.NoError(t, err)
	assert.NotNil(t, resp)

	// Wait for async delete
	time.Sleep(100 * time.Millisecond)

	// Failed runtime deletion must retain capacity and identity so a later
	// retry cannot over-admit while an orphan may still exist.
	statuses := manager.GetSandboxStatuses(ctx)
	require.Len(t, statuses, 1)
	assert.Equal(t, fastletapi.RuntimeStateFailed, statuses[0].Runtime.State)
	assert.Equal(t, fastletapi.DataPlaneStateFailed, statuses[0].DataPlane.State)
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
