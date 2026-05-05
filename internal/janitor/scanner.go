package janitor

import (
	"context"
	"fmt"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (j *Janitor) Scan(ctx context.Context) {
	klog.InfoS("Starting periodic containerd scan with CRD reconciliation")
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	containers, err := j.ctrdClient.Containers(ctx, "labels.\"fast-sandbox.io/managed\"==\"true\"")
	if err != nil {
		klog.ErrorS(err, "Failed to list containers")
		return
	}

	for _, c := range containers {
		labelsMap, err := c.Labels(ctx)
		if err != nil {
			continue
		}

		fastletUID := labelsMap["fast-sandbox.io/fastlet-uid"]
		fastletName := labelsMap["fast-sandbox.io/fastlet-name"]
		sandboxName := labelsMap["fast-sandbox.io/sandbox-name"]
		sandboxNamespace := labelsMap["fast-sandbox.io/namespace"]
		claimUID := labelsMap["fast-sandbox.io/claim-uid"]

		if fastletUID == "" || sandboxName == "" || sandboxNamespace == "" {
			continue
		}

		info, _ := c.Info(ctx)
		timeout := j.OrphanTimeout
		if timeout == 0 {
			timeout = defaultOrphanTimeout
		}
		if time.Since(info.CreatedAt) < timeout {
			continue
		}

		shouldCleanup := false
		reason := ""

		if !j.podExists(fastletUID) {
			shouldCleanup = true
			reason = "FastletPodDisappeared"
		}

		sandboxNotFound := false
		if !shouldCleanup {
			var sb apiv1alpha1.Sandbox
			err = j.K8sClient.Get(ctx, client.ObjectKey{Name: sandboxName, Namespace: sandboxNamespace}, &sb)
			if err != nil {
				if errors.IsNotFound(err) {
					shouldCleanup = true
					sandboxNotFound = true
					reason = "SandboxCRDNotFound"
				}
			} else {
				if claimUID != "" && string(sb.UID) != claimUID {
					shouldCleanup = true
					sandboxNotFound = true
					reason = "UIDMismatch"
				}
			}
		}

		if shouldCleanup {
			klog.InfoS("Found orphan container via CRD reconciliation",
				"container", c.ID(),
				"name", sandboxName,
				"reason", reason)
			j.queue.Add(CleanupTask{
				ContainerID:     c.ID(),
				FastletUID:      fastletUID,
				PodName:         fastletName,
				Namespace:       sandboxNamespace,
				SandboxName:     sandboxName,
				SandboxNotFound: sandboxNotFound,
			})
		}
	}
}

func (j *Janitor) podExists(uid string) bool {
	pods, err := j.podLister.List(labels.Everything())
	if err != nil {
		// Lister 失败时记录错误，返回 false 允许清理
		// 这样即使 Lister 出问题，orphan 容器也能被清理
		// 实际清理前还会再次验证 Fastlet Pod 状态
		klog.ErrorS(err, "Failed to list pods for orphan detection", "fastlet-uid", uid)
		return false
	}
	for _, p := range pods {
		if string(p.UID) == uid {
			return true
		}
	}
	return false
}

func (j *Janitor) enqueueOrphansByUID(ctx context.Context, uid string, name string, ns string) {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")

	filter := fmt.Sprintf("labels.\"fast-sandbox.io/fastlet-uid\"==\"%s\"", uid)
	containers, err := j.ctrdClient.Containers(ctx, filter)
	if err != nil {
		return
	}

	for _, c := range containers {
		klog.InfoS("Enqueuing orphan container for cleanup", "container", c.ID(), "fastlet", name)
		j.queue.Add(CleanupTask{
			ContainerID: c.ID(),
			FastletUID:  uid,
			PodName:     name,
			Namespace:   ns,
		})
	}
}
