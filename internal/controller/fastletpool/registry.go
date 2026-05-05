package fastletpool

import (
	"context"
	"fmt"
	"sync"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Lock ordering convention:
// 1. Always acquire registry-level locks (r.mu) before slot-level locks (slot.mu)
// 2. Never hold r.mu while performing expensive operations or I/O
// 3. Release r.mu before acquiring slot.mu whenever possible to minimize contention
// 4. This prevents deadlocks and improves concurrency

// FastletID is a logical identifier for an fastlet instance.
type FastletID string

// FastletInfo describes a sandbox fastlet pod registered in controller memory.
type FastletInfo struct {
	ID              FastletID
	Namespace       string
	PodName         string
	PodIP           string
	NodeName        string
	PoolName        string
	Capacity        int
	Allocated       int
	UsedPorts       map[int32]bool
	Images          []string
	SandboxStatuses map[string]api.SandboxStatus
	LastHeartbeat   time.Time
}

// FastletRegistry defines operations to manage fastlets in controller memory.
type FastletRegistry interface {
	RegisterOrUpdate(info FastletInfo)
	GetAllFastlets() []FastletInfo
	GetFastletByID(id FastletID) (FastletInfo, bool)
	Allocate(sb *apiv1alpha1.Sandbox) (*FastletInfo, error)
	Release(id FastletID, sb *apiv1alpha1.Sandbox)
	Restore(ctx context.Context, c client.Reader) error
	Remove(id FastletID)
	CleanupStaleFastlets(timeout time.Duration) int
}

type fastletSlot struct {
	mu   sync.RWMutex
	info FastletInfo
}

type InMemoryRegistry struct {
	mu       sync.RWMutex
	fastlets map[FastletID]*fastletSlot
}

// NewInMemoryRegistry creates a new in-memory registry.
func NewInMemoryRegistry() *InMemoryRegistry {
	return &InMemoryRegistry{
		fastlets: make(map[FastletID]*fastletSlot),
	}
}

func (r *InMemoryRegistry) RegisterOrUpdate(info FastletInfo) {
	r.mu.RLock()
	slot, exists := r.fastlets[info.ID]
	r.mu.RUnlock()
	if !exists {
		r.mu.Lock()
		slot, exists = r.fastlets[info.ID]
		if !exists {
			slot = &fastletSlot{
				info: FastletInfo{
					ID:              info.ID,
					UsedPorts:       make(map[int32]bool),
					SandboxStatuses: make(map[string]api.SandboxStatus),
				},
			}
			r.fastlets[info.ID] = slot
		}
		r.mu.Unlock()
	}

	slot.mu.Lock()
	defer slot.mu.Unlock()

	allocated := slot.info.Allocated
	usedPorts := slot.info.UsedPorts
	sandboxStatuses := slot.info.SandboxStatuses

	slot.info = info
	slot.info.Allocated = allocated

	if usedPorts != nil {
		slot.info.UsedPorts = usedPorts
	} else {
		slot.info.UsedPorts = make(map[int32]bool)
	}

	if sandboxStatuses != nil && info.SandboxStatuses == nil {
		slot.info.SandboxStatuses = sandboxStatuses
	} else if info.SandboxStatuses == nil {
		slot.info.SandboxStatuses = make(map[string]api.SandboxStatus)
	}
}

func (r *InMemoryRegistry) CleanupStaleFastlets(timeout time.Duration) int {
	now := time.Now()

	// First pass: collect potential stale fastlets under read lock
	r.mu.RLock()
	slots := make([]*fastletSlot, 0, len(r.fastlets))
	ids := make([]FastletID, 0, len(r.fastlets))
	for id, slot := range r.fastlets {
		slots = append(slots, slot)
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	var staleFastlets []FastletID
	for i, slot := range slots {
		slot.mu.RLock()
		if now.Sub(slot.info.LastHeartbeat) > timeout {
			staleFastlets = append(staleFastlets, ids[i])
		}
		slot.mu.RUnlock()
	}

	// Second pass: verify and delete under write lock
	// We need to re-check that the fastlet still exists and is still stale
	if len(staleFastlets) > 0 {
		r.mu.Lock()
		for _, id := range staleFastlets {
			if slot, exists := r.fastlets[id]; exists {
				// Re-verify the fastlet is still stale before deleting
				// Note: We don't hold slot.mu here to avoid lock ordering issues.
				// This is a best-effort cleanup; if the fastlet just updated its heartbeat,
				// it will be cleaned up in the next cycle.
				slot.mu.RLock()
				stale := now.Sub(slot.info.LastHeartbeat) > timeout
				slot.mu.RUnlock()
				if stale {
					delete(r.fastlets, id)
				}
			}
		}
		r.mu.Unlock()
	}

	return len(staleFastlets)
}

func (r *InMemoryRegistry) GetAllFastlets() []FastletInfo {
	r.mu.RLock()
	slots := make([]*fastletSlot, 0, len(r.fastlets))
	for _, slot := range r.fastlets {
		slots = append(slots, slot)
	}
	r.mu.RUnlock()

	out := make([]FastletInfo, 0, len(slots))
	for _, slot := range slots {
		slot.mu.RLock()
		out = append(out, slot.info)
		slot.mu.RUnlock()
	}
	return out
}

func (r *InMemoryRegistry) GetFastletByID(id FastletID) (FastletInfo, bool) {
	r.mu.RLock()
	slot, ok := r.fastlets[id]
	r.mu.RUnlock()

	if !ok {
		return FastletInfo{}, false
	}

	slot.mu.RLock()
	info := slot.info
	slot.mu.RUnlock()

	return info, true
}

func (r *InMemoryRegistry) Allocate(sb *apiv1alpha1.Sandbox) (*FastletInfo, error) {
	totalStart := time.Now()

	for _, p := range sb.Spec.ExposedPorts {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d: must be between 1 and 65535", p)
		}
	}

	// 1. Find candidates
	candidateStart := time.Now()
	r.mu.RLock()
	candidates := make([]*fastletSlot, 0, len(r.fastlets))
	for _, slot := range r.fastlets {
		candidates = append(candidates, slot)
	}
	r.mu.RUnlock()
	candidateDuration := time.Since(candidateStart)

	var bestSlot *fastletSlot
	var minScore = 1000000
	var imageHit bool

	// 2. Score fastlets and select best
	scoreStart := time.Now()
	for _, slot := range candidates {
		slot.mu.RLock()
		info := slot.info

		if info.PoolName != sb.Spec.PoolRef {
			slot.mu.RUnlock()
			continue
		}
		if info.Namespace != sb.Namespace {
			slot.mu.RUnlock()
			continue
		}
		if info.Capacity > 0 && info.Allocated >= info.Capacity {
			slot.mu.RUnlock()
			continue
		}

		portConflict := false
		for _, p := range sb.Spec.ExposedPorts {
			if info.UsedPorts[p] {
				portConflict = true
				break
			}
		}
		if portConflict {
			slot.mu.RUnlock()
			continue
		}

		hasImage := false
		for _, img := range info.Images {
			if img == sb.Spec.Image {
				hasImage = true
				break
			}
		}

		klog.V(4).Info("Checking image affinity", "sandbox", sb.Name, "fastlet", info.ID, "hasImage", hasImage, "image", sb.Spec.Image)

		score := info.Allocated
		if !hasImage {
			score += 1000
		}

		slot.mu.RUnlock()

		if score < minScore {
			minScore = score
			bestSlot = slot
			imageHit = hasImage
		}
	}
	scoreDuration := time.Since(scoreStart)

	if bestSlot == nil {
		return nil, fmt.Errorf("insufficient capacity or port conflict in pool %s", sb.Spec.PoolRef)
	}

	// 3. Final allocation
	selectStart := time.Now()
	bestSlot.mu.Lock()
	defer bestSlot.mu.Unlock()

	info := bestSlot.info
	if info.Capacity > 0 && info.Allocated >= info.Capacity {
		return nil, fmt.Errorf("fastlet %s capacity full during allocation", info.ID)
	}
	for _, p := range sb.Spec.ExposedPorts {
		if info.UsedPorts[p] {
			return nil, fmt.Errorf("port %d conflicted during allocation", p)
		}
	}

	bestSlot.info.Allocated++
	if bestSlot.info.UsedPorts == nil {
		bestSlot.info.UsedPorts = make(map[int32]bool)
	}
	for _, p := range sb.Spec.ExposedPorts {
		bestSlot.info.UsedPorts[p] = true
	}
	selectDuration := time.Since(selectStart)
	totalDuration := time.Since(totalStart)

	klog.InfoS("Registry Allocate timing",
		"sandbox", sb.Name,
		"total_ms", totalDuration.Milliseconds(),
		"candidate_ms", candidateDuration.Milliseconds(),
		"score_ms", scoreDuration.Milliseconds(),
		"select_ms", selectDuration.Milliseconds(),
		"selectedFastlet", info.ID,
		"imageHit", imageHit,
		"fastletCount", len(candidates),
		"bestSlot.info.Allocated", bestSlot.info.Allocated)

	res := bestSlot.info
	return &res, nil
}

func (r *InMemoryRegistry) Release(id FastletID, sb *apiv1alpha1.Sandbox) {
	klog.Info("[DEBUG-REGISTRY] Release ENTER",
		"fastletID", id,
		"sandbox", sb.Name,
		"ports", sb.Spec.ExposedPorts)

	r.mu.RLock()
	slot, ok := r.fastlets[id]
	r.mu.RUnlock()

	if !ok {
		klog.Warning("[DEBUG-REGISTRY] Release: slot not found for fastlet",
			"fastletID", id,
			"impact", "Allocated will NOT decrease!")
		return
	}

	slot.mu.Lock()
	defer slot.mu.Unlock()

	klog.Info("[DEBUG-REGISTRY] Release: slot state BEFORE",
		"allocated", slot.info.Allocated,
		"sandboxInStatuses", slot.info.SandboxStatuses[sb.Name],
		"usedPorts", slot.info.UsedPorts)

	// Always release allocated slot - sandbox may have already been removed from
	// SandboxStatuses due to async deletion or heartbeat sync delay.
	// The presence or absence of the sandbox in statuses doesn't matter for
	// allocated count, only whether this specific sandbox was counting against capacity.
	if _, exists := slot.info.SandboxStatuses[sb.Name]; exists {
		delete(slot.info.SandboxStatuses, sb.Name)
		klog.Info("[DEBUG-REGISTRY] Release: removed sandbox from SandboxStatuses")
	} else {
		klog.Info("[DEBUG-REGISTRY] Release: sandbox NOT in SandboxStatuses",
			"this", "is expected if already removed by heartbeat")
	}

	if slot.info.Allocated > 0 {
		slot.info.Allocated--
		klog.Info("[DEBUG-REGISTRY] Release: DECREASED Allocated",
			"allocatedAfter", slot.info.Allocated)
	} else {
		klog.Warning("[DEBUG-REGISTRY] Release: Allocated is already 0!",
			"this", "indicates double-free or accounting bug")
	}

	for _, p := range sb.Spec.ExposedPorts {
		delete(slot.info.UsedPorts, p)
	}

	klog.Info("[DEBUG-REGISTRY] Release: slot state AFTER",
		"allocated", slot.info.Allocated,
		"usedPorts", slot.info.UsedPorts)
}

func (r *InMemoryRegistry) Restore(ctx context.Context, c client.Reader) error {
	var sbList apiv1alpha1.SandboxList
	if err := c.List(ctx, &sbList); err != nil {
		return err
	}

	// Lock ordering: Always acquire r.mu before slot.mu to maintain consistency
	// with other operations in this file. We hold r.mu while creating slots,
	// then release it before modifying individual slot contents to minimize
	// lock contention.
	r.mu.Lock()
	var slotsToRestore []struct {
		id     FastletID
		sb     *apiv1alpha1.Sandbox
		create bool
		slot   *fastletSlot
	}

	for _, sb := range sbList.Items {
		if sb.Status.AssignedFastlet != "" {
			id := FastletID(sb.Status.AssignedFastlet)
			slot, ok := r.fastlets[id]
			if !ok {
				// Create new slot but don't modify contents yet
				slot = &fastletSlot{
					info: FastletInfo{
						ID:              id,
						PodName:         string(id),
						UsedPorts:       make(map[int32]bool),
						SandboxStatuses: make(map[string]api.SandboxStatus),
						LastHeartbeat:   time.Now(),
					},
				}
				r.fastlets[id] = slot
				slotsToRestore = append(slotsToRestore, struct {
					id     FastletID
					sb     *apiv1alpha1.Sandbox
					create bool
					slot   *fastletSlot
				}{id, &sb, true, slot})
			} else {
				slotsToRestore = append(slotsToRestore, struct {
					id     FastletID
					sb     *apiv1alpha1.Sandbox
					create bool
					slot   *fastletSlot
				}{id, &sb, false, slot})
			}
		}
	}
	r.mu.Unlock()

	// Now modify each slot's contents without holding r.mu
	// This prevents lock ordering issues and minimizes critical section time
	for _, item := range slotsToRestore {
		item.slot.mu.Lock()
		if item.slot.info.UsedPorts == nil {
			item.slot.info.UsedPorts = make(map[int32]bool)
		}
		if item.slot.info.SandboxStatuses == nil {
			item.slot.info.SandboxStatuses = make(map[string]api.SandboxStatus)
		}
		item.slot.info.Allocated++
		for _, p := range item.sb.Spec.ExposedPorts {
			item.slot.info.UsedPorts[p] = true
		}
		item.slot.mu.Unlock()
	}

	return nil
}

func (r *InMemoryRegistry) Remove(id FastletID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.fastlets, id)
}
