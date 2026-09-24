package network

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

type fakeDriver struct {
	mu          sync.Mutex
	prepared    []string
	destroyed   []string
	destroyErr  error
	invalidSlot map[string]error
}

func (d *fakeDriver) Prepare(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prepared = append(d.prepared, slot.ID)
	return nil
}

func (d *fakeDriver) Validate(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.invalidSlot[slot.ID]
}

func (d *fakeDriver) Destroy(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyErr != nil {
		return d.destroyErr
	}
	d.destroyed = append(d.destroyed, slot.ID)
	return nil
}

func newTestManager(t *testing.T, capacity int, root string, driver Driver, ids ...string) *Manager {
	t.Helper()
	index := 0
	config := DefaultConfig(capacity, "pod-uid-1")
	config.StateRoot = root
	config.NetNSRoot = filepath.Join(root, "netns")
	config.HostNetNSRoot = filepath.Join(root, "host-netns")
	config.IDGenerator = func() (string, error) {
		if index < len(ids) {
			id := ids[index]
			index++
			return id, nil
		}
		id := fmt.Sprintf("slot-%d", index+1)
		index++
		return id, nil
	}
	manager, err := NewManager(config, driver, NewFileStateStore(filepath.Join(root, config.PodUID)))
	require.NoError(t, err)
	// Quiesce before TempDir cleanup: a Release inside the test spawns a
	// background replenish that keeps writing state files after the body
	// returns, racing the RemoveAll with "directory not empty".
	t.Cleanup(manager.Quiesce)
	return manager
}

func owner(id string, attempt int64) Owner {
	return Owner{SandboxUID: id, InstanceGeneration: 1, AssignmentAttempt: attempt}
}

func TestOwnerEqualIgnoresCleanupHint(t *testing.T) {
	legacy := owner("sandbox-a", 1)
	current := legacy
	current.ResidualProcess = runtimecatalog.ResidualProcessFirecracker

	require.True(t, legacy.Equal(current))
	require.True(t, current.Equal(legacy))
}

// guestDataPlaneDriver records ApplyGuest calls. Its ApplyGuest takes the
// manager read lock while "working": if the manager held its write lock
// during the guest data plane, this would deadlock.
type guestDataPlaneDriver struct {
	fakeDriver
	manager *Manager
	applied []string
}

func (d *guestDataPlaneDriver) ApplyGuest(_ context.Context, slot *Slot, guestIP string) error {
	if d.manager != nil {
		_ = d.manager.Snapshot()
	}
	slot.GuestIP = guestIP
	d.applied = append(d.applied, slot.ID)
	return nil
}

func TestManagerApplyGuestRecordsAndPersists(t *testing.T) {
	root := t.TempDir()
	driver := &guestDataPlaneDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	driver.manager = manager
	require.NoError(t, manager.Initialize(context.Background()))

	owner := owner("sandbox-1", 1)
	_, err := manager.Acquire(context.Background(), owner)
	require.NoError(t, err)
	require.NoError(t, manager.ApplyGuest(context.Background(), owner, "10.17.0.9"))
	require.Equal(t, []string{"slot-a"}, driver.applied)

	slot, exists := manager.Lookup("sandbox-1")
	require.True(t, exists)
	require.Equal(t, "10.17.0.9", slot.GuestIP)

	// The applied address is persisted: a reloaded manager sees it.
	reloaded, err := NewManager(DefaultConfig(1, "pod-uid-1"), &guestDataPlaneDriver{}, NewFileStateStore(filepath.Join(root, "pod-uid-1")))
	require.NoError(t, err)
	require.NoError(t, reloaded.Initialize(context.Background()))
	slot, exists = reloaded.Lookup("sandbox-1")
	require.True(t, exists)
	require.Equal(t, "10.17.0.9", slot.GuestIP)
}

func TestManagerApplyGuestDoesNotHoldLockDuringExec(t *testing.T) {
	// The guest data plane fake takes the manager read lock inside its
	// ApplyGuest; with the write lock held this deadlocks (and the timeout
	// fails the test).
	driver := &guestDataPlaneDriver{}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a")
	driver.manager = manager
	require.NoError(t, manager.Initialize(context.Background()))

	owner := owner("sandbox-1", 1)
	_, err := manager.Acquire(context.Background(), owner)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- manager.ApplyGuest(context.Background(), owner, "10.17.0.9") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyGuest held the manager write lock while executing the guest data plane")
	}
}

func TestManagerApplyGuestRequiresBoundSlot(t *testing.T) {
	root := t.TempDir()
	driver := &guestDataPlaneDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	driver.manager = manager
	require.NoError(t, manager.Initialize(context.Background()))

	// No bound slot for the owner.
	require.ErrorIs(t, manager.ApplyGuest(context.Background(), owner("nobody", 1), "10.17.0.9"), ErrSlotNotFound)

	// A released slot is no longer applicable.
	bound := owner("sandbox-1", 1)
	_, err := manager.Acquire(context.Background(), bound)
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), bound))
	require.ErrorIs(t, manager.ApplyGuest(context.Background(), bound, "10.17.0.9"), ErrSlotNotFound)
	require.Empty(t, driver.applied)
}

