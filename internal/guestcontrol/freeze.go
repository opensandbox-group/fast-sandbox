// Package guestcontrol implements the Guest filesystem protection state machine.
// Before remote access is enabled, the control service must provide transport authentication, VM incarnation fencing, and restart reconciliation.
package guestcontrol

import (
	"errors"
	"sync"
	"time"
)

// Filesystem operations may block in the kernel and cannot be safely canceled.
// Implementations must not depend on writes to the protected filesystem.
type Filesystem interface {
	Freeze() error
	Thaw() error
}

type State string

const (
	Pending  State = "pending"
	Held     State = "held"
	Released State = "released"
	Expired  State = "expired"
)

var (
	ErrBusy    = errors.New("filesystem protection or cleanup is still active")
	ErrStale   = errors.New("operation generation has already terminated")
	ErrUnknown = errors.New("unknown operation generation")
)

type Status struct {
	Generation     uint64
	State          State
	CleanupPending bool
	Failure        error
}

// Manager controls one filesystem within an agent epoch. A fenced host coordinator must allocate generations.
// Only the latest state is retained; older generations are always rejected and cannot execute as new operations.
// After a service restart, reconcile any uncertain protection state before enabling a new Manager.
type Manager struct {
	mu       sync.Mutex
	fs       Filesystem
	maxLease time.Duration
	status   Status
	lease    time.Duration
	timer    *time.Timer
	deadline time.Time
	active   bool
}

func NewManager(fs Filesystem, maxLease time.Duration) (*Manager, error) {
	if fs == nil || maxLease <= 0 {
		return nil, errors.New("filesystem and positive maximum lease required")
	}
	return &Manager{fs: fs, maxLease: maxLease}, nil
}

// Freeze registers protection asynchronously so Status and Thaw remain responsive even when FIFREEZE blocks.
// Duplicate requests do not extend the lease.
func (m *Manager) Freeze(generation uint64, lease time.Duration) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation == 0 || lease <= 0 || lease > m.maxLease {
		return Status{}, errors.New("invalid generation or lease")
	}
	if generation < m.status.Generation {
		return Status{}, ErrStale
	}
	if generation == m.status.Generation {
		if lease != m.lease {
			return m.status, errors.New("conflicting operation replay")
		}
		return m.status, nil
	}
	if m.active {
		return m.status, ErrBusy
	}
	m.status = Status{Generation: generation, State: Pending}
	m.lease = lease
	m.deadline = time.Now().Add(lease)
	m.active = true
	m.timer = time.AfterFunc(lease, func() { m.expire(generation) })
	go m.freeze()
	return m.status, nil
}

func (m *Manager) Status(generation uint64) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.status.Generation || generation == 0 {
		return Status{}, ErrUnknown
	}
	return m.status, nil
}

// Thaw also cancels a pending Freeze or creates a terminal record before Freeze arrives.
// Released represents irreversible release intent; reuse is allowed only when CleanupPending becomes false.
func (m *Manager) Thaw(generation uint64) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation == 0 {
		return Status{}, ErrUnknown
	}
	if generation < m.status.Generation {
		return Status{}, ErrStale
	}
	if generation > m.status.Generation {
		if m.active {
			return m.status, ErrBusy
		}
		m.status = Status{Generation: generation, State: Released}
		m.lease = 0
		return m.status, nil
	}
	m.terminate(Released)
	return m.status, nil
}

func (m *Manager) expire(generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation == m.status.Generation {
		m.terminate(Expired)
	}
}

// The caller must hold mu. Pending workers compensate after their ioctl returns.
// Starting thaw concurrently would race with a freeze that succeeds late.
func (m *Manager) terminate(state State) {
	if m.status.State == Released || m.status.State == Expired {
		return
	}
	previous := m.status.State
	m.status.State = state
	m.status.CleanupPending = m.active
	if m.timer != nil {
		m.timer.Stop()
	}
	if previous == Held {
		go m.thaw()
	}
}

func (m *Manager) freeze() {
	err := m.fs.Freeze()
	m.mu.Lock()
	if err != nil {
		// A failed freeze does not authorize thawing a mount we may not own, such as EBUSY caused by an external freeze.
		// Keep the manager quarantined in this case.
		m.status.Failure = err
		if m.status.State == Pending {
			m.status.State = Released
		}
		m.status.CleanupPending = true
		m.timer.Stop()
		m.mu.Unlock()
		return
	}
	if m.status.State == Pending && !time.Now().Before(m.deadline) {
		m.terminate(Expired)
	}
	if m.status.State == Pending {
		m.status.State = Held
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.thaw()
}

func (m *Manager) thaw() {
	err := m.fs.Thaw()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Failure = err
	m.status.CleanupPending = err != nil
	// Quarantine this Manager after a thaw failure and reject subsequent operations.
	m.active = err != nil
}
