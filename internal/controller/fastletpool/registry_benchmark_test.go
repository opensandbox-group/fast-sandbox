package fastletpool

import (
	"fmt"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BenchmarkRegistryAllocate(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sb", Namespace: "default"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "alpine",
			PoolRef: "default-pool",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.Allocate(sb)
	}
}

func BenchmarkRegistryAllocateWithPorts(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sb", Namespace: "default"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:        "alpine",
			PoolRef:      "default-pool",
			ExposedPorts: []int32{8080, 9090},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.Allocate(sb)
	}
}

func BenchmarkRegistryAllocateNoImageMatch(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sb", Namespace: "default"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "ubuntu", // Not in fastlet image list
			PoolRef: "default-pool",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.Allocate(sb)
	}
}

func BenchmarkRegistryAllocateLargePool(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 1000 fastlets (stress test)
	for i := 0; i < 1000; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sb", Namespace: "default"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "alpine",
			PoolRef: "default-pool",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.Allocate(sb)
	}
}

func BenchmarkRegistryRegisterOrUpdate(b *testing.B) {
	registry := NewInMemoryRegistry()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i%1000)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i%1000),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}
}

func BenchmarkRegistryGetAllFastlets(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.GetAllFastlets()
	}
}

func BenchmarkRegistryGetAllFastletsLargePool(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 1000 fastlets
	for i := 0; i < 1000; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.GetAllFastlets()
	}
}

func BenchmarkRegistryGetFastletByID(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	targetID := FastletID("fastlet-50")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.GetFastletByID(targetID)
	}
}

func BenchmarkRegistryRelease(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 10,
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sb", Namespace: "default"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:        "alpine",
			PoolRef:      "default-pool",
			ExposedPorts: []int32{8080},
		},
	}
	fastletID := FastletID("fastlet-0")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.Release(fastletID, sb)
	}
}

func BenchmarkRegistryCleanupStaleFastlets(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:            FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:         fmt.Sprintf("10.0.0.%d", i),
			Capacity:      10,
			Images:        []string{"alpine", "nginx", "redis"},
			LastHeartbeat: time.Now().Add(-10 * time.Minute), // All stale
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry.CleanupStaleFastlets(5 * time.Minute)
	}
}

// BenchmarkParallelAllocate tests concurrent allocation performance
func BenchmarkParallelAllocate(b *testing.B) {
	registry := NewInMemoryRegistry()

	// Populate with 100 fastlets
	for i := 0; i < 100; i++ {
		registry.RegisterOrUpdate(FastletInfo{
			ID:       FastletID(fmt.Sprintf("fastlet-%d", i)),
			PodIP:    fmt.Sprintf("10.0.0.%d", i),
			Capacity: 100, // Large capacity for parallel allocations
			Images:   []string{"alpine", "nginx", "redis"},
		})
	}

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sb", Namespace: "default"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "alpine",
			PoolRef: "default-pool",
		},
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			sb.Name = fmt.Sprintf("test-sb-%d", i)
			registry.Allocate(sb)
			i++
		}
	})
}
