package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const deviceA = "GPU-00000000-0000-0000-0000-000000000001"
const deviceB = "GPU-00000000-0000-0000-0000-000000000002"

func owner(uid string) fastletapi.SandboxIdentity {
	return fastletapi.SandboxIdentity{SandboxUID: uid, FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-" + uid, InstanceGeneration: 1, AssignmentAttempt: 1}
}

func TestConcurrentAcquireAndRecovery(t *testing.T) {
	root := t.TempDir()
	open := func() *Manager {
		m, err := New(root, []string{deviceA, deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, m.Close()) })
		return m
	}
	m := open()
	const attempts = 20
	var wg sync.WaitGroup
	type result struct {
		allocation *fastletapi.GPUAllocation
		err        error
	}
	var results [attempts]result
	for i := range results {
		wg.Go(func() {
			results[i].allocation, results[i].err = m.Acquire(owner(fmt.Sprint(i)))
		})
	}
	wg.Wait()
	acquired := map[string][]string{}
	for i, result := range results {
		if result.err == nil {
			require.Len(t, result.allocation.Devices, 1)
			acquired[fmt.Sprint(i)] = result.allocation.Devices
			continue
		}
		require.ErrorIs(t, result.err, ErrExhausted)
	}
	require.Len(t, acquired, 2)
	require.NoError(t, m.Close())

	recovered := open()
	require.False(t, recovered.Available())
	records, err := recovered.Records()
	require.NoError(t, err)
	require.Len(t, records, 2)
	for _, record := range records {
		allocation, err := recovered.Acquire(record.Owner)
		require.NoError(t, err)
		require.Equal(t, acquired[record.Owner.SandboxUID], allocation.Devices)
	}
	require.NotEqual(t, records[0].Devices, records[1].Devices)
	require.NoError(t, recovered.ReleaseVerified(records[0]))
	allocation, err := recovered.Acquire(owner("replacement"))
	require.NoError(t, err)
	require.Equal(t, records[0].Devices, allocation.Devices)
}

func TestLeaseFencesAndAllocationValidation(t *testing.T) {
	m, err := New(t.TempDir(), []string{deviceA, deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()
	first, err := m.Acquire(owner("a"))
	require.NoError(t, err)
	next := owner("a")
	next.RuntimeInstanceID = "replacement"
	_, err = m.Acquire(next)
	require.ErrorContains(t, err, "different runtime")
	require.Error(t, m.Recover(Lease{Owner: owner("b"), Devices: first.Devices, Namespace: "k8s.io", Endpoint: DefaultContainerdSocket}))
	require.Error(t, m.Recover(Lease{Owner: owner("b"), Devices: []string{"nvidia.com/gpu=all"}, Namespace: "k8s.io", Endpoint: DefaultContainerdSocket}))
	require.Error(t, m.Recover(Lease{Owner: next, Devices: first.Devices, Namespace: "k8s.io", Endpoint: DefaultContainerdSocket}))
	first.Devices[0] = "mutated" // The returned allocation must not mutate the ledger.
	lease, found, err := m.Lookup("a")
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, m.Recover(lease))
	_, err = m.Acquire(fastletapi.SandboxIdentity{SandboxUID: "incomplete"})
	require.Error(t, err)
}

func TestInvalidStateAndDeviceInventoryFailClosed(t *testing.T) {
	for _, ids := range [][]string{{"0"}, {"nvidia.com/gpu=all"}, {"MIG-abc"}, {deviceA, deviceA}} {
		_, err := New(t.TempDir(), ids, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
		require.Error(t, err)
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "leases.json"), []byte("broken"), 0600))
	_, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.Error(t, err)
	root = t.TempDir()
	m, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	_, err = m.Acquire(owner("live"))
	require.NoError(t, err)
	require.NoError(t, m.Close())
	_, err = New(root, []string{deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.ErrorContains(t, err, "unassigned device")
}

func TestFailedPersistenceDoesNotGrantADevice(t *testing.T) {
	root := t.TempDir()
	m, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()
	// A destination directory makes atomic rename fail without relying on uid permissions.
	require.NoError(t, os.Mkdir(filepath.Join(root, "leases.json"), 0700))
	_, err = m.Acquire(owner("a"))
	require.Error(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "leases.json")))
	records, err := m.Records()
	require.NoError(t, err)
	require.Empty(t, records)
	_, err = m.Acquire(owner("b"))
	require.NoError(t, err)
}

func TestNodeJournalPreservesOtherFastletsAndPreviousPodOwners(t *testing.T) {
	root := t.TempDir()
	a, err := New(root, []string{deviceA}, 1, "pod-a", "ns-a", DefaultContainerdSocket)
	require.NoError(t, err)
	b, err := New(root, []string{deviceB}, 1, "pod-b", "ns-b", DefaultContainerdSocket)
	require.NoError(t, err)
	defer b.Close()
	_, err = a.Acquire(owner("sandbox-a"))
	require.NoError(t, err)
	ownerB := owner("sandbox-b")
	ownerB.FastletPodUID = "pod-b"
	_, err = b.Acquire(ownerB)
	require.NoError(t, err)
	records, err := a.Records()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "sandbox-a", records[0].Owner.SandboxUID)
	// Even a different Pod identity cannot reuse a GPU held by a live process.
	_, err = New(root, []string{deviceA}, 1, "pod-c", "ns-c", DefaultContainerdSocket)
	require.Error(t, err)
	require.NoError(t, a.Close())
	c, err := New(root, []string{deviceA}, 1, "pod-c", "ns-c", DefaultContainerdSocket)
	require.NoError(t, err)
	defer c.Close()
	require.False(t, c.Available(), "the old process is gone but its task has not been proven absent")
	records, err = c.Records()
	require.NoError(t, err)
	require.Len(t, records, 1)
	altered := records[0]
	altered.Owner.RuntimeInstanceID = "wrong"
	require.Error(t, c.ReleaseVerified(altered))
	require.False(t, c.Available())
	require.NoError(t, c.ReleaseVerified(records[0]))
	require.True(t, c.Available())
	records, err = b.Records()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, ownerB, records[0].Owner, "reconciling an old GPU owner must retain unrelated Fastlet leases")
}
