package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cdilib "tags.cncf.io/container-device-interface/pkg/cdi"

	"fast-sandbox/internal/fastlet/gpu"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/fastlet/podcgroup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const testGPUUUID = "GPU-00000000-0000-0000-0000-000000000001"
const otherGPUUUID = "GPU-00000000-0000-0000-0000-000000000002"

func emptyGPUCgroup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"cgroup.controllers", "cgroup.procs"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), nil, 0600))
	}
	return root
}

func TestGPUInjectionUsesOnlyReservedCDIDevice(t *testing.T) {
	directory := t.TempDir()
	document := map[string]any{"cdiVersion": "0.5.0", "kind": "nvidia.com/gpu", "devices": []any{
		map[string]any{"name": testGPUUUID, "containerEdits": map[string]any{"env": []string{"ASSIGNED_GPU=one"}, "deviceNodes": []any{map[string]any{"path": "/dev/test-gpu", "hostPath": "/dev/null"}}}},
		map[string]any{"name": "not-assigned", "containerEdits": map[string]any{"env": []string{"UNASSIGNED_GPU=visible"}}},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.json"), raw, 0600))
	require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(directory), cdilib.WithAutoRefresh(false)))
	t.Cleanup(func() {
		require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(cdilib.DefaultSpecDirs...), cdilib.WithAutoRefresh(true)))
	})
	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()
	driver := newTestContainerdRuntime()
	driver.fastletPodUID = "pod-a"
	require.NoError(t, driver.setGPUManager(manager))
	identity := fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	input := &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{Identity: identity}}
	allocation, err := manager.Acquire(identity)
	require.NoError(t, err)
	require.NoError(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: allocation}))
	spec := &oci.Spec{Process: &specs.Process{Env: []string{"USER_CONFIG=kept"}}}
	require.NoError(t, gpuSpecOpt(allocation)(context.Background(), nil, nil, spec))
	require.Contains(t, spec.Process.Env, "ASSIGNED_GPU=one")
	require.Contains(t, spec.Process.Env, "USER_CONFIG=kept")
	require.NotContains(t, spec.Process.Env, "UNASSIGNED_GPU=visible")
	require.Len(t, spec.Linux.Devices, 1)
	require.Equal(t, "/dev/test-gpu", spec.Linux.Devices[0].Path)
	require.Error(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=all"}}}))
	require.Error(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{}))
	input.Sandbox.Identity.RuntimeInstanceID = "replacement"
	require.Error(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: allocation}))
}

func TestGPURecoveryDoesNotGuessMissingDeviceAssignments(t *testing.T) {
	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()
	driver := newTestContainerdRuntime()
	driver.gpuManager = manager
	driver.fastletPodUID = "pod-a"
	identity := fastletapi.SandboxIdentity{SandboxUID: "a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	metadata := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: identity}}
	require.Error(t, driver.recoverGPUs(context.Background(), []*SandboxMetadata{metadata}))
	metadata.Allocation.GPU = &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=" + testGPUUUID}}
	require.NoError(t, driver.recoverGPUs(context.Background(), []*SandboxMetadata{metadata}))
	require.False(t, manager.Available())
	metadata.Config.Identity.RuntimeInstanceID = "other"
	require.Error(t, driver.recoverGPUs(context.Background(), []*SandboxMetadata{metadata}))
	require.False(t, manager.Available())
}

func TestGPURecoveryDoesNotCheckAnOldOwnerAgainstADifferentDaemon(t *testing.T) {
	driver := newTestContainerdRuntime()
	require.ErrorContains(t, driver.verifyGPUOwnerAbsent(context.Background(), gpu.Lease{Endpoint: "/other/containerd.sock"}), "different containerd endpoint")
}

func TestGPUFailedCreateReleasesLeaseForNextAttempt(t *testing.T) {
	driver := newTestContainerdRuntime()
	driver.fastletPodUID = "pod-a"
	driver.gpuCgroupRoot = emptyGPUCgroup(t)
	var err error
	driver.gpuManager, err = gpu.New(t.TempDir(), []string{testGPUUUID}, 1, driver.fastletPodUID, driver.containerdNamespace(), driver.gpuRuntimeEndpoint())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, driver.Close()) })
	tasks := &orphanGPUTaskClient{}
	driver.client, err = containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(&missingGPUContainerStore{}), containerd.WithTaskClient(tasks),
	))
	require.NoError(t, err)
	cfg := fastletnetwork.DefaultConfig(1, driver.fastletPodUID)
	driver.networkManager, err = fastletnetwork.NewManager(cfg, fastletnetwork.NewLinuxNetNSDriver(fastletnetwork.LinuxDriverConfig{}), fastletnetwork.NewFileStateStore(t.TempDir()))
	require.NoError(t, err)

	identity := fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", FastletPodUID: driver.fastletPodUID, RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	input := &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{Identity: identity}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = driver.EnsureSandbox(ctx, input)
	require.ErrorIs(t, err, fastletnetwork.ErrNoCleanSlot)

	input.Sandbox.Identity.AssignmentAttempt++
	input.Sandbox.Identity.RuntimeInstanceID = "runtime-b"
	_, err = driver.EnsureSandbox(t.Context(), input)
	require.ErrorIs(t, err, fastletnetwork.ErrNoCleanSlot, "the new attempt must get past GPU admission")
	require.True(t, driver.gpuManager.Available())

	tasks.tasks = []*tasktypes.Process{{ID: identity.SandboxUID}}
	_, err = driver.EnsureSandbox(t.Context(), input)
	require.ErrorContains(t, err, "orphan task")
	require.False(t, driver.gpuManager.Available())
}

