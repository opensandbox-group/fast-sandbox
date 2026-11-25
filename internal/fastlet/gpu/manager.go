// Package gpu owns exclusive device leases inside one Kubernetes GPU allocation.
package gpu

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const (
	ResourceName            = "nvidia.com/gpu"
	DefaultContainerdSocket = "/run/containerd/containerd.sock"
)

var ErrExhausted = errors.New("no unallocated GPUs")
var uuidPattern = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Lease struct {
	Endpoint  string                     `json:"endpoint"`
	Namespace string                     `json:"namespace"`
	Owner     fastletapi.SandboxIdentity `json:"owner"`
	Devices   []string                   `json:"devices"`
}

// MatchesOwner compares the runtime identity that owns the devices. A route
// generation can change without replacing the runtime or its GPU lease.
func (l Lease) MatchesOwner(owner fastletapi.SandboxIdentity) bool {
	return l.Owner.SandboxUID == owner.SandboxUID &&
		l.Owner.FastletPodUID == owner.FastletPodUID &&
		l.Owner.RuntimeInstanceID == owner.RuntimeInstanceID &&
		l.Owner.InstanceGeneration == owner.InstanceGeneration &&
		l.Owner.AssignmentAttempt == owner.AssignmentAttempt
}

func (l Lease) matches(other Lease) bool {
	return l.MatchesOwner(other.Owner) && l.Namespace == other.Namespace &&
		l.Endpoint == other.Endpoint && slices.Equal(l.Devices, other.Devices)
}

type state struct {
	Version int              `json:"version"`
	Leases  map[string]Lease `json:"leases"`
}

// Manager persists reservations before runtime side effects. A failed cleanup
// retains its lease; only the runtime driver may release after proving absence.
type Manager struct {
	mu        sync.Mutex
	inventory []string
	count     int
	path      string
	state     state
	locks     []*os.File
	podUID    string
	namespace string
	endpoint  string
	// poisoned fails closed when disk durability is uncertain; recovered only by process restart.
	poisoned error
}

func New(root string, ids []string, perSandbox int, podUID, namespace, endpoint string) (*Manager, error) {
	if root == "" || podUID == "" || namespace == "" || endpoint == "" || perSandbox <= 0 || len(ids) < perSandbox {
		return nil, errors.New("GPU state root and sufficient assigned devices are required")
	}
	inventory := make([]string, 0, len(ids))
	for _, id := range ids {
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("GPU %q is not an exclusive full-device UUID; index, MIG and replicated devices are unsupported", id)
		}
		device := ResourceName + "=" + id
		if slices.Contains(inventory, device) {
			return nil, fmt.Errorf("duplicate GPU %q", id)
		}
		inventory = append(inventory, device)
	}
	slices.Sort(inventory)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	m := &Manager{
		inventory: inventory,
		count:     perSandbox,
		path:      filepath.Join(root, "leases.json"),
		podUID:    podUID,
		namespace: namespace,
		endpoint:  endpoint,
	}
	// Hold one node-wide lock per GPU for the lifetime of the Fastlet process.
	// Other Fastlets with disjoint Kubernetes assignments may run concurrently.
	for _, id := range ids {
		lock, err := lockGPU(root, id)
		// A stale-owner check can briefly hold the same device lock.
		for retry := 0; retry < 2 && errors.Is(err, unix.EWOULDBLOCK); retry++ {
			time.Sleep(100 * time.Millisecond)
			lock, err = lockGPU(root, id)
		}
		if err != nil {
			_ = m.Close()
			return nil, err
		}
		m.locks = append(m.locks, lock)
	}
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		_ = m.Close()
		return nil, err
	}
	unlock()
	return m, nil
}

func lockGPU(root, id string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(root, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("GPU %s still has an active Fastlet owner: %w", id, err)
	}
	return lock, nil
}

