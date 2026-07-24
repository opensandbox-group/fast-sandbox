package containerd

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
)

func TestEnsureContainerdSandboxAbsentAlreadyAbsent(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(false, false, nil)

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, 1, backend.removeSnapshotCalls)
	require.Equal(t, 2, backend.loadContainerCalls)
}

func TestEnsureContainerdSandboxAbsentTaskNotFound(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(true, true, nil)
	backend.container.taskErr = errdefs.ErrNotFound

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.True(t, backend.container.deleteCalled)
	require.False(t, backend.containerExists)
	require.False(t, backend.snapshotExists)
}

func TestEnsureContainerdSandboxAbsentUnknownTaskLoadErrorConvergesWhenContainerIsDeleted(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(true, true, nil)
	backend.container.taskErr = errors.New("task service unavailable")

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.True(t, backend.container.deleteCalled)
}

func TestEnsureContainerdSandboxAbsentStoppedTaskIsNotSignalled(t *testing.T) {
	task := &fakeContainerdDeleteTask{status: containerd.Status{Status: containerd.Stopped}}
	backend := newFakeContainerdDeleteBackend(true, true, task)

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.Empty(t, task.signals)
	require.True(t, task.deleteCalled)
	require.True(t, backend.container.deleteCalled)
}

func TestEnsureContainerdSandboxAbsentRunningTaskStopsGracefully(t *testing.T) {
	waitCh := make(chan containerd.ExitStatus)
	task := &fakeContainerdDeleteTask{
		status: containerd.Status{Status: containerd.Running},
		waitCh: waitCh,
		onKill: func(signal syscall.Signal) {
			if signal == syscall.SIGTERM {
				close(waitCh)
			}
		},
	}
	backend := newFakeContainerdDeleteBackend(true, true, task)

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Second)

	require.NoError(t, err)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, task.signals)
	require.True(t, task.deleteCalled)
}

func TestEnsureContainerdSandboxAbsentRunningTaskIsForceKilledAfterTimeout(t *testing.T) {
	exitCh := make(chan containerd.ExitStatus, 1)
	task := &fakeContainerdDeleteTask{
		status: containerd.Status{Status: containerd.Running},
		waitCh: exitCh, pendingExit: exitCh, requireExitWaitBeforeDelete: true,
		onKill: func(signal syscall.Signal) {
			if signal == syscall.SIGKILL {
				exitCh <- containerd.ExitStatus{}
			}
		},
	}
	backend := newFakeContainerdDeleteBackend(true, true, task)

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, task.signals)
	require.False(t, task.deleteBeforeExitWait, "SIGKILL exit must be consumed before Task.Delete")
	require.True(t, task.deleteCalled)
}

func TestEnsureContainerdSandboxAbsentWaitFailureStillForcesTaskDeletion(t *testing.T) {
	task := &fakeContainerdDeleteTask{
		status:  containerd.Status{Status: containerd.Running},
		waitErr: errors.New("wait service unavailable"),
	}
	backend := newFakeContainerdDeleteBackend(true, true, task)

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, task.signals)
	require.True(t, task.deleteCalled)
}

func TestEnsureContainerdSandboxAbsentUnknownTaskStatusConvergesAfterForcedDelete(t *testing.T) {
	task := &fakeContainerdDeleteTask{
		statusErr: errors.New("status service unavailable"),
	}
	backend := newFakeContainerdDeleteBackend(true, true, task)

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
	require.Empty(t, task.signals)
	require.True(t, task.deleteCalled)
}

func TestEnsureContainerdSandboxAbsentIgnoresCleanupErrorsAfterVerifiedAbsence(t *testing.T) {
	task := &fakeContainerdDeleteTask{
		status:    containerd.Status{Status: containerd.Stopped},
		deleteErr: errors.New("task delete transport error"),
	}
	backend := newFakeContainerdDeleteBackend(true, true, task)
	backend.container.deleteErr = errors.New("container delete transport error")
	backend.container.removeEvenOnDeleteError = true
	backend.removeSnapshotErr = errors.New("snapshot delete transport error")
	backend.removeEvenOnSnapshotError = true

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.NoError(t, err)
}

func TestEnsureContainerdSandboxAbsentFailsWhenContainerRemains(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(true, false, nil)
	backend.container.taskErr = errdefs.ErrNotFound
	backend.container.deleteErr = errors.New("container busy")

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.ErrorContains(t, err, "container busy")
	require.ErrorContains(t, err, "still exists after deletion")
}

func TestEnsureContainerdSandboxAbsentFailsWhenSnapshotRemains(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(false, true, nil)
	backend.removeSnapshotErr = errors.New("snapshot busy")

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.ErrorContains(t, err, "snapshot busy")
	require.ErrorContains(t, err, "still exists after deletion")
}

func TestEnsureContainerdSandboxAbsentFailsWhenVerificationIsUnavailable(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(false, false, nil)
	backend.verifyContainerErr = errors.New("containerd unavailable")
	backend.verifySnapshotErr = errors.New("snapshotter unavailable")

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.ErrorContains(t, err, "containerd unavailable")
	require.ErrorContains(t, err, "snapshotter unavailable")
}

