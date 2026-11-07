package guestcontrol

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeFS struct {
	started   chan struct{}
	complete  chan struct{}
	thawed    chan struct{}
	thawCount atomic.Int32
	freezeErr error
	thawErr   error
}

func (f *fakeFS) Freeze() error { close(f.started); <-f.complete; return f.freezeErr }
func (f *fakeFS) Thaw() error   { f.thawCount.Add(1); close(f.thawed); return f.thawErr }
func fixture(t *testing.T) (*Manager, *fakeFS) {
	t.Helper()
	f := &fakeFS{started: make(chan struct{}), complete: make(chan struct{}), thawed: make(chan struct{})}
	m, err := NewManager(f, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m, f
}
func waitStatus(t *testing.T, m *Manager, check func(Status) bool) Status {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s, _ := m.Status(1)
		if check(s) {
			return s
		}
		select {
		case <-deadline:
			t.Fatalf("unexpected status: %+v", s)
		case <-tick.C:
		}
	}
}
func TestCancelPendingFreezeCompensatesLateSuccess(t *testing.T) {
	m, f := fixture(t)
	if _, err := m.Freeze(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	<-f.started
	s, err := m.Thaw(1)
	if err != nil || s.State != Released || !s.CleanupPending {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err = m.Freeze(2, time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("successor admitted: %v", err)
	}
	if f.thawCount.Load() != 0 {
		t.Fatal("thaw raced pending freeze")
	}
	close(f.complete)
	waitStatus(t, m, func(s Status) bool { return s.State == Released && !s.CleanupPending })
	if f.thawCount.Load() != 1 {
		t.Fatal("missing compensation")
	}
	s, err = m.Freeze(1, time.Minute)
	if err != nil || s.State != Released {
		t.Fatalf("terminal replay: %+v %v", s, err)
	}
}
func TestThawBeforeFreezeFencesDelayedRequest(t *testing.T) {
	m, _ := fixture(t)
	m.Thaw(1)
	if _, err := m.Freeze(1, time.Minute); err == nil {
		t.Fatal("canceled token accepted")
	}
	m.Thaw(2)
	if _, err := m.Freeze(1, time.Minute); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
}
func TestLeaseExpiresDuringPendingFreeze(t *testing.T) {
	m, f := fixture(t)
	m.Freeze(1, 5*time.Millisecond)
	<-f.started
	waitStatus(t, m, func(s Status) bool { return s.State == Expired && s.CleanupPending })
	close(f.complete)
	waitStatus(t, m, func(s Status) bool { return s.State == Expired && !s.CleanupPending })
}
func TestHeldFreezeReplaysAndThaws(t *testing.T) {
	m, f := fixture(t)
	m.Freeze(1, time.Minute)
	close(f.complete)
	waitStatus(t, m, func(s Status) bool { return s.State == Held })
	if _, err := m.Freeze(1, time.Second); err == nil {
		t.Fatal("conflicting replay accepted")
	}
	m.Thaw(1)
	m.Thaw(1)
	waitStatus(t, m, func(s Status) bool { return !s.CleanupPending && s.State == Released })
	if f.thawCount.Load() != 1 {
		t.Fatal("duplicate thaw")
	}
}
func TestFailuresQuarantineFilesystem(t *testing.T) {
	for _, phase := range []string{"freeze", "thaw"} {
		t.Run(phase, func(t *testing.T) {
			m, f := fixture(t)
			fault := errors.New("injected failure")
			if phase == "freeze" {
				f.freezeErr = fault
			} else {
				f.thawErr = fault
			}
			m.Freeze(1, time.Minute)
			close(f.complete)
			if phase == "thaw" {
				waitStatus(t, m, func(s Status) bool { return s.State == Held })
				m.Thaw(1)
			}
			s := waitStatus(t, m, func(s Status) bool { return s.Failure != nil })
			if !s.CleanupPending {
				t.Fatal("uncertain filesystem not quarantined")
			}
			if _, err := m.Freeze(2, time.Minute); !errors.Is(err, ErrBusy) {
				t.Fatal("successor admitted")
			}
			if phase == "freeze" && f.thawCount.Load() != 0 {
				t.Fatal("thawed unowned freeze")
			}
		})
	}
}
