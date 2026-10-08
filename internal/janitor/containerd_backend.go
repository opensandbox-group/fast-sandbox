package janitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// containerdK8sNamespace is the default containerd namespace of Kubernetes-managed containers.
const containerdK8sNamespace = "k8s.io"

type ContainerdBackend struct {
	client     *containerd.Client
	fifoDir    string
	namespaces []string
}

func NewContainerdBackend(client *containerd.Client, fifoDir string, runtimeNamespaces ...string) *ContainerdBackend {
	if fifoDir == "" {
		fifoDir = "/run/containerd/fifo"
	}
	runtimeNamespaces = normalizedContainerdNamespaces(runtimeNamespaces)
	return &ContainerdBackend{client: client, fifoDir: fifoDir, namespaces: runtimeNamespaces}
}

func (*ContainerdBackend) Name() ResourceBackend { return BackendContainerd }

func (b *ContainerdBackend) Scan(ctx context.Context) ([]ResourceIdentity, error) {
	if b.client == nil {
		return nil, errors.New("containerd client is not configured")
	}
	var resources []ResourceIdentity
	for _, runtimeNamespace := range b.namespaces {
		namespaced := namespaces.WithNamespace(ctx, runtimeNamespace)
		containers, err := b.client.Containers(namespaced, "labels.\"fast-sandbox.io/managed\"==\"true\"")
		if err != nil {
			return nil, fmt.Errorf("list managed containers in namespace %s: %w", runtimeNamespace, err)
		}
		for _, container := range containers {
			resource, err := b.identity(namespaced, container, runtimeNamespace)
			if err != nil {
				return nil, fmt.Errorf("read container %s identity in namespace %s: %w", container.ID(), runtimeNamespace, err)
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (b *ContainerdBackend) Cleanup(ctx context.Context, expected ResourceIdentity) error {
	if b.client == nil {
		return errors.New("containerd client is not configured")
	}
	runtimeNamespace := expected.ContainerdNamespace
	if runtimeNamespace == "" {
		runtimeNamespace = containerdK8sNamespace
	}
	ctx = namespaces.WithNamespace(ctx, runtimeNamespace)
	container, err := b.client.LoadContainer(ctx, expected.ResourceID)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	current, err := b.identity(ctx, container, runtimeNamespace)
	if err != nil {
		return err
	}
	if !sameResourceFence(expected, current) {
		return fmt.Errorf("container identity changed before cleanup")
	}

	var result error
	task, err := container.Task(ctx, cio.Load)
	if err == nil {
		exit, waitErr := task.Wait(ctx)
		if waitErr != nil && !errdefs.IsNotFound(waitErr) {
			result = errors.Join(result, waitErr)
		}
		if killErr := task.Kill(ctx, syscall.SIGKILL); killErr != nil && !errdefs.IsNotFound(killErr) {
			result = errors.Join(result, killErr)
		}
		if waitErr == nil {
			select {
			case <-exit:
			case <-time.After(5 * time.Second):
				result = errors.Join(result, errors.New("container task exit timeout"))
			}
		}
		if _, deleteErr := task.Delete(ctx); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
			result = errors.Join(result, deleteErr)
		}
	} else if !errdefs.IsNotFound(err) {
		result = errors.Join(result, err)
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		result = errors.Join(result, err)
	}
	if err := cleanupFIFOs(b.fifoDir, expected.ResourceID); err != nil {
		result = errors.Join(result, err)
	}
	return result
}

func (b *ContainerdBackend) identity(ctx context.Context, container containerd.Container, runtimeNamespace string) (ResourceIdentity, error) {
	info, err := container.Info(ctx)
	if err != nil {
		return ResourceIdentity{}, err
	}
	labels := info.Labels
	sandboxNamespace := labels["fast-sandbox.io/claim-namespace"]
	if sandboxNamespace == "" {
		sandboxNamespace = labels["fast-sandbox.io/namespace"]
	}
	sandboxUID := labels["fast-sandbox.io/id"]
	return ResourceIdentity{
		Backend: BackendContainerd, ResourceID: container.ID(), ContainerdNamespace: runtimeNamespace, CreatedAt: info.CreatedAt,
		FastletPodUID: labels["fast-sandbox.io/fastlet-uid"], FastletPodName: labels["fast-sandbox.io/fastlet-name"],
		FastletPodNamespace: labels["fast-sandbox.io/namespace"],
		SandboxUID:          sandboxUID, SandboxName: labels["fast-sandbox.io/sandbox-name"], SandboxNamespace: sandboxNamespace,
		InstanceGeneration: parseInt64(labels["fast-sandbox.io/instance-generation"]),
		AssignmentAttempt:  parseInt64(labels["fast-sandbox.io/assignment-attempt"]),
		RouteGeneration:    parseInt64(labels["fast-sandbox.io/route-generation"]),
	}, nil
}

func cleanupFIFOs(root, resourceID string) error {
	if resourceID == "" {
		return errors.New("container ID is empty")
	}
	matches, err := filepath.Glob(filepath.Join(root, resourceID+"*"))
	if err != nil {
		return err
	}
	var result error
	for _, match := range matches {
		if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func sameResourceFence(expected, current ResourceIdentity) bool {
	return expected.Backend == current.Backend && expected.ResourceID == current.ResourceID &&
		expected.ContainerdNamespace == current.ContainerdNamespace &&
		expected.FastletPodUID == current.FastletPodUID && expected.SandboxUID == current.SandboxUID &&
		expected.InstanceGeneration == current.InstanceGeneration && expected.AssignmentAttempt == current.AssignmentAttempt &&
		expected.ResidualProcess == current.ResidualProcess
}

func normalizedContainerdNamespaces(values []string) []string {
	if len(values) == 0 {
		return []string{containerdK8sNamespace}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return []string{containerdK8sNamespace}
	}
	return result
}
