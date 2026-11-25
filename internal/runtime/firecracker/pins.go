package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Node-wide image pins. The image cache under StateRoot is shared by every
// Fastlet on the node (one per Pool Pod). Sandbox state only tells a GC which
// images running Sandboxes use; it says nothing about another Pool's warm
// images or a delivery still in flight. Each Fastlet therefore publishes the
// cache keys it needs kept (its warm images, images with an in-flight
// delivery, and images used within the last hour) to
// <StateRoot>/pins/<owner>.json, and every GC honours the fresh pin files of
// all Fastlets on the node.
const (
	imagePinDir = "pins"
	// imageRecentUseWindow keeps an image pinned after its last pull or boot.
	imageRecentUseWindow = time.Hour
	// minImagePinTTL bounds how long the pin file of a vanished Fastlet keeps
	// protecting images; the effective TTL is max(3 x GC interval, this).
	minImagePinTTL = 30 * time.Minute
	// imagePinRefreshAge is how old an unchanged pin file may get before a GC
	// tick rewrites it; it stays well inside minImagePinTTL.
	imagePinRefreshAge = 5 * time.Minute
)

type imagePinFile struct {
	UpdatedAt time.Time `json:"updatedAt"`
	// Images lists cache keys (imageKey of the reference).
	Images []string `json:"images"`
}

// imagePinTTL is how long a pin file stays authoritative without a refresh.
func imagePinTTL(gcInterval time.Duration) time.Duration {
	return max(3*gcInterval, minImagePinTTL)
}

// pinImage keeps image cached for as long as this Fastlet runs (warm images).
func (d *Driver) pinImage(image string) {
	d.mu.Lock()
	if d.pinnedImages == nil {
		d.pinnedImages = make(map[string]struct{})
	}
	d.pinnedImages[imageKey(image)] = struct{}{}
	d.mu.Unlock()
	d.publishImagePins(true)
}

// noteImageUse pins image for imageRecentUseWindow after a pull, boot, or the
// start of a delivery.
func (d *Driver) noteImageUse(image string) {
	d.mu.Lock()
	if d.recentImages == nil {
		d.recentImages = make(map[string]time.Time)
	}
	d.recentImages[imageKey(image)] = time.Now()
	d.mu.Unlock()
}

// imagePinOwner names this Fastlet's pin file.
func (d *Driver) imagePinOwner() string {
	if d.podUID != "" {
		return d.podUID
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

// currentImagePins returns the sorted cache keys this Fastlet pins now and
// drops expired recent uses.
func (d *Driver) currentImagePins(now time.Time) (owner, stateRoot string, keys []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := make(map[string]struct{}, len(d.pinnedImages)+len(d.recentImages))
	for key := range d.pinnedImages {
		set[key] = struct{}{}
	}
	for key, used := range d.recentImages {
		if now.Sub(used) > imageRecentUseWindow {
			delete(d.recentImages, key)
			continue
		}
		set[key] = struct{}{}
	}
	keys = make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return d.imagePinOwner(), d.config.StateRoot, keys
}

// publishImagePins atomically rewrites this Fastlet's pin file. Without force
// (GC ticks) an unchanged file is only rewritten once it is
// imagePinRefreshAge old, and an empty pin set is never written. Nothing is
// written after Close. Failures are logged: pins only narrow what GC may
// evict, they never fail a create.
func (d *Driver) publishImagePins(force bool) {
	now := time.Now()
	owner, stateRoot, keys := d.currentImagePins(now)
	signature := strings.Join(keys, ",")
	d.mu.Lock()
	if d.pinsClosed || stateRoot == "" ||
		(!force && signature == d.pinSignature && (len(keys) == 0 || now.Sub(d.pinPublishedAt) < imagePinRefreshAge)) {
		d.mu.Unlock()
		return
	}
	d.pinSignature, d.pinPublishedAt = signature, now
	d.mu.Unlock()
	if err := writeImagePinFile(stateRoot, owner, imagePinFile{UpdatedAt: now.UTC(), Images: keys}); err != nil {
		klog.InfoS("publishing image pins failed", "owner", owner, "err", err)
	}
}

// removeImagePinsLocked deletes this Fastlet's pin file on shutdown and
// stops further publishing. The caller holds d.mu.
func (d *Driver) removeImagePinsLocked() {
	d.pinsClosed = true
	if d.config.StateRoot == "" {
		return
	}
	_ = os.Remove(imagePinPath(d.config.StateRoot, d.imagePinOwner()))
}

func imagePinPath(stateRoot, owner string) string {
	return filepath.Join(stateRoot, imagePinDir, owner+".json")
}

func writeImagePinFile(stateRoot, owner string, pins imagePinFile) error {
	if owner == "" || strings.ContainsAny(owner, `/\`) || owner == "." || owner == ".." {
		return fmt.Errorf("invalid pin owner %q", owner)
	}
	directory := filepath.Join(stateRoot, imagePinDir)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	payload, err := json.Marshal(pins)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+owner+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), imagePinPath(stateRoot, owner))
}

// loadNodeImagePins merges the pins of every Fastlet on the node whose file
// was refreshed within ttl. Unreadable or stale files are skipped: a pin file
// can only protect images, so ignoring a bad one never breaks a Sandbox.
func loadNodeImagePins(stateRoot string, now time.Time, ttl time.Duration) (map[string]struct{}, error) {
	pinned := make(map[string]struct{})
	entries, err := os.ReadDir(filepath.Join(stateRoot, imagePinDir))
	if errors.Is(err, os.ErrNotExist) {
		return pinned, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(stateRoot, imagePinDir, name))
		if err != nil {
			continue
		}
		var pins imagePinFile
		if err := json.Unmarshal(payload, &pins); err != nil || now.Sub(pins.UpdatedAt) > ttl {
			continue
		}
		for _, key := range pins.Images {
			if validImageDigest(key) {
				pinned[key] = struct{}{}
			}
		}
	}
	return pinned, nil
}