// lockAndReadJournal serializes node journal updates between independent Fastlets.
// Callers hold m.mu; the file lock stays held through the write.
func (m *Manager) lockAndReadJournal() (func(), error) {
	if m.poisoned != nil {
		return nil, m.poisoned
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(m.path), "leases.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	unlock := func() { _ = f.Close() }
	next := state{Version: 1, Leases: map[string]Lease{}}
	raw, err := os.ReadFile(m.path)
	if err == nil {
		err = json.Unmarshal(raw, &next)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil && (next.Version != 1 || next.Leases == nil) {
		err = errors.New("unsupported node GPU journal")
	}
	if err == nil {
		err = m.validate(next.Leases)
	}
	if err != nil {
		unlock()
		return nil, fmt.Errorf("read node GPU journal: %w", err)
	}
	m.state = next
	return unlock, nil
}

func (m *Manager) validate(leases map[string]Lease) error {
	ownersByDevice := map[string]string{}
	for uid, lease := range leases {
		owner := lease.Owner
		if uid == "" || uid != owner.SandboxUID || owner.FastletPodUID == "" || owner.RuntimeInstanceID == "" ||
			owner.InstanceGeneration <= 0 || owner.AssignmentAttempt <= 0 || lease.Namespace == "" || lease.Endpoint == "" {
			return errors.New("GPU lease has an incomplete owner")
		}
		ownedHere := owner.FastletPodUID == m.podUID
		if len(lease.Devices) == 0 || (ownedHere && len(lease.Devices) != m.count) {
			return fmt.Errorf("GPU lease %s has the wrong device count", uid)
		}
		if ownedHere && (lease.Namespace != m.namespace || lease.Endpoint != m.endpoint) {
			return fmt.Errorf("GPU lease %s belongs to a different containerd namespace or endpoint", uid)
		}
		for _, device := range lease.Devices {
			id, qualified := strings.CutPrefix(device, ResourceName+"=")
			if !qualified || !uuidPattern.MatchString(id) || (ownedHere && !slices.Contains(m.inventory, device)) {
				return fmt.Errorf("GPU lease %s references unassigned device %s", uid, device)
			}
			if previous, exists := ownersByDevice[device]; exists {
				return fmt.Errorf("GPU %s is leased twice (%s, %s)", device, previous, uid)
			}
			ownersByDevice[device] = uid
		}
	}
	return nil
}

func (m *Manager) Acquire(owner fastletapi.SandboxIdentity) (*fastletapi.GPUAllocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if owner.FastletPodUID != m.podUID {
		return nil, errors.New("GPU request is outside this Fastlet allocation")
	}
	if lease, exists := m.state.Leases[owner.SandboxUID]; exists {
		if !lease.MatchesOwner(owner) {
			return nil, errors.New("GPU lease belongs to a different runtime instance")
		}
		return &fastletapi.GPUAllocation{Devices: slices.Clone(lease.Devices)}, nil
	}
	used := map[string]bool{}
	for _, lease := range m.state.Leases {
		for _, device := range lease.Devices {
			used[device] = true
		}
	}
	devices := []string{}
	for _, device := range m.inventory {
		if !used[device] {
			devices = append(devices, device)
			if len(devices) == m.count {
				break
			}
		}
	}
	if len(devices) != m.count {
		return nil, ErrExhausted
	}
	next := maps.Clone(m.state.Leases)
	next[owner.SandboxUID] = Lease{Owner: owner, Devices: devices, Namespace: m.namespace, Endpoint: m.endpoint}
	if err := m.persistLeases(next); err != nil {
		return nil, err
	}
	return &fastletapi.GPUAllocation{Devices: slices.Clone(devices)}, nil
}

// Recover adopts only allocations verified against containerd inventory. Pending
// leases are retained until the driver has explicitly cleaned their runtime IDs.
func (m *Manager) Recover(lease Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		return err
	}
	defer unlock()
	if lease.Owner.FastletPodUID != m.podUID || lease.Namespace != m.namespace || lease.Endpoint != m.endpoint {
		return errors.New("cannot adopt another Fastlet runtime")
	}
	if previous, ok := m.state.Leases[lease.Owner.SandboxUID]; ok {
		if !previous.matches(lease) {
			return errors.New("runtime and durable GPU lease disagree")
		}
		return nil
	}
	next := maps.Clone(m.state.Leases)
	lease.Devices = slices.Clone(lease.Devices)
	next[lease.Owner.SandboxUID] = lease
	return m.persistLeases(next)
}

// Records returns leases relevant to this assignment, including abandoned prior owners.
func (m *Manager) Records() ([]Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		return nil, err
	}
	defer unlock()
	result := []Lease{}
	for _, lease := range m.state.Leases {
		relevant := lease.Owner.FastletPodUID == m.podUID
		for _, device := range lease.Devices {
			relevant = relevant || slices.Contains(m.inventory, device)
		}
		if relevant {
			lease.Devices = slices.Clone(lease.Devices)
			result = append(result, lease)
		}
	}
	return result, nil
}

