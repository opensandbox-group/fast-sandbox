package janitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func NewJanitor(kubeClient kubernetes.Interface, ctrdClient *containerd.Client, nodeName string) *Janitor {
	janitor := &Janitor{
		kubeClient:   kubeClient,
		nodeName:     nodeName,
		queue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter(), "janitor"),
		ScanInterval: 2 * time.Minute,
	}
	if ctrdClient != nil {
		janitor.AddBackend(NewContainerdBackend(ctrdClient, "/run/containerd/fifo", "k8s.io"))
	}
	return janitor
}

func (j *Janitor) Run(ctx context.Context) error {
	klog.InfoS("starting Node Janitor", "node", j.nodeName)

	factory := informers.NewSharedInformerFactoryWithOptions(j.kubeClient, time.Hour,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = "spec.nodeName=" + j.nodeName
		}))

	podInformer := factory.Core().V1().Pods()
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			j.handlePodDeletion(ctx, pod)
		},
	})

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync informer cache")
	}
	defer j.queue.ShutDown()

	go wait.UntilWithContext(ctx, j.runWorker, time.Second)

	if j.ScanInterval <= 0 {
		j.ScanInterval = 2 * time.Minute
	}
	ticker := time.NewTicker(j.ScanInterval)
	defer ticker.Stop()
	// Scan once at startup instead of waiting a full interval.
	j.Scan(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			j.Scan(ctx)
		}
	}
}

func (j *Janitor) handlePodDeletion(ctx context.Context, pod *corev1.Pod) {
	// Only Fastlet Pods carry the pool label; other Pod deletions never
	// own node-local runtime resources.
	if pool, ok := pod.Labels["fast-sandbox.io/pool"]; ok {
		klog.InfoS("detected Fastlet Pod deletion, scanning node resources", "pod", pod.Name, "podUID", pod.UID, "pool", pool)
		go j.Scan(ctx)
		// The first scan can race the Sandbox reconciler's durable FastletPodLost
		// observation. Retry after the orphan grace period instead of leaving the
		// resource until the much slower periodic scan.
		go func() {
			timer := time.NewTimer(j.orphanTimeout())
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				j.Scan(ctx)
			}
		}()
	}
}

func (j *Janitor) runWorker(ctx context.Context) {
	for j.processNextItem(ctx) {
	}
}

func (j *Janitor) processNextItem(ctx context.Context) bool {
	item, shutdown := j.queue.Get()
	if shutdown {
		return false
	}
	defer j.queue.Done(item)

	task, ok := item.(CleanupTask)
	if !ok {
		klog.ErrorS(errors.New("unexpected workqueue item"), "Janitor dropped non-task queue item", "item", fmt.Sprintf("%T", item))
		j.queue.Forget(item)
		return true
	}
	err := j.doCleanup(ctx, task)
	if err != nil {
		if j.queue.NumRequeues(item) < 3 {
			klog.ErrorS(err, "Janitor cleanup failed; retrying", "backend", task.Resource.Backend, "resource", task.Resource.ResourceID, "requeues", j.queue.NumRequeues(item)+1)
			j.queue.AddRateLimited(item)
		} else {
			// Terminal drop: without this line the orphan silently
			// disappears from every signal and is never retried again.
			klog.ErrorS(err, "Janitor cleanup failed; giving up after retries", "backend", task.Resource.Backend, "resource", task.Resource.ResourceID)
			j.queue.Forget(item)
		}
		return true
	}

	j.queue.Forget(item)
	return true
}
