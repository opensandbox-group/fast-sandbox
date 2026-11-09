package firecracker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/guestcontrol"
)

type captureFixture struct {
	calls        []string
	fail         string
	cancel       context.CancelFunc
	recovered    bool
	boundary     rootfsCaptureBoundary
	mutateFreeze func(*guestcontrol.Response)
	mutateThaw   func(*guestcontrol.Response)
	afterVerify  func()
}

func (f *captureFixture) step(s string) error {
	f.calls = append(f.calls, s)
	if f.fail == s {
		return errors.New(s)
	}
	return nil
}
func (f *captureFixture) Verify(context.Context) (rootfsCaptureBoundary, error) {
	err := f.step("verify")
	if f.afterVerify != nil {
		f.afterVerify()
	}
	return f.boundary, err
}
func (f *captureFixture) Release() error              { return f.step("release") }
func (f *captureFixture) Quarantine(error) error      { return f.step("quarantine") }
func (f *captureFixture) Pause(context.Context) error { return f.step("pause") }
func (f *captureFixture) Resume(ctx context.Context) error {
	f.recovered = ctx.Err() == nil
	return f.step("resume")
}
func (f *captureFixture) FreezeAndWait(_ context.Context, request guestcontrol.Request) (guestcontrol.Response, error) {
	r := captureResponse(request, guestcontrol.Held)
	if f.mutateFreeze != nil {
		f.mutateFreeze(&r)
	}
	return r, f.step("freeze")
}
func (f *captureFixture) ThawAndWait(ctx context.Context, request guestcontrol.Request) (guestcontrol.Response, error) {
	if ctx.Err() != nil {
		return guestcontrol.Response{}, ctx.Err()
	}
	r := captureResponse(request, guestcontrol.Released)
	if f.mutateThaw != nil {
		f.mutateThaw(&r)
	}
	return r, f.step("thaw")
}
func (f *captureFixture) transaction() rootfsCapture {
	f.boundary = rootfsCaptureBoundary{IOEngine: "Sync", Filesystem: "ext4", FilesystemExclusive: true}
	return rootfsCapture{
		Lease: f, VM: f, Guest: f, Request: guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: "epoch", Generation: 1, OperationID: "capture", LeaseMillis: 60000}, RecoveryTimeout: time.Second,
		Flush: func() error {
			if f.cancel != nil {
				f.cancel()
			}
			return f.step("flush")
		}, Cut: func() error { return f.step("cut") },
	}
}

func TestRootfsCaptureRecovery(t *testing.T) {
	for _, tc := range []struct {
		fail string
		want []string
	}{
		{"", []string{"verify", "freeze", "pause", "flush", "cut", "resume", "thaw", "release"}},
		{"verify", []string{"verify", "release"}},
		{"freeze", []string{"verify", "freeze", "thaw", "release"}},
		{"pause", []string{"verify", "freeze", "pause", "resume", "thaw", "quarantine"}},
		{"flush", []string{"verify", "freeze", "pause", "flush", "resume", "thaw", "release"}},
		{"cut", []string{"verify", "freeze", "pause", "flush", "cut", "resume", "thaw", "release"}},
		{"resume", []string{"verify", "freeze", "pause", "flush", "cut", "resume", "quarantine"}},
		{"thaw", []string{"verify", "freeze", "pause", "flush", "cut", "resume", "thaw", "quarantine"}},
		{"release", []string{"verify", "freeze", "pause", "flush", "cut", "resume", "thaw", "release", "quarantine"}},
	} {
		t.Run("failure_"+tc.fail, func(t *testing.T) {
			f := &captureFixture{fail: tc.fail}
			err := f.transaction().run(t.Context())
			if tc.fail == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.fail)
			}
			require.Equal(t, tc.want, f.calls)
		})
	}
}
func TestRootfsCaptureCanceledFlushWaitsBeforeRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &captureFixture{}
	tx := f.transaction()
	entered, finish := make(chan struct{}), make(chan struct{})
	tx.Flush = func() error { close(entered); <-finish; return f.step("flush") }
	done := make(chan error, 1)
	go func() { done <- tx.run(ctx) }()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("returned while flush still running")
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	require.ErrorIs(t, <-done, context.Canceled)
	require.True(t, f.recovered)
	require.Equal(t, []string{"verify", "freeze", "pause", "flush", "resume", "thaw", "release"}, f.calls)
}

func captureResponse(r guestcontrol.Request, state guestcontrol.State) guestcontrol.Response {
	return guestcontrol.Response{Protocol: guestcontrol.Protocol, Epoch: r.Epoch, Generation: r.Generation, MaxLeaseMillis: r.LeaseMillis, State: state}
}

func TestRootfsCaptureRequiresQualifiedBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, engine, fs string
		exclusive, ok    bool
	}{
		{"sync", "Sync", "ext4", true, true}, {"async", "Async", "ext4", true, true},
		{"unknown", "", "ext4", true, false}, {"unsupported", "Other", "ext4", true, false},
		{"raw-writer", "Async", "ext4", false, false}, {"unqualified-fs", "Async", "xfs", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &captureFixture{}
			tx := f.transaction()
			f.boundary = rootfsCaptureBoundary{tc.engine, tc.fs, tc.exclusive}
			err := tx.run(t.Context())
			if tc.ok {
				require.NoError(t, err)
				require.Contains(t, f.calls, "cut")
			} else {
				require.Error(t, err)
				require.Equal(t, []string{"verify", "release"}, f.calls)
			}
		})
	}
}

