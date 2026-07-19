package cache

import (
	"context"
	"sort"
	"strings"
	"sync"

	"fast-sandbox/internal/api"
)

const DefaultMaxInventory = 4096

type ImageSource interface {
	ListImages(ctx context.Context) ([]string, error)
}

// Tracker converts a runtime-specific image store into a bounded cache
// inventory protocol. Revision is process-local, so Epoch changes on every
// Fastlet process start and prevents a restarted process from accidentally
// matching an old revision held by a Fast-Path replica.
type Tracker struct {
	mu           sync.Mutex
	source       ImageSource
	epoch        string
	revision     uint64
	inventory    []string
	initialized  bool
	maxInventory int
}

func NewTracker(source ImageSource, epoch string, maxInventory int) *Tracker {
	if maxInventory <= 0 {
		maxInventory = DefaultMaxInventory
	}
	return &Tracker{source: source, epoch: epoch, maxInventory: maxInventory}
}

func (t *Tracker) Snapshot(ctx context.Context, cursor api.CacheCursor) (api.CacheSnapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.source == nil {
		return api.CacheSnapshot{Epoch: t.epoch, Complete: false}, nil
	}
	raw, err := t.source.ListImages(ctx)
	if err != nil {
		return api.CacheSnapshot{Epoch: t.epoch, Revision: t.revision, Complete: false}, err
	}
	normalized := NormalizeInventory(raw)
	complete := len(normalized) <= t.maxInventory
	if !complete {
		normalized = nil
	}
	if !t.initialized || !equalStrings(t.inventory, normalized) {
		t.revision++
		t.inventory = append(t.inventory[:0], normalized...)
		t.initialized = true
	}

	snapshot := api.CacheSnapshot{Epoch: t.epoch, Revision: t.revision, Complete: complete}
	if cursor.ForceFull || cursor.Epoch != t.epoch || cursor.Revision != t.revision {
		snapshot.Full = true
		if complete {
			snapshot.Images = append([]string(nil), t.inventory...)
		}
	}
	return snapshot, nil
}

func NormalizeInventory(images []string) []string {
	unique := make(map[string]struct{}, len(images))
	for _, image := range images {
		normalized := NormalizeReference(image)
		if normalized == "" || isPlatformOrContentReference(normalized) {
			continue
		}
		unique[normalized] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for image := range unique {
		result = append(result, image)
	}
	sort.Strings(result)
	return result
}

func NormalizeReference(image string) string {
	image = strings.TrimSpace(strings.ToLower(image))
	image = strings.TrimPrefix(image, "https://")
	image = strings.TrimPrefix(image, "http://")
	image = strings.TrimPrefix(image, "docker.io/")
	image = strings.TrimPrefix(image, "library/")
	return image
}

func isPlatformOrContentReference(image string) bool {
	return strings.HasPrefix(image, "sha256:") ||
		strings.HasPrefix(image, "import-") ||
		strings.HasPrefix(image, "kindest/") ||
		strings.HasPrefix(image, "fast-sandbox/") ||
		strings.HasPrefix(image, "registry.k8s.io/") ||
		strings.HasPrefix(image, "k8s.gcr.io/")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
