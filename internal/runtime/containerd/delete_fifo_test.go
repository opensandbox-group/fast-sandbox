//go:build unix

package containerd

import (
	"context"
	"path/filepath"
	"testing"

	taskapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

// Loading a Task for deletion must retain the SDK's FIFO cleanup.
func TestContainerdTaskDeleteCleansFIFOs(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr := filepath.Join(dir, "sandbox-stdout"), filepath.Join(dir, "sandbox-stderr")
	for _, path := range []string{stdout, stderr} {
		require.NoError(t, unix.Mkfifo(path, 0600))
	}
	service := &fifoTaskClient{process: &tasktypes.Process{
		ID: "sandbox", Status: tasktypes.Status_STOPPED, Stdout: stdout, Stderr: stderr,
	}}
	client, err := containerd.New("", containerd.WithDefaultRuntime("io.containerd.runc.v2"), containerd.WithServices(
		containerd.WithContainerStore(&fifoContainerStore{}), containerd.WithTaskClient(service),
	))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	c, err := client.LoadContainer(ctx, "sandbox")
	require.NoError(t, err)
	task, err := (containerdDeleteContainerAdapter{container: c}).Task(ctx)
	require.NoError(t, err)
	require.NoError(t, task.Delete(ctx))

	require.NoFileExists(t, stdout)
	require.NoFileExists(t, stderr)
}

type fifoContainerStore struct{ containers.Store }

func (*fifoContainerStore) Get(_ context.Context, id string) (containers.Container, error) {
	return containers.Container{ID: id}, nil
}

type fifoTaskClient struct {
	taskapi.TasksClient
	process *tasktypes.Process
}

func (s *fifoTaskClient) Get(context.Context, *taskapi.GetRequest, ...grpc.CallOption) (*taskapi.GetResponse, error) {
	return &taskapi.GetResponse{Process: s.process}, nil
}

func (s *fifoTaskClient) Delete(context.Context, *taskapi.DeleteTaskRequest, ...grpc.CallOption) (*taskapi.DeleteResponse, error) {
	return &taskapi.DeleteResponse{}, nil
}