func TestRootfsCaptureRejectsUnconfirmedGuestState(t *testing.T) {
	cases := map[string]func(*guestcontrol.Response){
		"epoch":      func(r *guestcontrol.Response) { r.Epoch = "replacement" },
		"generation": func(r *guestcontrol.Response) { r.Generation++ },
		"protocol":   func(r *guestcontrol.Response) { r.Protocol = "unknown" },
		"cleanup":    func(r *guestcontrol.Response) { r.CleanupPending = true },
		"failure":    func(r *guestcontrol.Response) { r.Failure = "EIO" },
		"error":      func(r *guestcontrol.Response) { r.Error = "rejected" },
		"pending":    func(r *guestcontrol.Response) { r.State = guestcontrol.Pending },
	}
	for name, mutate := range cases {
		t.Run("freeze/"+name, func(t *testing.T) {
			f := &captureFixture{mutateFreeze: mutate}
			err := f.transaction().run(t.Context())
			require.Error(t, err)
			if name == "epoch" || name == "generation" || name == "protocol" {
				require.Equal(t, []string{"verify", "freeze", "thaw", "quarantine"}, f.calls)
			} else {
				require.Equal(t, []string{"verify", "freeze", "thaw", "release"}, f.calls)
			}
		})
		t.Run("thaw/"+name, func(t *testing.T) {
			f := &captureFixture{mutateThaw: mutate}
			err := f.transaction().run(t.Context())
			require.Error(t, err)
			require.Equal(t, "quarantine", f.calls[len(f.calls)-1])
			require.NotContains(t, f.calls, "release")
		})
	}
	for _, state := range []guestcontrol.State{guestcontrol.Expired, guestcontrol.Released} {
		t.Run("freeze/"+string(state), func(t *testing.T) {
			f := &captureFixture{mutateFreeze: func(r *guestcontrol.Response) { r.State = state }}
			require.Error(t, f.transaction().run(t.Context()))
			require.NotContains(t, f.calls, "pause")
		})
	}
	t.Run("short-lease", func(t *testing.T) {
		f := &captureFixture{mutateFreeze: func(r *guestcontrol.Response) { r.MaxLeaseMillis = 1 }}
		require.Error(t, f.transaction().run(t.Context()))
		require.NotContains(t, f.calls, "pause")
	})
	t.Run("expired-cleanup-is-terminal", func(t *testing.T) {
		f := &captureFixture{mutateThaw: func(r *guestcontrol.Response) { r.State = guestcontrol.Expired }}
		require.NoError(t, f.transaction().run(t.Context()))
	})
}

func TestRootfsCaptureCanceledCutWaitsBeforeRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &captureFixture{}
	tx := f.transaction()
	entered, finish := make(chan struct{}), make(chan struct{})
	tx.Cut = func() error { close(entered); <-finish; return f.step("cut") }
	done := make(chan error, 1)
	go func() { done <- tx.run(ctx) }()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("returned while cut still mutates staging")
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, []string{"verify", "freeze", "pause", "flush", "cut", "resume", "thaw", "release"}, f.calls)
	require.True(t, f.recovered)
}

func TestRootfsCaptureFreezeDeadlineRejectsLateAcquisition(t *testing.T) {
	f := &captureFixture{mutateFreeze: func(*guestcontrol.Response) { time.Sleep(10 * time.Millisecond) }}
	tx := f.transaction()
	tx.Request.LeaseMillis = 1
	require.ErrorContains(t, tx.run(t.Context()), "expired before pause")
	require.Equal(t, []string{"verify", "freeze", "thaw", "release"}, f.calls)
}

func TestRootfsCapturePanicRecoversSourceButRetainsFence(t *testing.T) {
	f := &captureFixture{}
	tx := f.transaction()
	tx.Cut = func() error { f.step("cut"); panic("unexpected copy failure") }
	require.PanicsWithValue(t, "unexpected copy failure", func() { _ = tx.run(t.Context()) })
	require.Equal(t, []string{"verify", "freeze", "pause", "flush", "cut", "resume", "thaw", "quarantine"}, f.calls)
}

func TestRootfsCaptureCanceledQualificationDoesNotFreeze(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &captureFixture{afterVerify: cancel}
	require.ErrorIs(t, f.transaction().run(ctx), context.Canceled)
	require.Equal(t, []string{"verify", "release"}, f.calls)
}

type countingCaptureProgress struct {
	*captureFixture
}

func (*countingCaptureProgress) Record(capturePhase) error { return nil }

func TestRootfsQuarantineHasOnePersistenceOwner(t *testing.T) {
	for _, fail := range []string{"", "quarantine"} {
		lease, progress := &captureFixture{}, &countingCaptureProgress{&captureFixture{fail: fail}}
		tx := lease.transaction()
		tx.Progress = progress
		err := tx.quarantine(errors.New("resume failed"))
		if fail != "" {
			require.ErrorContains(t, err, "quarantine")
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, []string{"quarantine"}, progress.calls)
		require.Empty(t, lease.calls, "the same production journal must not be written again through Lease")
	}
}