func TestGPUCleanupRetainsLeaseUntilOrphanTaskIsAbsent(t *testing.T) {
	for _, path := range []string{"delete", "same-pod recovery", "replacement-pod recovery"} {
		t.Run(path, func(t *testing.T) {
			const sandboxID = "sandbox-a"
			ctx := context.Background()
			driver := newTestContainerdRuntime()
			driver.fastletPodUID = "pod-a"
			driver.gpuCgroupRoot = emptyGPUCgroup(t)
			driver.podCgroup = &podcgroup.Layout{Version: podcgroup.VersionV2, PodPath: "/kubepods/podb"}
			leaseNamespace := driver.containerdNamespace()
			root := t.TempDir()
			manager, err := gpu.New(root, []string{testGPUUUID}, 1, driver.fastletPodUID, leaseNamespace, driver.gpuRuntimeEndpoint())
			require.NoError(t, err)
			identity := fastletapi.SandboxIdentity{SandboxUID: sandboxID, FastletPodUID: driver.fastletPodUID, RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
			_, err = manager.Acquire(identity)
			require.NoError(t, err)
			if path != "delete" {
				require.NoError(t, manager.Close())
				if path == "replacement-pod recovery" {
					driver.fastletPodUID = "pod-b"
					driver.config.Namespace = "new-namespace"
					driver.podCgroup.PodPath = "/kubepods/podpod-b"
				}
				manager, err = gpu.New(root, []string{testGPUUUID}, 1, driver.fastletPodUID, driver.containerdNamespace(), driver.gpuRuntimeEndpoint())
				require.NoError(t, err)
			}
			driver.gpuManager = manager

			tasks := &orphanGPUTaskClient{tasks: []*tasktypes.Process{{ID: sandboxID, Status: tasktypes.Status_RUNNING}}}
			driver.client, err = containerd.New("", containerd.WithDefaultRuntime("io.containerd.runc.v2"), containerd.WithServices(
				containerd.WithContainerStore(&missingGPUContainerStore{}),
				containerd.WithTaskClient(tasks),
				containerd.WithSnapshotters(map[string]snapshots.Snapshotter{driver.snapshotter(): &missingGPUSnapshotter{}}),
			))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, driver.Close()) })

			cleanup := func() error { return driver.recoverGPUs(ctx, nil) }
			if path == "delete" {
				cleanup = func() error { return driver.DeleteSandbox(ctx, sandboxID) }
			}

			require.ErrorContains(t, cleanup(), "orphan task")
			require.Equal(t, leaseNamespace, tasks.namespace)
			require.False(t, manager.Available())

			tasks.err = status.Error(codes.Unavailable, "task service unavailable")
			require.ErrorContains(t, cleanup(), "task service unavailable")
			require.False(t, manager.Available())

			tasks.err = nil
			tasks.tasks = []*tasktypes.Process{{ID: "unrelated-task"}}
			oldLayout := podcgroup.Layout{Version: podcgroup.VersionV2, PodPath: "/kubepods/podpod-a"}
			path, err := oldLayout.SandboxPath(sandboxID)
			require.NoError(t, err)
			oldCgroup := filepath.Join(driver.gpuCgroupRoot, path)
			require.NoError(t, os.MkdirAll(oldCgroup, 0700))
			require.ErrorContains(t, cleanup(), "cgroup.events")
			require.False(t, manager.Available())
			events := filepath.Join(oldCgroup, "cgroup.events")
			require.NoError(t, os.WriteFile(events, []byte("populated 1\n"), 0600))
			require.ErrorContains(t, cleanup(), "populated")
			require.False(t, manager.Available())
			require.NoError(t, os.WriteFile(events, []byte("populated 0\n"), 0600))
			require.NoError(t, cleanup())
			require.True(t, manager.Available())
			require.NoError(t, cleanup())
		})
	}
}

