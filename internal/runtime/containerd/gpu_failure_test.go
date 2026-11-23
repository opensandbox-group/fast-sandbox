package containerd

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/fastlet/gpu"
	fastletsandbox "fast-sandbox/internal/fastlet/sandbox"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type gpuFailureStore struct {
	containers.Store
	mu        sync.Mutex
	container containers.Container
	present   bool
	task      bool
	state     tasktypes.Status
	updateErr error
	taskErr   error
}

func (s *gpuFailureStore) Get(_ context.Context, id string) (containers.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.present || id != s.container.ID {
		return containers.Container{}, errdefs.ErrNotFound
	}
	result := s.container
	result.Labels = maps.Clone(result.Labels)
	return result, nil
}

func (s *gpuFailureStore) List(ctx context.Context, _ ...string) ([]containers.Container, error) {
	item, err := s.Get(ctx, "sandbox")
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	return []containers.Container{item}, err
}

func (s *gpuFailureStore) Update(_ context.Context, item containers.Container, _ ...string) (containers.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return containers.Container{}, s.updateErr
	}
	if !s.present || item.ID != s.container.ID {
		return containers.Container{}, errdefs.ErrNotFound
	}
	maps.Copy(s.container.Labels, item.Labels)
	result := s.container
	result.Labels = maps.Clone(result.Labels)
	return result, nil
}

func (s *gpuFailureStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.container.ID {
		return errdefs.ErrNotFound
	}
	s.present = false
	return nil
}

type gpuFailureTasks struct {
	tasksapi.TasksClient
	store *gpuFailureStore
}

func (s *gpuFailureTasks) Get(_ context.Context, _ *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.taskErr != nil {
		return nil, s.store.taskErr
	}
	if !s.store.task {
		return nil, status.Error(codes.NotFound, "task absent")
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: "sandbox", Pid: 1234, Status: s.store.state}}, nil
}

func (s *gpuFailureTasks) Delete(context.Context, *tasksapi.DeleteTaskRequest, ...grpc.CallOption) (*tasksapi.DeleteResponse, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.store.task = false
	return &tasksapi.DeleteResponse{}, nil
}

func (s *gpuFailureTasks) List(context.Context, *tasksapi.ListTasksRequest, ...grpc.CallOption) (*tasksapi.ListTasksResponse, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.taskErr != nil {
		return nil, s.store.taskErr
	}
	response := &tasksapi.ListTasksResponse{}
	if s.store.task {
		response.Tasks = []*tasktypes.Process{{ID: "sandbox", Status: s.store.state}}
	}
	return response, nil
}

func (*gpuFailureTasks) Wait(context.Context, *tasksapi.WaitRequest, ...grpc.CallOption) (*tasksapi.WaitResponse, error) {
	return &tasksapi.WaitResponse{ExitStatus: 137}, nil
}

func (*gpuFailureTasks) Kill(context.Context, *tasksapi.KillRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// The facade supplies daemon capability checks without changing the real GPU
// lifecycle methods exercised by the Fastlet manager.
type gpuFailureRuntime struct{ *Driver }

func (r gpuFailureRuntime) ProbeCapabilities(context.Context) CapabilityReport {
	return CapabilityReport{Runtime: apiv1alpha2.RuntimeContainer, State: runtimecatalog.CapabilityReady}
}

func newGPUFailureFixture(t *testing.T) (*Driver, *gpuFailureStore, fastletapi.SandboxIdentity, string) {
	t.Helper()
	root := t.TempDir()
	manager, err := gpu.New(root, []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	identity := fastletapi.SandboxIdentity{SandboxUID: "sandbox", Name: "test", Namespace: "ns", FastletPodUID: "pod-a", RuntimeInstanceID: "instance", InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1}
	allocation, err := manager.Acquire(identity)
	require.NoError(t, err)
	driver := newTestContainerdRuntime()
	driver.gpuManager, driver.fastletPodUID = manager, "pod-a"
	driver.gpuCgroupRoot = emptyGPUCgroup(t)
	driver.config.Namespace, driver.config.Snapshotter = "k8s.io", "overlayfs"
	config := fastletapi.RuntimeSandboxConfig{Identity: identity, Desired: fastletapi.SandboxDesiredSpec{Image: "test/cuda"}}
	store := &gpuFailureStore{present: true, task: true, state: tasktypes.Status_RUNNING, container: containers.Container{
		ID: "sandbox", Image: "test/cuda", Labels: driver.prepareLabels(&config, fastletapi.RuntimeAllocation{GPU: allocation}),
	}}
	driver.client, err = containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(store), containerd.WithTaskClient(&gpuFailureTasks{store: store}),
		containerd.WithSnapshotters(map[string]snapshots.Snapshotter{"overlayfs": &missingGPUSnapshotter{}}),
	))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, driver.Close()) })
	return driver, store, identity, root
}

