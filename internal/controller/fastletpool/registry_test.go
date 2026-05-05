package fastletpool

import (
	"sync"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// Test Helpers
// ============================================================================

func newTestFastletInfo(id FastletID, opts ...func(*FastletInfo)) FastletInfo {
	info := FastletInfo{
		ID:              id,
		Namespace:       "default",
		PodName:         string(id),
		PodIP:           "10.0.0.1",
		NodeName:        "test-node",
		PoolName:        "test-pool",
		Capacity:        10,
		Allocated:       0,
		UsedPorts:       make(map[int32]bool),
		Images:          []string{},
		SandboxStatuses: make(map[string]api.SandboxStatus),
		LastHeartbeat:   time.Now(),
	}
	for _, opt := range opts {
		opt(&info)
	}
	return info
}

func withNamespace(ns string) func(*FastletInfo) {
	return func(a *FastletInfo) { a.Namespace = ns }
}

func withPoolName(pool string) func(*FastletInfo) {
	return func(a *FastletInfo) { a.PoolName = pool }
}

func withCapacity(cap int) func(*FastletInfo) {
	return func(a *FastletInfo) { a.Capacity = cap }
}

func withAllocated(alloc int) func(*FastletInfo) {
	return func(a *FastletInfo) { a.Allocated = alloc }
}

func withUsedPorts(ports ...int32) func(*FastletInfo) {
	return func(a *FastletInfo) {
		a.UsedPorts = make(map[int32]bool)
		for _, p := range ports {
			a.UsedPorts[p] = true
		}
	}
}

func withImages(images ...string) func(*FastletInfo) {
	return func(a *FastletInfo) { a.Images = images }
}

func withSandboxStatus(name string, status api.SandboxStatus) func(*FastletInfo) {
	return func(a *FastletInfo) {
		if a.SandboxStatuses == nil {
			a.SandboxStatuses = make(map[string]api.SandboxStatus)
		}
		a.SandboxStatuses[name] = status
	}
}

func withLastHeartbeat(t time.Time) func(*FastletInfo) {
	return func(a *FastletInfo) { a.LastHeartbeat = t }
}

func newTestSandbox(name string, opts ...func(*apiv1alpha1.Sandbox)) *apiv1alpha1.Sandbox {
	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "alpine:latest",
			PoolRef: "test-pool",
		},
	}
	for _, opt := range opts {
		opt(sb)
	}
	return sb
}

func withSandboxNamespace(ns string) func(*apiv1alpha1.Sandbox) {
	return func(sb *apiv1alpha1.Sandbox) { sb.Namespace = ns }
}

func withSandboxPoolRef(pool string) func(*apiv1alpha1.Sandbox) {
	return func(sb *apiv1alpha1.Sandbox) { sb.Spec.PoolRef = pool }
}

func withSandboxImage(image string) func(*apiv1alpha1.Sandbox) {
	return func(sb *apiv1alpha1.Sandbox) { sb.Spec.Image = image }
}

func withSandboxPorts(ports ...int32) func(*apiv1alpha1.Sandbox) {
	return func(sb *apiv1alpha1.Sandbox) { sb.Spec.ExposedPorts = ports }
}

// ============================================================================
// 1. RegisterOrUpdate Tests
// ============================================================================

func TestInMemoryRegistry_RegisterOrUpdate_NewFastlet(t *testing.T) {
	// R-01: Registering a new fastlet initializes it correctly
	registry := NewInMemoryRegistry()

	fastletInfo := newTestFastletInfo("fastlet-1",
		withPoolName("pool-a"),
		withCapacity(5),
		withImages("alpine:latest", "nginx:latest"),
	)

	registry.RegisterOrUpdate(fastletInfo)

	// Verify fastlet was registered
	fastlet, ok := registry.GetFastletByID("fastlet-1")
	require.True(t, ok, "Fastlet should be registered")
	assert.Equal(t, FastletID("fastlet-1"), fastlet.ID)
	assert.Equal(t, "pool-a", fastlet.PoolName)
	assert.Equal(t, 5, fastlet.Capacity)
	assert.Equal(t, 0, fastlet.Allocated, "New fastlet should start with 0 allocations")
	assert.NotNil(t, fastlet.UsedPorts, "UsedPorts should be initialized")
	assert.NotNil(t, fastlet.SandboxStatuses, "SandboxStatuses should be initialized")
	assert.Equal(t, []string{"alpine:latest", "nginx:latest"}, fastlet.Images)
}

