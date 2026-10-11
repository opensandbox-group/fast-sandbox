package orchestrator

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// BeginFastPathCreate marks RPC ownership before the intent becomes visible.
// The colocated reconciler may project status but must defer runtime calls.
// This is process-local coordination; durable assignment CAS still fences
// separate processes, and a crash drops the tracker for controller recovery.
func (o *Orchestrator) BeginFastPathCreate(key types.NamespacedName) func() {
	o.activeCreatesMu.Lock()
	if o.activeCreates == nil {
		o.activeCreates = make(map[types.NamespacedName]int)
	}
	o.activeCreates[key]++
	o.activeCreatesMu.Unlock()
	return sync.OnceFunc(func() {
		o.activeCreatesMu.Lock()
		defer o.activeCreatesMu.Unlock()
		if o.activeCreates[key] <= 1 {
			delete(o.activeCreates, key)
		} else {
			o.activeCreates[key]--
		}
	})
}

func (o *Orchestrator) FastPathCreateActive(key types.NamespacedName) bool {
	o.activeCreatesMu.Lock()
	defer o.activeCreatesMu.Unlock()
	return o.activeCreates[key] > 0
}