func TestEnsureContainerdSandboxAbsentDoesNotRemoveSnapshotAfterUnknownLoadFailure(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(true, true, nil)
	backend.initialContainerErr = errors.New("containerd unavailable")

	err := ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond)

	require.ErrorContains(t, err, "load container")
	require.Zero(t, backend.removeSnapshotCalls)
	require.True(t, backend.snapshotExists)
}

func TestEnsureContainerdSandboxAbsentIsRepeatable(t *testing.T) {
	backend := newFakeContainerdDeleteBackend(true, true, &fakeContainerdDeleteTask{
		status: containerd.Status{Status: containerd.Stopped},
	})

	require.NoError(t, ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond))
	require.NoError(t, ensureContainerdSandboxAbsent(context.Background(), backend, "sandbox", "snapshot", time.Millisecond))
}

type fakeContainerdDeleteBackend struct {
	container                 *fakeContainerdDeleteContainer
	containerExists           bool
	snapshotExists            bool
	initialContainerErr       error
	verifyContainerErr        error
	verifySnapshotErr         error
	removeSnapshotErr         error
	removeEvenOnSnapshotError bool
	loadContainerCalls        int
	removeSnapshotCalls       int
}

func newFakeContainerdDeleteBackend(containerExists, snapshotExists bool, task *fakeContainerdDeleteTask) *fakeContainerdDeleteBackend {
	backend := &fakeContainerdDeleteBackend{
		containerExists: containerExists,
		snapshotExists:  snapshotExists,
	}
	backend.container = &fakeContainerdDeleteContainer{backend: backend, task: task}
	return backend
}

func (b *fakeContainerdDeleteBackend) LoadContainer(context.Context, string) (containerdDeleteContainer, error) {
	b.loadContainerCalls++
	if b.loadContainerCalls == 1 && b.initialContainerErr != nil {
		return nil, b.initialContainerErr
	}
	if b.loadContainerCalls > 1 && b.verifyContainerErr != nil {
		return nil, b.verifyContainerErr
	}
	if !b.containerExists {
		return nil, errdefs.ErrNotFound
	}
	return b.container, nil
}

func (b *fakeContainerdDeleteBackend) RemoveSnapshot(context.Context, string) error {
	b.removeSnapshotCalls++
	if !b.snapshotExists {
		return errdefs.ErrNotFound
	}
	if b.removeSnapshotErr != nil {
		if b.removeEvenOnSnapshotError {
			b.snapshotExists = false
		}
		return b.removeSnapshotErr
	}
	b.snapshotExists = false
	return nil
}

func (b *fakeContainerdDeleteBackend) StatSnapshot(context.Context, string) error {
	if b.verifySnapshotErr != nil {
		return b.verifySnapshotErr
	}
	if !b.snapshotExists {
		return errdefs.ErrNotFound
	}
	return nil
}

type fakeContainerdDeleteContainer struct {
	backend                 *fakeContainerdDeleteBackend
	task                    *fakeContainerdDeleteTask
	taskErr                 error
	deleteErr               error
	removeEvenOnDeleteError bool
	deleteCalled            bool
}

func (c *fakeContainerdDeleteContainer) Task(context.Context) (containerdDeleteTask, error) {
	if c.taskErr != nil {
		return nil, c.taskErr
	}
	return c.task, nil
}

func (c *fakeContainerdDeleteContainer) Delete(context.Context) error {
	c.deleteCalled = true
	if c.deleteErr != nil {
		if c.removeEvenOnDeleteError {
			c.backend.containerExists = false
		}
		return c.deleteErr
	}
	c.backend.containerExists = false
	// containerd.WithSnapshotCleanup normally removes this snapshot. The
	// explicit backend removal remains necessary for orphaned/partial states.
	c.backend.snapshotExists = false
	return nil
}

type fakeContainerdDeleteTask struct {
	status                      containerd.Status
	statusErr                   error
	waitCh                      <-chan containerd.ExitStatus
	waitErr                     error
	killErr                     map[syscall.Signal]error
	deleteErr                   error
	signals                     []syscall.Signal
	deleteCalled                bool
	onKill                      func(syscall.Signal)
	pendingExit                 chan containerd.ExitStatus
	requireExitWaitBeforeDelete bool
	deleteBeforeExitWait        bool
}

func (t *fakeContainerdDeleteTask) Status(context.Context) (containerd.Status, error) {
	return t.status, t.statusErr
}

func (t *fakeContainerdDeleteTask) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	return t.waitCh, t.waitErr
}

func (t *fakeContainerdDeleteTask) Kill(_ context.Context, signal syscall.Signal) error {
	t.signals = append(t.signals, signal)
	if t.onKill != nil {
		t.onKill(signal)
	}
	return t.killErr[signal]
}

func (t *fakeContainerdDeleteTask) Delete(context.Context) error {
	t.deleteCalled = true
	if t.requireExitWaitBeforeDelete && len(t.pendingExit) > 0 {
		t.deleteBeforeExitWait = true
	}
	return t.deleteErr
}