func TestInMemoryRegistry_RegisterOrUpdate_UpdateExisting(t *testing.T) {
	// R-02: Updating an existing fastlet preserves allocated/ports
	registry := NewInMemoryRegistry()

	// Initial registration
	fastletInfo := newTestFastletInfo("fastlet-1",
		withCapacity(5),
	)
	registry.RegisterOrUpdate(fastletInfo)

	// Perform an allocation to set allocated count and ports
	sandbox := newTestSandbox("test-sb", withSandboxPorts(8080, 9090))
	_, err := registry.Allocate(sandbox)
	require.NoError(t, err)

	// Verify state after allocation
	fastlet, _ := registry.GetFastletByID("fastlet-1")
	assert.Equal(t, 1, fastlet.Allocated)
	assert.True(t, fastlet.UsedPorts[8080])
	assert.True(t, fastlet.UsedPorts[9090])

	// Update with new heartbeat info (simulating heartbeat from fastlet)
	updatedInfo := newTestFastletInfo("fastlet-1",
		withCapacity(10), // Capacity changed
		withImages("ubuntu:latest"),
	)
	registry.RegisterOrUpdate(updatedInfo)

	// Verify allocated count and ports are preserved
	fastlet, ok := registry.GetFastletByID("fastlet-1")
	require.True(t, ok)
	assert.Equal(t, 1, fastlet.Allocated, "Allocated should be preserved")
	assert.Equal(t, 10, fastlet.Capacity, "Capacity should be updated")
	assert.True(t, fastlet.UsedPorts[8080], "UsedPorts should be preserved")
	assert.True(t, fastlet.UsedPorts[9090], "UsedPorts should be preserved")
	assert.Equal(t, []string{"ubuntu:latest"}, fastlet.Images, "Images should be updated")
}

func TestInMemoryRegistry_RegisterOrUpdate_PreservesSandboxStatuses(t *testing.T) {
	// R-03: Updating preserves existing sandbox statuses when input has nil
	registry := NewInMemoryRegistry()

	// Initial registration with sandbox status
	fastletInfo := newTestFastletInfo("fastlet-1")
	registry.RegisterOrUpdate(fastletInfo)

	// Manually add a sandbox status (simulating status sync from fastlet)
	fastlet, _ := registry.GetFastletByID("fastlet-1")
	fastlet.SandboxStatuses["sb-1"] = api.SandboxStatus{Phase: "running"}

	// Update with info that has nil SandboxStatuses (default from newTestFastletInfo)
	updatedInfo := FastletInfo{
		ID:        "fastlet-1",
		PoolName:  "test-pool",
		Capacity:  20,
		Namespace: "default",
		PodName:   "fastlet-1",
		// SandboxStatuses is nil (not set)
	}
	registry.RegisterOrUpdate(updatedInfo)

	// Verify sandbox status is preserved
	fastlet, ok := registry.GetFastletByID("fastlet-1")
	require.True(t, ok)
	assert.Contains(t, fastlet.SandboxStatuses, "sb-1")
	assert.Equal(t, "running", fastlet.SandboxStatuses["sb-1"].Phase)
}

// ============================================================================
// 2. Allocate Tests
// ============================================================================

