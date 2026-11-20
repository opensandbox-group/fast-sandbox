package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
)

const sharedSnapfilesDir = ".snapfiles"
const sharedSnapfilesMaxIdle = 16
const sharedSnapfilesIdleTTL = 10 * time.Minute

type sharedSnapfilesState struct {
	Device     *deviceRecord `json:"device,omitempty"`
	RootfsOnly bool          `json:"rootfs_only,omitempty"`
}

var rootfsOnlySnapfilesEntries = []string{inDiskManifest, "vault.json", "policy.json"}

func sharedSnapfilesRoot() string { return filepath.Join(global.stateDir, sharedSnapfilesDir) }

// The resolved layers and credential scope are both part of the identity. Tags
// can change; credentials must never borrow another access scope's prepared disk.
func sharedSnapfilesKey(reference string, image resolvedImage, auth dockerConfig) (string, error) {
	ref, err := parseRegistryReference(reference)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(image)
	if err != nil {
		return "", err
	}
	ref.Reference = digest.FromBytes(raw).String()
	scoped, err := createManifestCachePath(ref, auth)
	if err != nil {
		return "", err
	}
	return digest.FromString(scoped).Encoded(), nil
}

func lockSharedSnapfiles(ctx context.Context, key string) (func(), error) {
	return lockSharedSnapfilesMode(ctx, key, tryLock)
}

func lockSharedSnapfilesRead(ctx context.Context, key string) (func(), error) {
	return lockSharedSnapfilesMode(ctx, key, trySharedLock)
}