func TestManagerCloseDestroysRemainingSlots(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{}
	manager := newTestManager(t, 2, root, driver, "slot-a", "slot-b")
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, 2, manager.Snapshot().Clean)

	// A bound slot is destroyed too.
	_, err := manager.Acquire(context.Background(), owner("sandbox-1", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Close(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
	require.Equal(t, 0, manager.Snapshot().Bound)
	require.ElementsMatch(t, []string{"slot-a", "slot-b"}, driver.destroyed)

	// Idempotent.
	require.NoError(t, manager.Close(context.Background()))

	// No further slot preparation happens after Close: a Release (which
	// would normally replenish) must not recreate resources.
	require.NoError(t, manager.Replenish(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
}

func TestManagerCloseRacingPreparationDiscardsSlot(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, []string{"slot-a"}, driver.prepared)

	// Close destroys the prepared slot and is idempotent.
	require.NoError(t, manager.Close(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
	require.Equal(t, []string{"slot-a"}, driver.destroyed)

	// Replenish after Close must not prepare any further slot.
	require.NoError(t, manager.Replenish(context.Background()))
	require.Equal(t, []string{"slot-a"}, driver.prepared)
	require.Equal(t, 0, manager.Snapshot().Clean)
}

func TestManagerAcquireReleaseDestroysUsedSlot(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{}
	manager := newTestManager(t, 2, root, driver, "slot-a", "slot-b", "slot-c")
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, 2, manager.Snapshot().Clean)

	first, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	second, err := manager.Acquire(context.Background(), owner("sandbox-b", 1))
	require.NoError(t, err)
	require.NotEqual(t, first.IP, second.IP)
	_, err = manager.Acquire(context.Background(), owner("sandbox-c", 1))
	require.ErrorIs(t, err, ErrNoCleanSlot)
	require.Equal(t, uint64(2), manager.Snapshot().Hit)
	require.Equal(t, uint64(1), manager.Snapshot().Miss)

	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))
	require.NoError(t, manager.Replenish(context.Background()))
	require.Eventually(t, func() bool { return manager.Snapshot().Clean == 1 }, testEventuallyTimeout, testEventuallyInterval)
	replacement, ok := manager.Lookup("sandbox-a")
	require.False(t, ok)
	require.Nil(t, replacement)
	driver.mu.Lock()
	require.Contains(t, driver.destroyed, first.ID)
	require.NotContains(t, driver.prepared[2:], first.ID)
	driver.mu.Unlock()
}

func TestManagerAcquireIsIdentityIdempotentAndFenced(t *testing.T) {
	manager := newTestManager(t, 1, t.TempDir(), &fakeDriver{}, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	first, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	second, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	_, err = manager.Acquire(context.Background(), owner("sandbox-a", 2))
	require.ErrorIs(t, err, ErrOwnerConflict)
}

func TestManagerConcurrentAcquireNeverOvercommits(t *testing.T) {
	const capacity = 8
	manager := newTestManager(t, capacity, t.TempDir(), &fakeDriver{})
	require.NoError(t, manager.Initialize(context.Background()))

	var group sync.WaitGroup
	results := make(chan *Slot, capacity*2)
	for index := 0; index < capacity*2; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			slot, err := manager.Acquire(context.Background(), owner(fmt.Sprintf("sandbox-%d", index), 1))
			if err == nil {
				results <- slot
			}
		}()
	}
	group.Wait()
	close(results)
	ids := map[string]struct{}{}
	for slot := range results {
		ids[slot.ID] = struct{}{}
	}
	require.Len(t, ids, capacity)
	require.Equal(t, capacity, manager.Snapshot().Bound)
}

func TestManagerRecoversStateAndDestroysRuntimeOrphan(t *testing.T) {
	root := t.TempDir()
	firstDriver := &fakeDriver{}
	first := newTestManager(t, 2, root, firstDriver, "slot-a", "slot-b")
	require.NoError(t, first.Initialize(context.Background()))
	bound, err := first.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)

	secondDriver := &fakeDriver{}
	second := newTestManager(t, 2, root, secondDriver, "slot-c")
	require.NoError(t, second.Initialize(context.Background()))
	recovered, ok := second.Lookup("sandbox-a")
	require.True(t, ok)
	require.Equal(t, bound.ID, recovered.ID)
	require.NoError(t, second.Reconcile(context.Background(), nil))
	require.Eventually(t, func() bool { return second.Snapshot().Clean == 2 }, testEventuallyTimeout, testEventuallyInterval)
	secondDriver.mu.Lock()
	require.Contains(t, secondDriver.destroyed, bound.ID)
	secondDriver.mu.Unlock()
}

func TestManagerRetriesInterruptedDestroyOnInitialize(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{destroyErr: errors.New("busy")}
	first := newTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, first.Initialize(context.Background()))
	require.NoError(t, func() error {
		_, err := first.Acquire(context.Background(), owner("sandbox-a", 1))
		return err
	}())
	require.Error(t, first.Release(context.Background(), owner("sandbox-a", 1)))
	require.Equal(t, 1, first.Snapshot().Destroying)

	driver.destroyErr = nil
	second := newTestManager(t, 1, root, driver, "slot-b")
	require.NoError(t, second.Initialize(context.Background()))
	require.Equal(t, 1, second.Snapshot().Clean)
}

const (
	testEventuallyTimeout  = 2e9
	testEventuallyInterval = 10e6
)