func TestGPUReclaimsDisjointForeignLeaseAfterOwnerExits(t *testing.T) {
	driver := newTestContainerdRuntime()
	driver.fastletPodUID = "pod-b"
	driver.gpuCgroupRoot = emptyGPUCgroup(t)
	journal := t.TempDir()
	old, err := gpu.New(journal, []string{testGPUUUID}, 1, "pod-a", driver.containerdNamespace(), driver.gpuRuntimeEndpoint())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.Close()) })
	identity := fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	_, err = old.Acquire(identity)
	require.NoError(t, err)
	driver.gpuManager, err = gpu.New(journal, []string{otherGPUUUID}, 1, "pod-b", driver.containerdNamespace(), driver.gpuRuntimeEndpoint())
	require.NoError(t, err)
	driver.client, err = containerd.New("", containerd.WithDefaultRuntime("io.containerd.runc.v2"), containerd.WithServices(
		containerd.WithContainerStore(&missingGPUContainerStore{}), containerd.WithTaskClient(&orphanGPUTaskClient{}),
	))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, driver.Close()) })

	require.ErrorContains(t, driver.DeleteSandbox(t.Context(), identity.SandboxUID), "active Fastlet owner")
	require.ErrorContains(t, driver.reclaimForeignGPULease(t.Context(), identity.SandboxUID), "active Fastlet owner")
	require.NoError(t, old.Close())
	require.NoError(t, driver.DeleteSandbox(t.Context(), identity.SandboxUID))
	identity.FastletPodUID, identity.RuntimeInstanceID, identity.InstanceGeneration = "pod-b", "runtime-b", 2
	allocation, err := driver.gpuManager.Acquire(identity)
	require.NoError(t, err)
	require.Equal(t, []string{gpu.ResourceName + "=" + otherGPUUUID}, allocation.Devices)
}

func TestGPURecoveryReclaimsLeaseSplitAcrossReplacementFastlets(t *testing.T) {
	ids := []string{testGPUUUID, otherGPUUUID}
	journal := t.TempDir()
	old, err := gpu.New(journal, ids, 2, "old-pod", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	identity := fastletapi.SandboxIdentity{SandboxUID: "old-sandbox", FastletPodUID: "old-pod", RuntimeInstanceID: "old-instance", InstanceGeneration: 1, AssignmentAttempt: 1}
	_, err = old.Acquire(identity)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	tasks := &orphanGPUTaskClient{tasks: []*tasktypes.Process{{ID: identity.SandboxUID}}}
	var replacements []*Driver
	for i, id := range ids {
		driver := newTestContainerdRuntime()
		driver.config.Namespace = "k8s.io"
		driver.fastletPodUID = fmt.Sprintf("replacement-%d", i)
		driver.gpuCgroupRoot = emptyGPUCgroup(t)
		driver.gpuManager, err = gpu.New(journal, []string{id}, 1, driver.fastletPodUID, driver.containerdNamespace(), driver.gpuRuntimeEndpoint())
		require.NoError(t, err)
		driver.client, err = containerd.New("", containerd.WithDefaultRuntime("io.containerd.runc.v2"), containerd.WithServices(
			containerd.WithContainerStore(&missingGPUContainerStore{}), containerd.WithTaskClient(tasks),
		))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, driver.Close()) })
		replacements = append(replacements, driver)
	}
	for _, driver := range replacements {
		require.Error(t, driver.recoverGPUs(t.Context(), nil))
		require.False(t, driver.gpuManager.Available())
	}
	tasks.tasks = nil
	for i, driver := range replacements {
		require.NoError(t, driver.recoverGPUs(t.Context(), nil))
		owner := fastletapi.SandboxIdentity{SandboxUID: driver.fastletPodUID, FastletPodUID: driver.fastletPodUID, RuntimeInstanceID: "new-instance", InstanceGeneration: 1, AssignmentAttempt: 1}
		allocation, err := driver.gpuManager.Acquire(owner)
		require.NoError(t, err)
		require.Equal(t, []string{gpu.ResourceName + "=" + ids[i]}, allocation.Devices)
	}
}

type missingGPUContainerStore struct{ containers.Store }

func (*missingGPUContainerStore) Get(context.Context, string) (containers.Container, error) {
	return containers.Container{}, errdefs.ErrNotFound
}

type missingGPUSnapshotter struct{ snapshots.Snapshotter }

func (*missingGPUSnapshotter) Remove(context.Context, string) error { return errdefs.ErrNotFound }

func (*missingGPUSnapshotter) Stat(context.Context, string) (snapshots.Info, error) {
	return snapshots.Info{}, errdefs.ErrNotFound
}

type orphanGPUTaskClient struct {
	tasksapi.TasksClient
	tasks     []*tasktypes.Process
	err       error
	namespace string
}

func (*orphanGPUTaskClient) Get(context.Context, *tasksapi.GetRequest, ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	return nil, status.Error(codes.NotFound, "container metadata not found")
}

func (c *orphanGPUTaskClient) List(ctx context.Context, _ *tasksapi.ListTasksRequest, _ ...grpc.CallOption) (*tasksapi.ListTasksResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.namespace, _ = namespaces.Namespace(ctx)
	return &tasksapi.ListTasksResponse{Tasks: c.tasks}, c.err
}