func TestInMemoryRegistry_Allocate_ImageAffinity(t *testing.T) {
	// A-01: Allocation prefers fastlets with cached image
	registry := NewInMemoryRegistry()

	// Register two fastlets - one with the image, one without
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-with-image",
		withPoolName("test-pool"),
		withCapacity(10),
		withImages("alpine:latest", "nginx:latest"),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-without-image",
		withPoolName("test-pool"),
		withCapacity(10),
		withImages("ubuntu:latest"),
	))

	// Simulate existing allocations by allocating dummy sandboxes
	for i := 0; i < 3; i++ {
		dummySB := newTestSandbox("dummy-" + string(rune('0'+i)))
		registry.Allocate(dummySB)
	}
	// fastlet-with-image now has 3 allocations
	for i := 0; i < 1; i++ {
		dummySB := newTestSandbox("dummy2-" + string(rune('0'+i)))
		registry.Allocate(dummySB)
	}
	// fastlet-without-image now has 1 allocation (both fastlets share capacity since they're in same pool)

	sandbox := newTestSandbox("test-sb",
		withSandboxImage("alpine:latest"),
	)

	fastlet, err := registry.Allocate(sandbox)
	require.NoError(t, err)
	// The fastlet with the cached image should be preferred
	// Both fastlets share the pool, so the allocation goes to the one with cached image
	assert.Equal(t, "test-pool", fastlet.PoolName)

	// Verify an allocation happened
	fastlets := registry.GetAllFastlets()
	totalAllocated := 0
	for _, a := range fastlets {
		totalAllocated += a.Allocated
	}
	assert.Equal(t, 5, totalAllocated, "Should have 5 total allocations")
}

func TestInMemoryRegistry_Allocate_CapacityCheck(t *testing.T) {
	// A-02: Allocation fails when no capacity
	registry := NewInMemoryRegistry()

	// Register a limited capacity fastlet
	registry.RegisterOrUpdate(newTestFastletInfo("full-fastlet",
		withPoolName("test-pool"),
		withCapacity(2),
	))

	// Fill it to capacity
	for i := 0; i < 2; i++ {
		dummySB := newTestSandbox("fill-" + string(rune('0'+i)))
		_, err := registry.Allocate(dummySB)
		require.NoError(t, err)
	}

	// Verify it's full
	fastlet, _ := registry.GetFastletByID("full-fastlet")
	require.Equal(t, 2, fastlet.Allocated)

	sandbox := newTestSandbox("test-sb",
		withSandboxPoolRef("test-pool"),
	)

	_, err := registry.Allocate(sandbox)
	assert.Error(t, err, "Should fail when no capacity in matching pool")
	assert.Contains(t, err.Error(), "insufficient capacity")
}

func TestInMemoryRegistry_Allocate_PortConflict(t *testing.T) {
	// A-03: Allocation handles port conflicts correctly
	registry := NewInMemoryRegistry()

	// Register a single fastlet
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
		withCapacity(10),
	))

	// Allocate one sandbox with port 8080
	sb1 := newTestSandbox("sb-1", withSandboxPorts(8080))
	_, _ = registry.Allocate(sb1)

	// Try to allocate another sandbox with BOTH 8080 and 9090
	// This should fail because 8080 is already in use
	sandbox := newTestSandbox("test-sb",
		withSandboxPorts(8080, 9090),
	)

	_, err := registry.Allocate(sandbox)
	assert.Error(t, err, "Should fail when port 8080 is already in use")
	assert.Contains(t, err.Error(), "insufficient capacity or port conflict")
}

func TestInMemoryRegistry_Allocate_SelectsFastletWithAvailablePorts(t *testing.T) {
	// A-04: Allocation selects fastlet with available ports
	registry := NewInMemoryRegistry()

	// Register two fastlets
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
		withCapacity(10),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withPoolName("test-pool"),
		withCapacity(10),
	))

	// Allocate a sandbox with port 8080 to fastlet-1
	sb1 := newTestSandbox("sb-1", withSandboxPorts(8080))
	allocated1, _ := registry.Allocate(sb1)
	fastletWithPort8080 := allocated1.ID

	// Now try to allocate a sandbox that needs BOTH 8080 and 9090
	// It should go to the fastlet that doesn't have 8080 in use
	sandbox := newTestSandbox("test-sb",
		withSandboxPorts(8080, 9090),
	)

	fastlet, err := registry.Allocate(sandbox)
	require.NoError(t, err)
	assert.NotEqual(t, fastletWithPort8080, fastlet.ID, "Should select fastlet without port 8080 conflict")
	assert.True(t, fastlet.UsedPorts[8080], "Port 8080 should be marked as used in returned info")
	assert.True(t, fastlet.UsedPorts[9090], "Port 9090 should be marked as used in returned info")

	// Verify ports are marked in registry
	storedFastlet, _ := registry.GetFastletByID(fastlet.ID)
	assert.True(t, storedFastlet.UsedPorts[8080])
	assert.True(t, storedFastlet.UsedPorts[9090])
}

