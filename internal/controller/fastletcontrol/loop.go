package fastletcontrol

import (
	"context"
	"math/rand"
	"sync"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/fastletpool"

	corev1 "k8s.io/api/core/v1"
	clientgocache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

type HeartbeatClient interface {
	Heartbeat(ctx context.Context, fastletIP string, req *api.HeartbeatRequest) (*api.HeartbeatResponse, error)
}

// Loop gets membership from the shared Kubernetes Pod informer and probes only
// those watched endpoints at a low-frequency, jittered, bounded concurrency.
// It never lists every Pod on each heartbeat cycle.
type Loop struct {
	Cache          ctrlcache.Cache
	Registry       *fastletpool.InMemoryRegistry
	FastletClient  HeartbeatClient
	Interval       time.Duration
	RequestTimeout time.Duration
	MaxConcurrent  int
	probeSemaphore chan struct{}
}

func NewLoop(cache ctrlcache.Cache, registry *fastletpool.InMemoryRegistry, fastletClient HeartbeatClient) *Loop {
	return &Loop{
		Cache: cache, Registry: registry, FastletClient: fastletClient,
		Interval: 20 * time.Second, RequestTimeout: 5 * time.Second, MaxConcurrent: 8,
	}
}

func (l *Loop) Start(ctx context.Context) {
	logger := klog.Background().WithName("fastlet-heartbeat-loop")
	if l.Cache == nil {
		logger.Error(nil, "Pod informer cache is required")
		return
	}
	informer, err := l.Cache.GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		logger.Error(err, "Get Fastlet Pod informer")
		return
	}
	maxConcurrent := l.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	l.probeSemaphore = make(chan struct{}, maxConcurrent)
	if _, err := informer.AddEventHandler(clientgocache.ResourceEventHandlerFuncs{
		AddFunc: func(object any) {
			l.onPodAdd(object)
			if info, ok := probeCandidate(object); ok {
				l.probeAsync(ctx, info)
			}
		},
		UpdateFunc: func(previous, current any) {
			l.onPodUpdate(previous, current)
			if shouldProbeUpdate(previous, current) {
				if info, ok := probeCandidate(current); ok {
					l.probeAsync(ctx, info)
				}
			}
		},
		DeleteFunc: l.onPodDelete,
	}); err != nil {
		logger.Error(err, "Register Fastlet Pod informer handler")
		return
	}
	if !l.Cache.WaitForCacheSync(ctx) {
		logger.Info("Pod informer stopped before cache sync")
		return
	}

	l.Registry.SetStaleAfter(3 * l.Interval)
	for {
		timer := time.NewTimer(l.nextInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			l.syncOnce(ctx)
		}
	}
}

func probeCandidate(object any) (fastletpool.FastletInfo, bool) {
	pod, ok := object.(*corev1.Pod)
	if !ok || pod.Labels["app"] != "sandbox-fastlet" {
		return fastletpool.FastletInfo{}, false
	}
	info := fastletInfoFromPod(pod)
	return info, info.PodReady
}

func shouldProbeUpdate(previous, current any) bool {
	oldPod, oldOK := previous.(*corev1.Pod)
	newPod, newOK := current.(*corev1.Pod)
	if !newOK || newPod.Labels["app"] != "sandbox-fastlet" {
		return false
	}
	newInfo := fastletInfoFromPod(newPod)
	if !newInfo.PodReady {
		return false
	}
	if !oldOK || oldPod.Labels["app"] != "sandbox-fastlet" {
		return true
	}
	oldInfo := fastletInfoFromPod(oldPod)
	return !oldInfo.PodReady || oldInfo.PodUID != newInfo.PodUID || oldInfo.PodIP != newInfo.PodIP
}

func (l *Loop) onPodUpdate(previous, current any) {
	oldPod, oldOK := previous.(*corev1.Pod)
	newPod, newOK := current.(*corev1.Pod)
	if oldOK && oldPod.Labels["app"] == "sandbox-fastlet" &&
		(!newOK || newPod.Labels["app"] != "sandbox-fastlet" || newPod.Name != oldPod.Name || newPod.UID != oldPod.UID) {
		l.Registry.RemoveIfPodUID(fastletpool.FastletID(oldPod.Name), string(oldPod.UID))
	}
	if newOK {
		l.onPodAdd(newPod)
	}
}

