package janitor

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func (j *Janitor) doCleanup(ctx context.Context, task CleanupTask) error {
	klog.InfoS("Starting cleanup of orphan sandbox", "container", task.ContainerID, "fastlet", task.PodName)

	if !task.SandboxNotFound && j.verifyPodExists(ctx, task.FastletUID, task.PodName, task.Namespace) {
		klog.InfoS("Pod still exists via direct API check, aborting cleanup",
			"pod-name", task.PodName, "fastlet-uid", task.FastletUID, "namespace", task.Namespace)
		return nil
	}

	// 确保使用 k8s.io 命名空间
	ctx = namespaces.WithNamespace(ctx, "k8s.io")

	// 1. 加载容器
	c, err := j.ctrdClient.LoadContainer(ctx, task.ContainerID)
	if err != nil {
		// 如果容器不存在，认为是清理完成
		return nil
	}

	// 2. 处理任务
	t, err := c.Task(ctx, nil)
	if err == nil {
		klog.InfoS("Killing task", "container", task.ContainerID)
		t.Kill(ctx, syscall.SIGKILL)

		// 等待退出
		exitCh, err := t.Wait(ctx)
		if err == nil {
			select {
			case <-exitCh:
			case <-time.After(5 * time.Second):
				klog.InfoS("Task exit timeout, proceeding to delete", "container", task.ContainerID)
			}
		}
		t.Delete(ctx)
	}

	// 3. 删除容器 (带 Snapshot 清理)
	if err := c.Delete(ctx, client.WithSnapshotCleanup); err != nil {
		klog.ErrorS(err, "Failed to delete container metadata", "container", task.ContainerID)
	}

	// 4. 清理 FIFO 文件
	j.cleanupFIFOs(task.ContainerID)

	klog.InfoS("Cleanup completed successfully", "container", task.ContainerID)
	return nil
}

func (j *Janitor) cleanupFIFOs(containerID string) {
	fifoDir := "/run/containerd/fifo"
	pattern := filepath.Join(fifoDir, containerID+"*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, m := range matches {
		os.Remove(m)
	}
}

func (j *Janitor) verifyPodExists(ctx context.Context, podUID, podName, namespace string) bool {
	if j.kubeClient == nil {
		return false
	}
	fastletPod, err := j.kubeClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if fastletPod != nil && string(fastletPod.UID) == podUID {
		if fastletPod.DeletionTimestamp != nil {
			klog.InfoS("Pod is being deleted, allowing container cleanup", "pod", podName, "namespace", namespace)
			return false
		}
		klog.InfoS("Pod exists for direct verification", "pod", podName, "namespace", namespace)
		return true
	}
	klog.ErrorS(err, "Failed to get pod for direct verification", "pod", podName, "namespace", namespace)
	return false
}