func TestInMemoryRegistry_Allocate_NoMatch(t *testing.T) {
	// A-05: Allocation returns error when no suitable fastlets
	registry := NewInMemoryRegistry()

	// Register fastlets in different namespace
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withNamespace("kube-system"),
		withPoolName("test-pool"),
	))

	// Register fastlets in different pool
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withNamespace("default"),
		withPoolName("other-pool"),
	))

	sandbox := newTestSandbox("test-sb",
		withSandboxNamespace("default"),
		withSandboxPoolRef("test-pool"),
	)

	_, err := registry.Allocate(sandbox)
	assert.Error(t, err, "Should fail when no fastlets match namespace and pool")
	assert.Contains(t, err.Error(), "insufficient capacity")
}

func TestInMemoryRegistry_Allocate_ZeroCapacity(t *testing.T) {
	// A-06: Fastlets with zero capacity have no limit
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("unlimited-fastlet",
		withPoolName("test-pool"),
		withCapacity(0), // Zero capacity means unlimited
	))

	// Allocate many sandboxes - should all succeed
	for i := 0; i < 100; i++ {
		dummySB := newTestSandbox("unlimited-" + string(rune('0'+i%10)))
		_, err := registry.Allocate(dummySB)
		require.NoError(t, err)
	}

	fastlet, _ := registry.GetFastletByID("unlimited-fastlet")
	assert.Equal(t, 100, fastlet.Allocated, "Should handle many allocations with capacity=0")

	// One more should still work
	sandbox := newTestSandbox("test-sb")
	allocatedFastlet, err := registry.Allocate(sandbox)
	require.NoError(t, err)
	assert.Equal(t, FastletID("unlimited-fastlet"), allocatedFastlet.ID)
	assert.Equal(t, 101, allocatedFastlet.Allocated)
}

func TestInMemoryRegistry_Allocate_InvalidPort(t *testing.T) {
	// A-07: Invalid port numbers are rejected
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
	))

	sandbox := newTestSandbox("test-sb",
		withSandboxPorts(0), // Invalid port
	)

	_, err := registry.Allocate(sandbox)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid port")
}

func TestInMemoryRegistry_Allocate_LeastLoadedPreferred(t *testing.T) {
	// A-08: When image affinity doesn't apply, prefer least loaded fastlet
	registry := NewInMemoryRegistry()

	// Build distinct load so selection does not depend on map iteration order.
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
		withCapacity(10),
	))
	for i := 0; i < 5; i++ {
		_, err := registry.Allocate(newTestSandbox("load-" + string(rune('0'+i))))
		require.NoError(t, err)
	}

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withPoolName("test-pool"),
		withCapacity(10),
	))

	// Verify state
	fastlet1, _ := registry.GetFastletByID("fastlet-1")
	fastlet2, _ := registry.GetFastletByID("fastlet-2")
	require.Equal(t, 5, fastlet1.Allocated, "fastlet-1 should start more loaded")
	require.Equal(t, 0, fastlet2.Allocated, "fastlet-2 should start least loaded")

	sandbox := newTestSandbox("test-sb",
		withSandboxImage("ubuntu:latest"), // Neither fastlet has this image
	)

	fastlet, err := registry.Allocate(sandbox)
	require.NoError(t, err)
	assert.Equal(t, FastletID("fastlet-2"), fastlet.ID, "Should prefer least loaded fastlet")
	assert.Equal(t, 1, fastlet.Allocated)
}

