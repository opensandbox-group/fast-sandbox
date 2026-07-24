package placement

import (
	"fmt"
	"testing"
)

func BenchmarkRegistryTopK1000(b *testing.B) {
	registry := NewInMemoryRegistry()
	for index := 0; index < 1000; index++ {
		images := []string{"ubuntu:24.04"}
		if index%10 == 0 {
			images = []string{"alpine:latest"}
		}
		seedFastlet(b, registry, readyFastlet(fmt.Sprintf("fastlet-%04d", index), index%5, 10, images...))
	}
	request := candidate("alpine:latest", "request-a")
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		registry.TopK(request, 3)
	}
}
