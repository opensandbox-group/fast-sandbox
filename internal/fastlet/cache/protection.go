package cache

import (
	"sort"
	"sync"
	"time"
)

type ProtectionReason string

const (
	ProtectWarm   ProtectionReason = "PoolWarm"
	ProtectActive ProtectionReason = "ActiveSandbox"
	ProtectInfra  ProtectionReason = "InfraArtifact"
	ProtectHot    ProtectionReason = "HotImage"
)

type protection struct {
	counts   map[ProtectionReason]int
	hotUntil time.Time
}

// ProtectionIndex is the policy boundary used before any cache eviction. It
// is runtime-neutral and safe to use even while node-scoped containerd GC is
// disabled pending cross-Fastlet coordination.
type ProtectionIndex struct {
	mu    sync.Mutex
	items map[string]*protection
	now   func() time.Time
}

func NewProtectionIndex(now func() time.Time) *ProtectionIndex {
	if now == nil {
		now = time.Now
	}
	return &ProtectionIndex{items: make(map[string]*protection), now: now}
}

func (p *ProtectionIndex) Protect(reference string, reason ProtectionReason) {
	reference = NormalizeReference(reference)
	if reference == "" {
		return
	}
	p.mu.Lock()
	item := p.itemLocked(reference)
	item.counts[reason]++
	p.mu.Unlock()
}

func (p *ProtectionIndex) Unprotect(reference string, reason ProtectionReason) {
	reference = NormalizeReference(reference)
	p.mu.Lock()
	defer p.mu.Unlock()
	item := p.items[reference]
	if item == nil {
		return
	}
	if item.counts[reason] > 1 {
		item.counts[reason]--
	} else {
		delete(item.counts, reason)
	}
	p.deleteEmptyLocked(reference, item)
}

func (p *ProtectionIndex) ProtectHotUntil(reference string, until time.Time) {
	reference = NormalizeReference(reference)
	if reference == "" {
		return
	}
	p.mu.Lock()
	item := p.itemLocked(reference)
	if until.After(item.hotUntil) {
		item.hotUntil = until
	}
	p.mu.Unlock()
}

func (p *ProtectionIndex) Replace(reason ProtectionReason, references []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for reference, item := range p.items {
		delete(item.counts, reason)
		p.deleteEmptyLocked(reference, item)
	}
	for _, reference := range references {
		reference = NormalizeReference(reference)
		if reference == "" {
			continue
		}
		p.itemLocked(reference).counts[reason] = 1
	}
}

func (p *ProtectionIndex) IsProtected(reference string) bool {
	reference = NormalizeReference(reference)
	p.mu.Lock()
	defer p.mu.Unlock()
	item := p.items[reference]
	if item == nil {
		return false
	}
	if len(item.counts) > 0 || p.now().Before(item.hotUntil) {
		return true
	}
	delete(p.items, reference)
	return false
}

func (p *ProtectionIndex) PlanEviction(candidates []string) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !p.IsProtected(candidate) {
			result = append(result, NormalizeReference(candidate))
		}
	}
	sort.Strings(result)
	return result
}

func (p *ProtectionIndex) itemLocked(reference string) *protection {
	item := p.items[reference]
	if item == nil {
		item = &protection{counts: make(map[ProtectionReason]int)}
		p.items[reference] = item
	}
	return item
}

func (p *ProtectionIndex) deleteEmptyLocked(reference string, item *protection) {
	if len(item.counts) == 0 && !p.now().Before(item.hotUntil) {
		delete(p.items, reference)
	}
}
