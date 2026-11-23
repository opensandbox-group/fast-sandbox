package containerd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerdcdi "github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	cdilib "tags.cncf.io/container-device-interface/pkg/cdi"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/fastlet/gpu"
	"fast-sandbox/internal/fastlet/podcgroup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// ConfigureGPU initializes the device inventory and durable leases for a GPU Fastlet.
// Call it after Initialize and SetNamespace, before recovering Sandboxes.
func (r *Driver) ConfigureGPU(ctx context.Context, profile apiv1alpha2.SandboxResourceProfile, containerName string, expectedDevices int) error {
	if r.runtimeName != apiv1alpha2.RuntimeContainer {
		return fmt.Errorf("%w: GPU allocation requires the container runtime", runtimecontract.ErrUnsupportedRuntime)
	}
	if r.client == nil || r.fastletPodUID == "" {
		return fmt.Errorf("%w: GPU configuration requires an initialized Fastlet Pod", runtimecontract.ErrInvalidConfig)
	}
	if r.gpuManager != nil {
		return fmt.Errorf("%w: GPU manager is already configured", runtimecontract.ErrInvalidConfig)
	}
	socket := filepath.Join(runtimecatalog.GPUPodResourcesMountPath, "kubelet.sock")
	ids, err := gpu.AssignedDevices(ctx, socket, r.fastletNamespace, r.fastletPodName, containerName, expectedDevices)
	if err != nil {
		return err
	}
	if err := gpu.ValidateAssignedDevices(ctx, ids, profile.GPURequirements); err != nil {
		return err
	}
	manager, err := gpu.New(runtimecatalog.GPUStateRoot, ids, int(profile.GPUs), r.fastletPodUID, r.containerdNamespace(), r.gpuRuntimeEndpoint())
	if err != nil {
		return err
	}
	if err := r.setGPUManager(manager); err != nil {
		return errors.Join(err, manager.Close())
	}
	return nil
}

func (r *Driver) setGPUManager(manager *gpu.Manager) error {
	if manager == nil || r.gpuManager != nil {
		return errors.New("GPU manager must be set exactly once")
	}
	if err := cdilib.Refresh(); err != nil {
		return fmt.Errorf("refresh NVIDIA CDI inventory: %w", err)
	}
	// Resolve every assigned GPU before the Fastlet can advertise readiness.
	for _, device := range manager.Devices() {
		if cdilib.GetDefaultCache().GetDevice(device) == nil {
			return fmt.Errorf("assigned GPU has no CDI definition: %s", device)
		}
	}
	r.gpuManager = manager
	return nil
}

// validateGPUAllocation ensures the sandbox's GPU allocation matches its durable device lease and ownership.
func (r *Driver) validateGPUAllocation(input *fastletapi.EnsureSandboxInput, allocation fastletapi.RuntimeAllocation) error {
	if r.gpuManager == nil {
		if allocation.GPU != nil {
			return errors.New("GPU allocation requires a configured GPU manager")
		}
		return nil
	}
	if input == nil || allocation.GPU == nil {
		return errors.New("GPU Pool requires a reserved GPU allocation")
	}
	owner := input.Sandbox.Identity
	lease, found, err := r.gpuManager.Lookup(owner.SandboxUID)
	if err != nil {
		return err
	}
	if !found || lease.Owner.FastletPodUID != r.fastletPodUID {
		return errors.New("GPU allocation is not durably reserved by this Fastlet")
	}
	if !lease.MatchesOwner(owner) || !slices.Equal(lease.Devices, allocation.GPU.Devices) {
		return errors.New("GPU allocation does not match its durable runtime owner")
	}
	return nil
}

func (r *Driver) recoverGPUs(ctx context.Context, managed []*SandboxMetadata) error {
	if r.gpuManager == nil {
		for _, metadata := range managed {
			if metadata != nil && metadata.Allocation.GPU != nil {
				return errors.New("GPU runtimes cannot recover without their GPU manager")
			}
		}
		return nil
	}
	present, err := r.recoverGPUInventory(managed)
	if err != nil {
		return err
	}
	// A crash may leave a reservation before the container object was created.
	// Cleanup uses exact IDs and retains the lease on any ambiguous outcome.
	records, err := r.gpuManager.Records()
	if err != nil {
		return err
	}
	for _, lease := range records {
		sandboxID := lease.Owner.SandboxUID
		if lease.Owner.FastletPodUID != r.fastletPodUID {
			if err := r.releaseGPULease(ctx, lease); err != nil {
				return err
			}
			continue
		}
		if present[sandboxID] {
			continue
		}
		if _, err := r.InspectSandbox(ctx, sandboxID); err == nil {
			return fmt.Errorf("container %q is missing from the GPU runtime inventory", sandboxID)
		} else if !errors.Is(err, ErrSandboxNotFound) {
			return fmt.Errorf("check GPU runtime %q before recovery cleanup: %w", sandboxID, err)
		}
		if err := r.DeleteSandbox(ctx, sandboxID); err != nil {
			return err
		}
	}
	return nil
}