func TestInMemoryRegistry_Allocate_NamespaceMatch(t *testing.T) {
	// A-09: Allocation only considers fastlets in the same namespace
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withNamespace("default"),
		withPoolName("test-pool"),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withNamespace("kube-system"),
		withPoolName("test-pool"),
	))

	sandbox := newTestSandbox("test-sb",
		withSandboxNamespace("default"),
	)

	fastlet, err := registry.Allocate(sandbox)
	require.NoError(t, err)
	assert.Equal(t, FastletID("fastlet-1"), fastlet.ID, "Should match namespace")
}

func TestInMemoryRegistry_Allocate_ImageAffinityOverLoad(t *testing.T) {
	// A-10: Image affinity is preferred over lower load
	registry := NewInMemoryRegistry()

	// Fastlet with cached image
	registry.RegisterOrUpdate(newTestFastletInfo("cached-fastlet",
		withPoolName("test-pool"),
		withCapacity(10),
		withImages("alpine:latest"),
	))
	// Fastlet without cached image - very limited capacity
	registry.RegisterOrUpdate(newTestFastletInfo("empty-fastlet",
		withPoolName("test-pool"),
		withCapacity(1), // Will fill after 1 allocation
		withImages("ubuntu:latest"),
	))

	// First allocation will go to cached-fastlet (lower ID, both have 0 allocated)
	dummySB1 := newTestSandbox("dummy-1", withSandboxImage("ubuntu:latest"))
	registry.Allocate(dummySB1) // Goes to cached-fastlet (both 0 allocated, tie-breaker)

	// Second allocation will go to empty-fastlet (still 0 allocated vs cached-fastlet's 1)
	dummySB2 := newTestSandbox("dummy-2", withSandboxImage("ubuntu:latest"))
	registry.Allocate(dummySB2) // Goes to empty-fastlet

	// Now cached-fastlet has 1, empty-fastlet has 1 (and is full at capacity=1)

	// Verify state
	cachedFastlet, _ := registry.GetFastletByID("cached-fastlet")
	emptyFastlet, _ := registry.GetFastletByID("empty-fastlet")
	require.Equal(t, 1, cachedFastlet.Allocated, "cached-fastlet should have 1")
	require.Equal(t, 1, emptyFastlet.Allocated, "empty-fastlet should be full")

	// Request with alpine image - cached-fastlet has image affinity
	// Score cached-fastlet = 1 + 0 (has image) = 1
	// Score empty-fastlet = full (capacity=1, allocated=1), so skipped
	sandbox := newTestSandbox("test-sb",
		withSandboxImage("alpine:latest"),
	)

	fastlet, err := registry.Allocate(sandbox)
	require.NoError(t, err)
	assert.Equal(t, FastletID("cached-fastlet"), fastlet.ID, "Should prefer image affinity over lower load")
	assert.Equal(t, 2, fastlet.Allocated)
}

// ============================================================================
// 3. Release Tests
// ============================================================================

func TestInMemoryRegistry_Release(t *testing.T) {
	// L-01: Release decrements allocation and frees ports
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
		withCapacity(10),
	))

	sandbox := newTestSandbox("test-sb",
		withSandboxPorts(8080, 9090),
	)

	// First allocate
	_, err := registry.Allocate(sandbox)
	require.NoError(t, err)

	// Verify allocation
	fastlet, _ := registry.GetFastletByID("fastlet-1")
	assert.Equal(t, 1, fastlet.Allocated)
	assert.True(t, fastlet.UsedPorts[8080])

	// Now release
	registry.Release("fastlet-1", sandbox)

	fastlet, ok := registry.GetFastletByID("fastlet-1")
	require.True(t, ok)
	assert.Equal(t, 0, fastlet.Allocated, "Allocated should be decremented")
	assert.False(t, fastlet.UsedPorts[8080], "Port 8080 should be freed")
	assert.False(t, fastlet.UsedPorts[9090], "Port 9090 should be freed")
}

func TestInMemoryRegistry_Release_NonExistent(t *testing.T) {
	// L-02: Release handles non-existent fastlets gracefully
	registry := NewInMemoryRegistry()

	sandbox := newTestSandbox("test-sb")

	// Should not panic
	registry.Release("non-existent", sandbox)

	// Registry should remain empty
	fastlets := registry.GetAllFastlets()
	assert.Empty(t, fastlets)
}