func (l *Loop) nextInterval() time.Duration {
	if l.Interval <= 0 {
		return 20 * time.Second
	}
	return time.Duration(float64(l.Interval) * (0.8 + rand.Float64()*0.4))
}

func (l *Loop) onPodAdd(object any) {
	pod, ok := object.(*corev1.Pod)
	if !ok || pod.Labels["app"] != "sandbox-fastlet" {
		return
	}
	l.Registry.UpsertPod(fastletInfoFromPod(pod))
}

func (l *Loop) onPodDelete(object any) {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		if tombstone, tombstoneOK := object.(clientgocache.DeletedFinalStateUnknown); tombstoneOK {
			pod, ok = tombstone.Obj.(*corev1.Pod)
		}
	}
	if ok && pod.Labels["app"] == "sandbox-fastlet" {
		l.Registry.RemoveIfPodUID(fastletpool.FastletID(pod.Name), string(pod.UID))
	}
}

func fastletInfoFromPod(pod *corev1.Pod) fastletpool.FastletInfo {
	return fastletpool.FastletInfo{
		ID: fastletpool.FastletID(pod.Name), Namespace: pod.Namespace,
		PodName: pod.Name, PodUID: string(pod.UID), PodIP: pod.Status.PodIP,
		NodeName: pod.Spec.NodeName, PoolName: pod.Labels["fast-sandbox.io/pool"],
		RuntimeName:         apiv1alpha1.RuntimeName(pod.Labels["fast-sandbox.io/runtime"]),
		RuntimeProfileHash:  pod.Annotations["fast-sandbox.io/runtime-profile-hash"],
		ResourceProfileHash: pod.Annotations["fast-sandbox.io/resource-profile-hash"],
		InfraProfile:        pod.Labels["fast-sandbox.io/infra-profile"],
		InfraProfileHash:    pod.Annotations["fast-sandbox.io/infra-profile-hash"],
		DrainRequested:      fastletpool.PodDrainRequested(pod),
		Draining:            fastletpool.PodDrainRequested(pod),
		PodReady:            pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" && podConditionTrue(pod.Status.Conditions, corev1.PodReady),
		PodObservedAt:       time.Now(),
	}
}

func podConditionTrue(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (l *Loop) syncOnce(ctx context.Context) {
	maxConcurrent := l.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	timeout := l.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	semaphore := l.probeSemaphore
	if semaphore == nil {
		semaphore = make(chan struct{}, maxConcurrent)
	}
	var group sync.WaitGroup
	for _, info := range l.Registry.GetAllFastlets() {
		if info.PodIP == "" {
			continue
		}
		group.Add(1)
		go func(info fastletpool.FastletInfo) {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			l.probeOne(ctx, info, timeout)
		}(info)
	}
	group.Wait()
}

func (l *Loop) probeAsync(ctx context.Context, info fastletpool.FastletInfo) {
	go func() {
		select {
		case l.probeSemaphore <- struct{}{}:
			defer func() { <-l.probeSemaphore }()
		case <-ctx.Done():
			return
		}
		timeout := l.RequestTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		l.probeOne(ctx, info, timeout)
	}()
}

func (l *Loop) probeOne(ctx context.Context, info fastletpool.FastletInfo, timeout time.Duration) {
	logger := klog.Background().WithName("fastlet-heartbeat-loop")
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	heartbeat, err := l.FastletClient.Heartbeat(requestContext, info.PodIP, &api.HeartbeatRequest{
		Cache: api.CacheCursor{Epoch: info.CacheEpoch, Revision: info.CacheRevision},
	})
	if err != nil {
		logger.Error(err, "Fastlet Heartbeat failed", "pod", info.PodName, "podUID", info.PodUID)
		return
	}
	if err := l.Registry.ApplyHeartbeat(info.ID, info.PodUID, heartbeat, time.Now()); err != nil {
		logger.Error(err, "Reject Fastlet Heartbeat", "pod", info.PodName, "podUID", info.PodUID)
	}
}