func lockSharedSnapfilesMode(ctx context.Context, key string, acquire func(string, string) (func(), error)) (func(), error) {
	for {
		unlock, err := acquire(sharedSnapfilesRoot(), key)
		if !errors.Is(err, errBusy) {
			return unlock, err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func readSharedSnapfiles(home string) (sharedSnapfilesState, error) {
	var state sharedSnapfilesState
	raw, err := os.ReadFile(statePath(home))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}

func hasMaterializedRootfsOnly(home string, cached sharedSnapfilesState) bool {
	if !cached.RootfsOnly || cached.Device != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(home, snapfilesMount, inDiskManifest))
	return err == nil && info.Mode().IsRegular()
}

func installSharedSnapfilesLease(sandboxHome, key, home string, state *sandboxState) error {
	state.SharedSnapfiles, state.MountPoint = key, filepath.Join(home, snapfilesMount)
	if err := writeStateAtomic(sandboxHome, state); err != nil {
		return err
	}
	return os.Symlink(state.MountPoint, filepath.Join(sandboxHome, snapfilesMount))
}

// tryAcquireMaterializedRootfsOnly serves warm RootfsOnly snapfiles under a
// shared lock without calling ublkd.List, mounting ext4, or serializing
// concurrent sandbox creates on the same snapshot.
func tryAcquireMaterializedRootfsOnly(ctx context.Context, sandboxHome, key, home string, state *sandboxState) (bool, error) {
	unlock, err := lockSharedSnapfilesRead(ctx, key)
	if err != nil {
		return false, err
	}
	defer unlock()
	cached, err := readSharedSnapfiles(home)
	if err != nil || !hasMaterializedRootfsOnly(home, cached) {
		return false, nil
	}
	if err := installSharedSnapfilesLease(sandboxHome, key, home, state); err != nil {
		return false, err
	}
	now := time.Now()
	return true, os.Chtimes(statePath(home), now, now)
}

// materializeRootfsOnlySnapfiles copies the small metadata/state files out of a
// freshly mounted RootfsOnly snapfiles ext4 image into a plain host directory
// and releases the ublk device immediately so subsequent creates need no ublk
// device, ext4 mount, or ublkd.List probe. Errors before unmount leave the live
// mount intact; errors after unmount are returned so create rolls back cleanly.
func materializeRootfsOnlySnapfiles(ctx context.Context, client *ublkdClient, home string, state *sandboxState, cached *sharedSnapfilesState, persist func() error) error {
	if cached.Device == nil {
		return nil
	}
	manifest, err := loadManifest(filepath.Join(state.MountPoint, inDiskManifest))
	if err != nil || !manifest.IsRootfsOnly() {
		return nil
	}
	referenced, err := sharedSnapfilesReferenced(state.SharedSnapfiles, state.SandboxID)
	if err != nil || referenced {
		return nil
	}
	staged := filepath.Join(home, ".rootfs-only-tmp")
	_ = os.RemoveAll(staged)
	if err := os.MkdirAll(staged, 0o750); err != nil {
		return nil
	}
	cleanupStaged := true
	defer func() {
		if cleanupStaged {
			_ = os.RemoveAll(staged)
		}
	}()
	for _, name := range rootfsOnlySnapfilesEntries {
		source := filepath.Join(state.MountPoint, name)
		info, statErr := os.Stat(source)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) && name != inDiskManifest {
				continue
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := copyFile(source, filepath.Join(staged, name), 0o600); err != nil {
			return nil
		}
	}
	configPath := cached.Device.Config
	if err := unmount(state.MountPoint); err != nil {
		return nil
	}
	if _, err := releaseOwnedDevicesByConfig(ctx, client, configPath); err != nil {
		return fmt.Errorf("release materialized rootfsOnly snapfiles device: %w", err)
	}
	_ = os.Remove(configPath)
	_ = os.RemoveAll(filepath.Join(home, ".local-blobs"))
	cached.Device = nil
	if err := os.Remove(state.MountPoint); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = persist()
		return err
	}
	if err := os.Rename(staged, state.MountPoint); err != nil {
		_ = persist()
		return err
	}
	cleanupStaged = false
	cached.RootfsOnly = true
	return persist()
}

func acquireSharedSnapfiles(ctx context.Context, client *ublkdClient, sandboxHome string, state *sandboxState, image resolvedImage, auth dockerConfig) error {
	key, err := sharedSnapfilesKey(state.SnapfilesRef, image, auth)
	if err != nil {
		return err
	}
	home := filepath.Join(sharedSnapfilesRoot(), key)
	if reused, err := tryAcquireMaterializedRootfsOnly(ctx, sandboxHome, key, home, state); err != nil || reused {
		return err
	}
	unlock, err := lockSharedSnapfiles(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	// Install the durable lease while holding the same lock eviction takes.
	if err := installSharedSnapfilesLease(sandboxHome, key, home, state); err != nil {
		return err
	}
	cached, err := readSharedSnapfiles(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hasMaterializedRootfsOnly(home, cached) {
		now := time.Now()
		return os.Chtimes(statePath(home), now, now)
	}
	persist := func() error {
		raw, err := json.Marshal(cached)
		if err != nil {
			return err
		}
		return writeFileAtomic(statePath(home), raw, 0600)
	}
	if cached.Device != nil {
		if err := recoverSharedSnapfilesDevice(ctx, client, state, &cached, persist); err != nil {
			return err
		}
	}
	freshAttach := false
	if cached.Device == nil {
		// Persist cleanup ownership before local lower hard links are created.
		if err := persist(); err != nil {
			return err
		}
		config, err := writeSnapfilesConfig(home, image)
		if err != nil {
			return err
		}
		if _, err := addSandboxDevice(ctx, client, config, &cached.Device, persist); err != nil {
			return err
		}
		freshAttach = true
	}
	if !isMounted(state.MountPoint) {
		if err := mountReadOnly(cached.Device.DevPath, state.MountPoint); err != nil {
			return err
		}
	}
	if freshAttach {
		if err := materializeRootfsOnlySnapfiles(ctx, client, home, state, &cached, persist); err != nil {
			return err
		}
	}
	now := time.Now()
	return os.Chtimes(statePath(home), now, now)
}

// A pending Add can finish after its caller exits. Fence it before replacing the
// config, then release any device it produced before starting a fresh Add.
func recoverSharedSnapfilesDevice(ctx context.Context, client *ublkdClient, state *sandboxState, cached *sharedSnapfilesState, persist func() error) error {
	devices, err := client.List(ctx)
	if err != nil {
		return err
	}
	for _, device := range devices {
		if device.Config != cached.Device.Config || (cached.Device.DevPath != "" && device.Dev != cached.Device.DevPath) || device.Writable {
			continue
		}
		if cached.Device.DevPath == "" {
			cached.Device.DevID, cached.Device.DevPath = device.DevID, device.Dev
			return persist()
		}
		return nil
	}
	referenced, err := sharedSnapfilesReferenced(state.SharedSnapfiles, state.SandboxID)
	if err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("shared snapfiles device missing: %s; other Sandbox leases remain; retained for cleanup", cached.Device.Config)
	}
	if err := unmount(state.MountPoint); err != nil {
		return err
	}
	if err := client.FenceAdd(ctx, cached.Device.Config); err != nil {
		return err
	}
	if _, err := releaseOwnedDevicesByConfig(ctx, client, cached.Device.Config); err != nil {
		return err
	}
	cached.Device = nil
	return persist()
}

// The alias also identifies shared ownership if a Sandbox state is missing or
// unreadable. Never pass a shared alias to umount during a teardown retry.
func sharedSnapfilesAlias(home string) (string, error) {
	path := filepath.Join(home, snapfilesMount)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	key := filepath.Base(filepath.Dir(target))
	if len(key) != 64 || target != filepath.Join(sharedSnapfilesRoot(), key, snapfilesMount) {
		return "", fmt.Errorf("invalid shared snapfiles alias: %s", path)
	}
	return key, nil
}

// Remove the alias before releasing the lease so an interrupted delete cannot
// later follow it and unmount a filesystem held by another Sandbox.
func releaseSharedSnapfilesLease(home string, state *sandboxState) error {
	if state.SharedSnapfiles == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(home, snapfilesMount)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Failed Adds must not extend retention on each retry.
	homePath := filepath.Join(sharedSnapfilesRoot(), state.SharedSnapfiles)
	if cached, err := readSharedSnapfiles(homePath); err == nil && ((cached.Device != nil && cached.Device.DevPath != "") || cached.RootfsOnly) {
		now := time.Now()
		_ = os.Chtimes(statePath(homePath), now, now)
	}
	state.SharedSnapfiles = ""
	return nil
}

// Sandbox states are the leases. No process-local count can disappear on a CLI
// exit, and an unreadable state prevents eviction rather than risking a live VM.
func sharedSnapfilesReferenced(key, excludeSandboxID string) (bool, error) {
	ids, err := listSandboxIDs(global.stateDir)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == excludeSandboxID {
			continue
		}
		state, err := readState(global.sandboxHome(id))
		if errors.Is(err, os.ErrNotExist) {
			alias, aliasErr := sharedSnapfilesAlias(global.sandboxHome(id))
			if aliasErr != nil {
				return false, aliasErr
			}
			if alias == key {
				return true, nil
			}
			continue
		}
		if err != nil {
			return false, err
		}
		if state.SharedSnapfiles == key {
			return true, nil
		}
	}
	return false, nil
}

// Normal deletion retains a bounded set of warm disks; orphan cleanup forces
// eviction, but only after every Sandbox lease has been released.
func pruneSharedSnapfiles(ctx context.Context, client *ublkdClient, all bool) error {
	entries, err := os.ReadDir(sharedSnapfilesRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		key  string
		used time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 64 {
			continue
		}
		info, err := os.Stat(statePath(filepath.Join(sharedSnapfilesRoot(), entry.Name())))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{entry.Name(), info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used.After(candidates[j].used) })
	idle := 0
	for _, candidate := range candidates {
		isIdle, err := pruneSharedSnapfilesEntry(ctx, client, candidate.key, all, idle)
		if err != nil {
			return err
		}
		if isIdle {
			idle++
		}
	}
	return nil
}

func pruneSharedSnapfilesEntry(ctx context.Context, client *ublkdClient, key string, all bool, idleCount int) (bool, error) {
	unlock, err := tryLock(sharedSnapfilesRoot(), key)
	if errors.Is(err, errBusy) {
		if all {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unlock()
	referenced, err := sharedSnapfilesReferenced(key, "")
	if err != nil {
		return false, err
	}
	if referenced {
		if all {
			return false, fmt.Errorf("shared snapfiles %s still has Sandbox leases", key)
		}
		return false, nil
	}
	home := filepath.Join(sharedSnapfilesRoot(), key)
	// Recheck recency under the lock: a concurrent create may have just used it.
	info, err := os.Stat(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !all && idleCount < sharedSnapfilesMaxIdle && time.Since(info.ModTime()) < sharedSnapfilesIdleTTL {
		return true, nil
	}
	cached, err := readSharedSnapfiles(home)
	if err != nil {
		return false, err
	}
	if err := unmount(filepath.Join(home, snapfilesMount)); err != nil {
		return false, err
	}
	if cached.Device != nil {
		if cached.Device.DevPath == "" {
			if err := client.FenceAdd(ctx, cached.Device.Config); err != nil {
				return false, err
			}
		}
		if _, err := releaseOwnedDevicesByConfig(ctx, client, cached.Device.Config); err != nil {
			return false, err
		}
	}
	return true, os.RemoveAll(home)
}

func newPruneSnapfilesCommand() *cobra.Command {
	return &cobra.Command{Use: "cleanup-snapfiles", Short: "Release shared snapfiles with no Sandbox leases", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return pruneSharedSnapfiles(cmd.Context(), newUblkdClient(global.ublkdSocket), true)
		}}
}