func TestInMemoryRegistry_Release_WithSandboxStatus(t *testing.T) {
	// L-03: Release removes sandbox status
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
		withCapacity(10),
		withSandboxStatus("test-sb", api.SandboxStatus{Phase: "running"}),
	))

	sandbox := newTestSandbox("test-sb",
		withSandboxPorts(8080),
	)

	// First allocate to set ports
	_, err := registry.Allocate(sandbox)
	require.NoError(t, err)

	// Add sandbox status
	fastlet, _ := registry.GetFastletByID("fastlet-1")
	fastlet.SandboxStatuses["test-sb"] = api.SandboxStatus{Phase: "running"}

	// Now release
	registry.Release("fastlet-1", sandbox)

	fastlet, _ = registry.GetFastletByID("fastlet-1")
	_, exists := fastlet.SandboxStatuses["test-sb"]
	assert.False(t, exists, "Sandbox status should be removed")
}

func TestInMemoryRegistry_Release_WithPartialPortMatch(t *testing.T) {
	// L-04: Release only frees the specified ports
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("test-pool"),
		withCapacity(10),
	))

	// Allocate first sandbox with ports 8080, 9090
	sb1 := newTestSandbox("sb-1", withSandboxPorts(8080, 9090))
	_, err := registry.Allocate(sb1)
	require.NoError(t, err)

	// Allocate second sandbox with port 7070
	sb2 := newTestSandbox("sb-2", withSandboxPorts(7070))
	_, err = registry.Allocate(sb2)
	require.NoError(t, err)

	// Verify all ports are in use
	fastlet, _ := registry.GetFastletByID("fastlet-1")
	assert.True(t, fastlet.UsedPorts[8080])
	assert.True(t, fastlet.UsedPorts[9090])
	assert.True(t, fastlet.UsedPorts[7070])
	assert.Equal(t, 2, fastlet.Allocated)

	// Release first sandbox
	registry.Release("fastlet-1", sb1)

	fastlet, _ = registry.GetFastletByID("fastlet-1")
	assert.Equal(t, 1, fastlet.Allocated)
	assert.False(t, fastlet.UsedPorts[8080])
	assert.False(t, fastlet.UsedPorts[9090])
	assert.True(t, fastlet.UsedPorts[7070], "Port 7070 should remain in use")
}

// ============================================================================
// 4. GetAllFastlets Tests
// ============================================================================

func TestInMemoryRegistry_GetAllFastlets(t *testing.T) {
	// G-01: Getting all fastlets returns correct list
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withPoolName("pool-a"),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withPoolName("pool-b"),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-3",
		withPoolName("pool-a"),
	))

	fastlets := registry.GetAllFastlets()
	assert.Len(t, fastlets, 3, "Should return all 3 fastlets")

	fastletIDs := make(map[FastletID]bool)
	for _, a := range fastlets {
		fastletIDs[a.ID] = true
	}
	assert.True(t, fastletIDs["fastlet-1"])
	assert.True(t, fastletIDs["fastlet-2"])
	assert.True(t, fastletIDs["fastlet-3"])
}

func TestInMemoryRegistry_GetAllFastlets_Empty(t *testing.T) {
	// G-02: Empty registry returns empty list
	registry := NewInMemoryRegistry()

	fastlets := registry.GetAllFastlets()
	assert.Empty(t, fastlets)
}

func TestInMemoryRegistry_GetAllFastlets_ThreadSafe(t *testing.T) {
	// G-03: GetAllFastlets is thread-safe during concurrent operations
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1"))

	var wg sync.WaitGroup
	wg.Add(2)

	// Concurrent read and update
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_ = registry.GetAllFastlets()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
				withCapacity(5+i),
			))
		}
	}()

	wg.Wait()

	// Should not panic or deadlock
	fastlets := registry.GetAllFastlets()
	assert.NotEmpty(t, fastlets)
}

// ============================================================================
// 5. GetFastletByID Tests
// ============================================================================