// Lookup includes foreign owners on disjoint devices. The runtime must fence
// those owners before replacing a Sandbox with the same UID.
func (m *Manager) Lookup(uid string) (Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		return Lease{}, false, err
	}
	defer unlock()
	lease, exists := m.state.Leases[uid]
	lease.Devices = slices.Clone(lease.Devices)
	return lease, exists, nil
}

// FenceForeignOwner needs one device lock: the prior Fastlet must hold every
// device in its allocation before it can create runtimes. Requiring them all
// would block replacement Fastlets that now hold different parts of a lease.
func (m *Manager) FenceForeignOwner(lease Lease) (func(), error) {
	for _, device := range lease.Devices {
		if slices.Contains(m.inventory, device) {
			return func() {}, nil // Held for this manager's lifetime.
		}
	}
	var lockErr error
	for _, device := range lease.Devices {
		id := strings.TrimPrefix(device, ResourceName+"=")
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("invalid leased GPU UUID %q", id)
		}
		lock, err := lockGPU(filepath.Dir(m.path), id)
		if err == nil {
			return func() { _ = lock.Close() }, nil
		}
		lockErr = errors.Join(lockErr, err)
	}
	if lockErr == nil {
		return nil, errors.New("foreign GPU lease has no devices to fence")
	}
	return nil, lockErr
}

// ReleaseVerified deletes a lease only if its owner and devices still match.
// The caller must first prove that the old runtime can no longer use the devices.
func (m *Manager) ReleaseVerified(expected Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		return err
	}
	defer unlock()
	current, exists := m.state.Leases[expected.Owner.SandboxUID]
	if !exists {
		return nil
	}
	if !current.matches(expected) {
		return errors.New("GPU owner changed during absence verification")
	}
	next := maps.Clone(m.state.Leases)
	delete(next, expected.Owner.SandboxUID)
	return m.persistLeases(next)
}

func (m *Manager) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.lockAndReadJournal()
	if err != nil {
		return false
	}
	defer unlock()
	free := len(m.inventory)
	for _, lease := range m.state.Leases {
		for _, device := range lease.Devices {
			if slices.Contains(m.inventory, device) {
				free--
			}
		}
	}
	return free >= m.count
}

// persistLeases writes and syncs the new journal while the caller holds the file lock.
func (m *Manager) persistLeases(next map[string]Lease) error {
	if m.poisoned != nil {
		return m.poisoned
	}
	if err := m.validate(next); err != nil {
		return err
	}
	raw, err := json.Marshal(state{Version: 1, Leases: next})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".leases-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), m.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(m.path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		m.poisoned = fmt.Errorf("GPU lease durability is uncertain: %w", err)
		klog.ErrorS(m.poisoned, "GPU lease manager stopped accepting requests", "journal", m.path)
		return m.poisoned
	}
	m.state = state{Version: 1, Leases: next}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for _, lock := range m.locks {
		result = errors.Join(result, lock.Close())
	}
	m.locks = nil
	m.poisoned = errors.New("GPU manager is closed")
	return result
}

func (m *Manager) Devices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.inventory)
}
