package network

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
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
	return manager
}

func owner(id string, attempt int64) Owner {
	return Owner{SandboxUID: id, InstanceGeneration: 1, AssignmentAttempt: attempt}
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