func TestGPUUnexpectedExitReclaimsWithoutDeleteAndRetainsFailure(t *testing.T) {
	driver, store, identity, _ := newGPUFailureFixture(t)
	manager, err := fastletsandbox.NewSandboxManagerWithConfig(gpuFailureRuntime{driver}, fastletsandbox.SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-a", RuntimeName: apiv1alpha2.RuntimeContainer, RecoverOnStart: true,
	})
	require.NoError(t, err)
	require.NoError(t, manager.Recover(context.Background()))
	require.False(t, driver.SandboxRuntimeExited(identity.SandboxUID), "a live Sandbox retains its GPU when a child process exits")
	store.mu.Lock()
	store.state = tasktypes.Status_STOPPED
	store.mu.Unlock()
	statuses := manager.GetSandboxStatuses(context.Background())
	require.Len(t, statuses, 1)
	require.Equal(t, fastletapi.RuntimeStateFailed, statuses[0].Runtime.State)
	require.Eventually(t, func() bool {
		admission, _, _ := manager.State()
		return admission.Used == 0 && driver.gpuManager.Available()
	}, 2*time.Second, time.Millisecond)
	metadata, err := driver.InspectSandbox(context.Background(), identity.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, gpuFailedPhase, metadata.Phase)
	require.Equal(t, identity, metadata.Config.Identity)
	_, err = driver.EnsureSandbox(context.Background(), &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{Identity: identity}})
	require.ErrorContains(t, err, "failed GPU runtime")
	_, err = driver.recoverGPUInventory([]*SandboxMetadata{metadata})
	require.NoError(t, err)
	require.True(t, driver.gpuManager.Available(), "terminal metadata must not re-reserve the released device")
	require.NoError(t, driver.DeleteSandbox(context.Background(), identity.SandboxUID))
	_, err = driver.InspectSandbox(context.Background(), identity.SandboxUID)
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

func TestGPUTerminalFailureIsFencedAndResumesAfterRestart(t *testing.T) {
	driver, store, identity, root := newGPUFailureFixture(t)
	store.state = tasktypes.Status_STOPPED
	stale := identity
	stale.RuntimeInstanceID = "other-instance"
	require.Error(t, driver.RecordTerminalFailure(context.Background(), stale, "exited"))
	store.updateErr = errors.New("metadata write failed")
	require.ErrorContains(t, driver.RecordTerminalFailure(context.Background(), identity, "exited"), "metadata write failed")
	require.False(t, driver.gpuManager.Available())
	store.updateErr = nil
	require.NoError(t, driver.RecordTerminalFailure(context.Background(), identity, "exited"))
	require.Error(t, driver.CleanupTerminalFailure(context.Background(), stale))
	require.False(t, driver.gpuManager.Available())
	require.NoError(t, driver.gpuManager.Close())
	var err error
	driver.gpuManager, err = gpu.New(root, []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	metadata, err := driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	var retired bool
	require.NoError(t, driver.RecoverRuntimeResources(t.Context(), metadata, func(_ context.Context, item *SandboxMetadata) error {
		require.Equal(t, identity, item.Config.Identity)
		require.False(t, driver.gpuManager.Available())
		retired = true
		return nil
	}))
	require.True(t, retired)
	require.True(t, driver.gpuManager.Available())
	failed, err := driver.InspectSandbox(context.Background(), identity.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, gpuFailedPhase, failed.Phase)
	require.Equal(t, identity, failed.Config.Identity)
	require.Equal(t, "exited", failed.RuntimeMessage)
}

func TestGPUUnknownTaskStateDoesNotAuthorizeFailureCleanup(t *testing.T) {
	driver, store, identity, _ := newGPUFailureFixture(t)
	store.taskErr = status.Error(codes.Unavailable, "task service offline")
	_, err := driver.InspectSandbox(context.Background(), identity.SandboxUID)
	require.Error(t, err)
	require.False(t, driver.SandboxRuntimeExited(identity.SandboxUID))
	require.Error(t, driver.RecordTerminalFailure(context.Background(), identity, "exited"))
	require.False(t, driver.gpuManager.Available())
}

func TestGPUStartupRecoveryRetiresOwnerBeforeReturningDevice(t *testing.T) {
	driver, store, identity, _ := newGPUFailureFixture(t)
	store.state = tasktypes.Status_STOPPED
	ctx := t.Context()
	managed, err := driver.ListManagedSandboxes(ctx)
	require.NoError(t, err)
	retireErr := errors.New("owner retirement unavailable")
	require.ErrorIs(t, driver.RecoverRuntimeResources(ctx, managed, func(context.Context, *SandboxMetadata) error {
		return retireErr
	}), retireErr)
	require.False(t, driver.gpuManager.Available())
	failed, err := driver.InspectSandbox(ctx, identity.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, gpuFailedPhase, failed.Phase)

	require.NoError(t, driver.RecoverRuntimeResources(ctx, managed, func(context.Context, *SandboxMetadata) error { return nil }))
	replacement := identity
	replacement.RuntimeInstanceID, replacement.InstanceGeneration = "replacement", 2
	_, err = driver.gpuManager.Acquire(replacement)
	require.NoError(t, err)
	require.ErrorContains(t, driver.CleanupTerminalFailure(ctx, identity), "another runtime instance")
	require.False(t, driver.gpuManager.Available())
}
