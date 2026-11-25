package containerd

import (
	"context"
	"errors"
	"fmt"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

const (
	gpuFailureLabel = "fast-sandbox.io/gpu-runtime-failure"
	gpuFailedPhase  = "runtime-failed"
)

// SandboxRuntimeExited is used by Fastlet's heartbeat failure handler. A
// transient containerd error or a live sandbox with an exited child is not an exit.
func (r *Driver) SandboxRuntimeExited(sandboxID string) bool {
	if r.gpuManager == nil || r.client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitStopTimeout)
	defer cancel()
	return r.gpuTaskExited(r.withNamespace(ctx), sandboxID)
}

func (r *Driver) gpuTaskExited(ctx context.Context, sandboxID string) bool {
	response, err := r.client.TaskService().Get(ctx, &tasksapi.GetRequest{ContainerID: sandboxID})
	return errdefs.IsNotFound(err) || status.Code(err) == codes.NotFound ||
		(err == nil && response.GetProcess().GetStatus() == tasktypes.Status_STOPPED)
}

// RecordTerminalFailure retains the failed identity for Controller acknowledgement
// before returning any resource that a new Sandbox can use.
func (r *Driver) RecordTerminalFailure(ctx context.Context, identity fastletapi.SandboxIdentity, message string) error {
	if r.gpuManager == nil {
		return runtimecontract.ErrUnsupportedRuntime
	}
	metadata, err := r.InspectSandbox(ctx, identity.SandboxUID)
	if err != nil {
		return err
	}
	if metadata.Allocation.GPU == nil || !runtimecontract.SameRuntimeIdentity(metadata.Config.Identity, identity) ||
		(metadata.Phase != string(containerd.Stopped) && metadata.Phase != gpuFailedPhase) {
		return errors.New("GPU runtime identity or exit state changed before terminal failure recording")
	}
	if metadata.Phase == gpuFailedPhase {
		return nil
	}
	ctx = r.withNamespace(ctx)
	if !r.gpuTaskExited(ctx, identity.SandboxUID) {
		return errors.New("GPU runtime task exit is not confirmed")
	}
	container, err := r.client.LoadContainer(ctx, identity.SandboxUID)
	if err != nil {
		return err
	}
	if message == "" {
		message = "GPU runtime exited unexpectedly"
	}
	_, err = container.SetLabels(ctx, map[string]string{gpuFailureLabel: message})
	return err
}

func (r *Driver) CleanupTerminalFailure(ctx context.Context, identity fastletapi.SandboxIdentity) error {
	metadata, err := r.InspectSandbox(ctx, identity.SandboxUID)
	if err != nil {
		return err
	}
	if metadata.Phase != gpuFailedPhase || !runtimecontract.SameRuntimeIdentity(metadata.Config.Identity, identity) {
		return errors.New("GPU terminal cleanup identity does not match the failed runtime")
	}
	lease, hasLease, err := r.lookupGPULease(identity.SandboxUID)
	if err != nil {
		return err
	}
	if hasLease && !lease.MatchesOwner(identity) {
		return errors.New("GPU terminal cleanup lease belongs to another runtime instance")
	}
	ctx = r.withNamespace(ctx)
	container, err := r.client.LoadContainer(ctx, identity.SandboxUID)
	if err != nil {
		return err
	}
	// Keep the failed container record; only its task may still hold a GPU.
	cleanupErrs := deleteContainerdTask(ctx, containerdDeleteContainerAdapter{container: container}, identity.SandboxUID, waitStopTimeout)
	if err := r.verifyGPUTaskAndProcessesAbsent(ctx, identity, r.containerdNamespace()); err != nil {
		return errors.Join(append(cleanupErrs, err)...)
	}
	if hasLease {
		if err := r.gpuManager.ReleaseVerified(lease); err != nil {
			return err
		}
	}
	return r.removeSandboxResources(ctx, identity.SandboxUID)
}

func (r *Driver) recoverGPUTerminalFailures(ctx context.Context, managed []*SandboxMetadata, retireOwner runtimecontract.ResourceRecoveryOwnerRetirer) error {
	if r.gpuManager == nil {
		return nil
	}
	for _, metadata := range managed {
		if metadata == nil || metadata.Allocation.GPU == nil || (metadata.Phase != string(containerd.Stopped) && metadata.Phase != gpuFailedPhase) {
			continue
		}
		identity := metadata.Config.Identity
		if err := r.RecordTerminalFailure(ctx, identity, metadata.RuntimeMessage); err != nil {
			return fmt.Errorf("record failed GPU runtime %q: %w", identity.SandboxUID, err)
		}
		metadata.Phase = gpuFailedPhase
		if retireOwner != nil {
			if err := retireOwner(ctx, metadata); err != nil {
				return err
			}
		}
		if err := r.CleanupTerminalFailure(ctx, identity); err != nil {
			return fmt.Errorf("reclaim failed GPU runtime %q: %w", identity.SandboxUID, err)
		}
	}
	return nil
}