func gpuSpecOpt(allocation *fastletapi.GPUAllocation) oci.SpecOpts {
	return containerdcdi.WithCDIDevices(allocation.Devices...)
}

func (r *Driver) lookupGPULease(sandboxID string) (gpu.Lease, bool, error) {
	if r.gpuManager == nil {
		return gpu.Lease{}, false, nil
	}
	return r.gpuManager.Lookup(sandboxID)
}

func (r *Driver) reclaimForeignGPULease(ctx context.Context, sandboxID string) error {
	lease, found, err := r.lookupGPULease(sandboxID)
	if err != nil || !found || lease.Owner.FastletPodUID == r.fastletPodUID {
		return err
	}
	return r.releaseGPULease(ctx, lease)
}

func (r *Driver) releaseFailedCreateGPU(ctx context.Context, owner fastletapi.SandboxIdentity, allocation *fastletapi.GPUAllocation) error {
	// A failed or canceled Create can still leave a container or task behind.
	// Leave that lease for the normal runtime deletion path if absence is uncertain.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitStopTimeout)
	defer cancel()
	return r.releaseGPULease(ctx, gpu.Lease{
		Owner: owner, Devices: allocation.Devices,
		Namespace: r.containerdNamespace(), Endpoint: r.gpuRuntimeEndpoint(),
	})
}

func (r *Driver) releaseGPULease(ctx context.Context, lease gpu.Lease) error {
	if lease.Owner.FastletPodUID != r.fastletPodUID {
		unlock, err := r.gpuManager.FenceForeignOwner(lease)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if err := r.verifyGPUOwnerAbsent(ctx, lease); err != nil {
		return err
	}
	return r.gpuManager.ReleaseVerified(lease)
}

// An orphan task can outlive its container metadata. This check also applies
// to foreign leases, where Fastlet must leave runtime cleanup to NodeJanitor.
func (r *Driver) verifyGPUOwnerAbsent(ctx context.Context, lease gpu.Lease) error {
	if lease.Endpoint != r.gpuRuntimeEndpoint() {
		return errors.New("cannot verify prior GPU owner from a different containerd endpoint; operator reconciliation is required")
	}
	ctx = namespaces.WithNamespace(ctx, lease.Namespace)
	_, err := r.client.LoadContainer(ctx, lease.Owner.SandboxUID)
	if err == nil {
		return fmt.Errorf("GPU is still owned by container %s of Fastlet %s", lease.Owner.SandboxUID, lease.Owner.FastletPodUID)
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return r.verifyGPUTaskAndProcessesAbsent(ctx, lease.Owner, lease.Namespace)
}

func (r *Driver) verifyGPUTaskAndProcessesAbsent(ctx context.Context, owner fastletapi.SandboxIdentity, namespace string) error {
	sandboxID := owner.SandboxUID
	// TaskService.Get also loads container metadata before looking up the task.
	// List walks the runtime's task inventory independently of that metadata.
	tasks, err := r.client.TaskService().List(ctx, &tasksapi.ListTasksRequest{})
	if err != nil {
		return fmt.Errorf("list containerd tasks before releasing GPU for %q: %w", sandboxID, err)
	}
	for _, task := range tasks.GetTasks() {
		if task.GetID() == sandboxID {
			return fmt.Errorf("GPU is still owned by orphan task %s", sandboxID)
		}
	}

	// containerd can omit tasks whose shim fails to answer. The kernel can
	// still account for the workload processes in the old Sandbox cgroup.
	root := r.gpuCgroupRoot
	if root == "" {
		root = podcgroup.HostRoot
		if r.podCgroup == nil {
			root = podcgroup.HostPath
		}
	}
	podUID := owner.FastletPodUID
	if r.podCgroup == nil {
		podUID = ""
	}
	if err := podcgroup.VerifySandboxProcessesAbsent(root, podUID, sandboxID, filepath.Join("/", namespace, sandboxID)); err != nil {
		return fmt.Errorf("verify GPU owner %q processes: %w", sandboxID, err)
	}
	return nil
}

func (r *Driver) recoverGPUInventory(managed []*SandboxMetadata) (map[string]bool, error) {
	present := map[string]bool{}
	for _, metadata := range managed {
		if metadata == nil || metadata.Phase == gpuFailedPhase {
			continue
		}
		if metadata.Allocation.GPU == nil {
			return nil, errors.New("GPU Pool runtime has no durable device allocation")
		}
		if metadata.Config.Identity.FastletPodUID != r.fastletPodUID {
			return nil, errors.New("GPU runtime belongs to a different Fastlet Pod")
		}
		lease := gpu.Lease{
			Owner:     metadata.Config.Identity,
			Devices:   metadata.Allocation.GPU.Devices,
			Namespace: r.containerdNamespace(),
			Endpoint:  r.gpuRuntimeEndpoint(),
		}
		if err := r.gpuManager.Recover(lease); err != nil {
			return nil, err
		}
		present[lease.Owner.SandboxUID] = true
	}
	return present, nil
}

func (r *Driver) gpuRuntimeEndpoint() string {
	if r.socketPath != "" {
		return r.socketPath
	}
	return gpu.DefaultContainerdSocket
}
