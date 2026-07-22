package runtime

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"k8s.io/klog/v2"
)

// containerdDeleteBackend is deliberately narrower than containerd.Client so
// the deletion state machine can be verified without a live containerd daemon.
type containerdDeleteBackend interface {
	LoadContainer(context.Context, string) (containerdDeleteContainer, error)
	RemoveSnapshot(context.Context, string) error
	StatSnapshot(context.Context, string) error
}

type containerdDeleteContainer interface {
	Task(context.Context) (containerdDeleteTask, error)
	Delete(context.Context) error
}

type containerdDeleteTask interface {
	Status(context.Context) (containerd.Status, error)
	Wait(context.Context) (<-chan containerd.ExitStatus, error)
	Kill(context.Context, syscall.Signal) error
	Delete(context.Context) error
}

type containerdDeleteClient struct {
	client *containerd.Client
}

func (b containerdDeleteClient) LoadContainer(ctx context.Context, id string) (containerdDeleteContainer, error) {
	c, err := b.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	return containerdDeleteContainerAdapter{container: c}, nil
}

func (b containerdDeleteClient) RemoveSnapshot(ctx context.Context, name string) error {
	return b.client.SnapshotService("").Remove(ctx, name)
}

func (b containerdDeleteClient) StatSnapshot(ctx context.Context, name string) error {
	_, err := b.client.SnapshotService("").Stat(ctx, name)
	return err
}

type containerdDeleteContainerAdapter struct {
	container containerd.Container
}

func (c containerdDeleteContainerAdapter) Task(ctx context.Context) (containerdDeleteTask, error) {
	task, err := c.container.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	return containerdDeleteTaskAdapter{task: task}, nil
}

func (c containerdDeleteContainerAdapter) Delete(ctx context.Context) error {
	return c.container.Delete(ctx, containerd.WithSnapshotCleanup)
}

type containerdDeleteTaskAdapter struct {
	task containerd.Task
}

func (t containerdDeleteTaskAdapter) Status(ctx context.Context) (containerd.Status, error) {
	return t.task.Status(ctx)
}

func (t containerdDeleteTaskAdapter) Wait(ctx context.Context) (<-chan containerd.ExitStatus, error) {
	return t.task.Wait(ctx)
}

func (t containerdDeleteTaskAdapter) Kill(ctx context.Context, signal syscall.Signal) error {
	return t.task.Kill(ctx, signal)
}

func (t containerdDeleteTaskAdapter) Delete(ctx context.Context) error {
	_, err := t.task.Delete(ctx, containerd.WithProcessKill)
	return err
}

// ensureContainerdSandboxAbsent treats deletion as an ensure-absent state
// transition. Intermediate cleanup errors do not keep a Sandbox finalizer
// alive when final verification proves both runtime objects are gone.
func ensureContainerdSandboxAbsent(
	ctx context.Context,
	backend containerdDeleteBackend,
	sandboxID string,
	snapshotName string,
	gracePeriod time.Duration,
) error {
	container, err := backend.LoadContainer(ctx, sandboxID)
	if err != nil && !errdefs.IsNotFound(err) {
		// An unknown load failure does not prove absence. Removing the snapshot in
		// this state could damage a container which is still live but unreadable.
		return fmt.Errorf("load container %q before deletion: %w", sandboxID, err)
	}

	var cleanupErrs []error
	if err == nil {
		cleanupErrs = append(cleanupErrs, deleteContainerdTask(ctx, container, sandboxID, gracePeriod)...)
		if deleteErr := container.Delete(ctx); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete container %q: %w", sandboxID, deleteErr))
		}
	}

	if snapshotErr := backend.RemoveSnapshot(ctx, snapshotName); snapshotErr != nil && !errdefs.IsNotFound(snapshotErr) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove snapshot %q: %w", snapshotName, snapshotErr))
	}

	verificationErr := verifyContainerdSandboxAbsent(ctx, backend, sandboxID, snapshotName)
	if verificationErr == nil {
		if len(cleanupErrs) > 0 {
			klog.InfoS("Containerd deletion converged after cleanup errors", "sandbox", sandboxID, "errors", errors.Join(cleanupErrs...))
		}
		return nil
	}
	cleanupErrs = append(cleanupErrs, verificationErr)
	return errors.Join(cleanupErrs...)
}

func deleteContainerdTask(
	ctx context.Context,
	container containerdDeleteContainer,
	sandboxID string,
	gracePeriod time.Duration,
) []error {
	task, err := container.Task(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return []error{fmt.Errorf("load task for container %q: %w", sandboxID, err)}
	}

	var cleanupErrs []error
	status, statusErr := task.Status(ctx)
	if statusErr != nil && !errdefs.IsNotFound(statusErr) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("inspect task for container %q: %w", sandboxID, statusErr))
	}
	if statusErr == nil && status.Status == containerd.Running {
		waitCh, waitErr := task.Wait(ctx)
		if waitErr != nil && !errdefs.IsNotFound(waitErr) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("wait task for container %q: %w", sandboxID, waitErr))
		}
		if killErr := task.Kill(ctx, syscall.SIGTERM); killErr != nil && !errdefs.IsNotFound(killErr) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("signal task for container %q: %w", sandboxID, killErr))
		}
		if waitErr == nil && !waitForContainerdTask(ctx, waitCh, gracePeriod) {
			if killErr := task.Kill(ctx, syscall.SIGKILL); killErr != nil && !errdefs.IsNotFound(killErr) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("force signal task for container %q: %w", sandboxID, killErr))
			}
		}
	}
	if taskDeleteErr := task.Delete(ctx); taskDeleteErr != nil && !errdefs.IsNotFound(taskDeleteErr) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delete task for container %q: %w", sandboxID, taskDeleteErr))
	}
	return cleanupErrs
}

func waitForContainerdTask(ctx context.Context, waitCh <-chan containerd.ExitStatus, timeout time.Duration) bool {
	if waitCh == nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waitCh:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func verifyContainerdSandboxAbsent(ctx context.Context, backend containerdDeleteBackend, sandboxID, snapshotName string) error {
	var verificationErrs []error
	if _, err := backend.LoadContainer(ctx, sandboxID); err == nil {
		verificationErrs = append(verificationErrs, fmt.Errorf("container %q still exists after deletion", sandboxID))
	} else if !errdefs.IsNotFound(err) {
		verificationErrs = append(verificationErrs, fmt.Errorf("verify container %q deletion: %w", sandboxID, err))
	}
	if err := backend.StatSnapshot(ctx, snapshotName); err == nil {
		verificationErrs = append(verificationErrs, fmt.Errorf("snapshot %q still exists after deletion", snapshotName))
	} else if !errdefs.IsNotFound(err) {
		verificationErrs = append(verificationErrs, fmt.Errorf("verify snapshot %q deletion: %w", snapshotName, err))
	}
	return errors.Join(verificationErrs...)
}
