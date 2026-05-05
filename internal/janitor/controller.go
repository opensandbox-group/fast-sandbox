package janitor

import (
	"context"
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
	return &Janitor{
		kubeClient:   kubeClient,
		ctrdClient:   ctrdClient,
		nodeName:     nodeName,
		queue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter(), "janitor"),
		ScanInterval: 2 * time.Minute, // 默认值
	}
}

func (j *Janitor) Run(ctx context.Context) error {
	klog.InfoS("Starting Node Janitor", "node", j.nodeName)

	// 1. 初始化 Informer
	factory := informers.NewSharedInformerFactoryWithOptions(j.kubeClient, time.Hour,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = "spec.nodeName=" + j.nodeName
		}))

	podInformer := factory.Core().V1().Pods()
	j.podLister = podInformer.Lister()

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				// 处理已经完全删除的情况
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

	// 2. 启动 Worker
	go wait.UntilWithContext(ctx, j.runWorker, time.Second)

	// 3. 启动定时扫描
	ticker := time.NewTicker(j.ScanInterval)
	defer ticker.Stop()
	// 初始扫描
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
	// 检查是否是 Fastlet Pod (通过 Label)
	if pool, ok := pod.Labels["fast-sandbox.io/pool"]; ok {
		klog.InfoS("Detected fastlet pod deletion, checking for orphans", "pod", pod.Name, "pool", pool)
		j.enqueueOrphansByUID(ctx, string(pod.UID), pod.Name, pod.Namespace)
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

	task := item.(CleanupTask)
	err := j.doCleanup(ctx, task)
	if err != nil {
		if j.queue.NumRequeues(item) < 3 {
			j.queue.AddRateLimited(item)
		} else {
			j.queue.Forget(item)
		}
		return true
	}

	j.queue.Forget(item)
	return true
}