func TestInMemoryRegistry_GetFastletByID(t *testing.T) {
	// GB-01: Getting fastlet by ID works correctly
	registry := NewInMemoryRegistry()

	expectedInfo := newTestFastletInfo("fastlet-1",
		withPoolName("pool-a"),
		withCapacity(5),
		withImages("alpine:latest"),
	)
	registry.RegisterOrUpdate(expectedInfo)

	fastlet, ok := registry.GetFastletByID("fastlet-1")
	require.True(t, ok)
	assert.Equal(t, FastletID("fastlet-1"), fastlet.ID)
	assert.Equal(t, "pool-a", fastlet.PoolName)
	assert.Equal(t, 5, fastlet.Capacity)
	assert.Equal(t, []string{"alpine:latest"}, fastlet.Images)
}

func TestInMemoryRegistry_GetFastletByID_NotFound(t *testing.T) {
	// GB-02: Getting non-existent fastlet returns false
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1"))

	_, ok := registry.GetFastletByID("non-existent")
	assert.False(t, ok, "Should return false for non-existent fastlet")
}

func TestInMemoryRegistry_GetFastletByID_ThreadSafe(t *testing.T) {
	// GB-03: GetFastletByID is thread-safe
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1", withCapacity(5)))

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_, _ = registry.GetFastletByID("fastlet-1")
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
				withCapacity(5+i),
			))
		}
	}()

	wg.Wait()

	// Should not panic or deadlock
	fastlet, ok := registry.GetFastletByID("fastlet-1")
	assert.True(t, ok)
	assert.NotEqual(t, 5, fastlet.Capacity, "Capacity should have been updated")
}

// ============================================================================
// 6. Remove Tests
// ============================================================================

func TestInMemoryRegistry_Remove(t *testing.T) {
	// RM-01: Remove deletes fastlet from registry
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1"))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2"))

	// Verify both exist
	_, ok1 := registry.GetFastletByID("fastlet-1")
	_, ok2 := registry.GetFastletByID("fastlet-2")
	assert.True(t, ok1)
	assert.True(t, ok2)

	registry.Remove("fastlet-1")

	// Verify fastlet-1 is gone, fastlet-2 remains
	_, ok1 = registry.GetFastletByID("fastlet-1")
	_, ok2 = registry.GetFastletByID("fastlet-2")
	assert.False(t, ok1, "fastlet-1 should be removed")
	assert.True(t, ok2, "fastlet-2 should still exist")
}

func TestInMemoryRegistry_Remove_NonExistent(t *testing.T) {
	// RM-02: Removing non-existent fastlet is safe
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1"))

	// Should not panic
	registry.Remove("non-existent")

	// Original fastlet should still exist
	_, ok := registry.GetFastletByID("fastlet-1")
	assert.True(t, ok)
}

// ============================================================================
// 7. CleanupStaleFastlets Tests
// ============================================================================

func TestInMemoryRegistry_CleanupStaleFastlets(t *testing.T) {
	// C-01: Cleanup removes fastlets with stale heartbeats
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fresh-fastlet",
		withLastHeartbeat(time.Now().Add(-30*time.Second)),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("stale-fastlet",
		withLastHeartbeat(time.Now().Add(-5*time.Minute)),
	))

	timeout := 2 * time.Minute
	cleaned := registry.CleanupStaleFastlets(timeout)

	assert.Equal(t, 1, cleaned, "Should clean 1 stale fastlet")

	fastlets := registry.GetAllFastlets()
	assert.Len(t, fastlets, 1, "Should have 1 fastlet remaining")

	_, ok := registry.GetFastletByID("fresh-fastlet")
	assert.True(t, ok, "fresh-fastlet should remain")

	_, ok = registry.GetFastletByID("stale-fastlet")
	assert.False(t, ok, "stale-fastlet should be removed")
}

func TestInMemoryRegistry_CleanupStaleFastlets_None(t *testing.T) {
	// C-02: No fastlets cleaned when all are fresh
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withLastHeartbeat(time.Now().Add(-30*time.Second)),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withLastHeartbeat(time.Now()),
	))

	timeout := 5 * time.Minute
	cleaned := registry.CleanupStaleFastlets(timeout)

	assert.Equal(t, 0, cleaned)

	fastlets := registry.GetAllFastlets()
	assert.Len(t, fastlets, 2)
}

func TestInMemoryRegistry_CleanupStaleFastlets_All(t *testing.T) {
	// C-03: All fastlets cleaned when all are stale
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withLastHeartbeat(time.Now().Add(-10*time.Minute)),
	))
	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-2",
		withLastHeartbeat(time.Now().Add(-1*time.Hour)),
	))

	timeout := 1 * time.Minute
	cleaned := registry.CleanupStaleFastlets(timeout)

	assert.Equal(t, 2, cleaned)

	fastlets := registry.GetAllFastlets()
	assert.Empty(t, fastlets)
}

func TestInMemoryRegistry_CleanupStaleFastlets_EmptyRegistry(t *testing.T) {
	// C-04: Cleanup on empty registry is safe
	registry := NewInMemoryRegistry()

	timeout := 1 * time.Minute
	cleaned := registry.CleanupStaleFastlets(timeout)

	assert.Equal(t, 0, cleaned)
}

func TestInMemoryRegistry_CleanupStaleFastlets_Boundary(t *testing.T) {
	// C-05: Fastlets exactly at timeout boundary are cleaned
	registry := NewInMemoryRegistry()

	// Fastlet exactly at timeout boundary (using slightly more to be safe)
	registry.RegisterOrUpdate(newTestFastletInfo("boundary-fastlet",
		withLastHeartbeat(time.Now().Add(-2*time.Minute-time.Second)),
	))

	timeout := 2 * time.Minute
	cleaned := registry.CleanupStaleFastlets(timeout)

	assert.Equal(t, 1, cleaned, "Fastlet at boundary should be cleaned")
}

// ============================================================================
// 8. Thread Safety Tests
// ============================================================================

func TestInMemoryRegistry_ConcurrentRegister(t *testing.T) {
	// T-01: Concurrent registrations are safe
	registry := NewInMemoryRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			fastletID := FastletID("fastlet-" + string(rune('0'+idx)))
			registry.RegisterOrUpdate(newTestFastletInfo(fastletID))
		}(i)
	}

	wg.Wait()

	fastlets := registry.GetAllFastlets()
	assert.NotEmpty(t, fastlets, "Should have some fastlets registered")
}

func TestInMemoryRegistry_ConcurrentAllocate(t *testing.T) {
	// T-02: Concurrent allocations work correctly
	registry := NewInMemoryRegistry()

	// Register fastlets with capacity
	for i := 0; i < 5; i++ {
		fastletID := FastletID("fastlet-" + string(rune('0'+i)))
		registry.RegisterOrUpdate(newTestFastletInfo(fastletID,
			withCapacity(10),
		))
	}

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sandbox := newTestSandbox("test-sb")
			_, err := registry.Allocate(sandbox)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// Verify total allocations
	fastlets := registry.GetAllFastlets()
	totalAllocated := 0
	for _, a := range fastlets {
		totalAllocated += a.Allocated
	}
	assert.Equal(t, successCount, totalAllocated, "All successful allocations should be counted")
}

func TestInMemoryRegistry_ConcurrentRelease(t *testing.T) {
	// T-03: Concurrent releases are safe
	registry := NewInMemoryRegistry()

	registry.RegisterOrUpdate(newTestFastletInfo("fastlet-1",
		withCapacity(100),
	))

	// Allocate some sandboxes
	for i := 0; i < 10; i++ {
		sandbox := newTestSandbox("sb-" + string(rune('0'+i)))
		registry.Allocate(sandbox)
	}

	var wg sync.WaitGroup

	// Concurrent releases
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sandbox := newTestSandbox("sb-" + string(rune('0'+idx)))
			registry.Release("fastlet-1", sandbox)
		}(i)
	}

	wg.Wait()

	fastlet, _ := registry.GetFastletByID("fastlet-1")
	assert.Equal(t, 0, fastlet.Allocated, "All allocations should be released")
}
