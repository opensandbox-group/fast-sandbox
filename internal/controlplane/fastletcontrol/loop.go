package fastletcontrol

import (
	"context"
	"math/rand"
	"sync"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	corev1 "k8s.io/api/core/v1"
	clientgocache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

type HeartbeatClient interface {
	Heartbeat(ctx context.Context, fastletIP string, req *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error)
}

// Loop gets membership from the shared Kubernetes Pod informer and probes only
// those watched endpoints at a low-frequency, jittered, bounded concurrency.
// It never lists every Pod on each heartbeat cycle.
type Loop struct {
	Cache                  ctrlcache.Cache
	Registry               *placement.InMemoryRegistry
	FastletClient          HeartbeatClient
	Interval               time.Duration
	ReadinessRetryInterval time.Duration
	RequestTimeout         time.Duration
	MaxConcurrent          int
	probeSemaphore         chan struct{}
	readinessProbeMu       sync.Mutex
	readinessProbes        map[placement.FastletID]string
}

func NewLoop(cache ctrlcache.Cache, registry *placement.InMemoryRegistry, fastletClient HeartbeatClient) *Loop {
	return &Loop{
		Cache: cache, Registry: registry, FastletClient: fastletClient,
		Interval: 20 * time.Second, ReadinessRetryInterval: time.Second,
		RequestTimeout: 5 * time.Second, MaxConcurrent: 8,
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
		logger.Info("pod informer stopped before cache sync")
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

func probeCandidate(object any) (placement.FastletInfo, bool) {
	pod, ok := object.(*corev1.Pod)
	if !ok || pod.Labels["app"] != "sandbox-fastlet" {
		return placement.FastletInfo{}, false
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
		l.Registry.RemoveIfPodUID(placement.FastletID(oldPod.Name), string(oldPod.UID))
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
		l.Registry.RemoveIfPodUID(placement.FastletID(pod.Name), string(pod.UID))
	}
}

func fastletInfoFromPod(pod *corev1.Pod) placement.FastletInfo {
	return placement.FastletInfo{
		ID: placement.FastletID(pod.Name), Namespace: pod.Namespace,
		PodName: pod.Name, PodUID: string(pod.UID), PodIP: pod.Status.PodIP,
		NodeName: pod.Spec.NodeName, PoolName: pod.Labels["fast-sandbox.io/pool"],
		RuntimeName:         apiv1alpha2.RuntimeName(pod.Labels["fast-sandbox.io/runtime"]),
		RuntimeProfileHash:  pod.Annotations["fast-sandbox.io/runtime-profile-hash"],
		ResourceProfileHash: pod.Annotations["fast-sandbox.io/resource-profile-hash"],
		InfraRevision:       pod.Annotations["fast-sandbox.io/infra-revision"],
		FastletRevision:     pod.Annotations[placement.AnnotationPodTemplateHash],
		DrainRequested:      placement.PodDrainRequested(pod),
		Draining:            placement.PodDrainRequested(pod),
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
		go func(info placement.FastletInfo) {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			l.probeOne(ctx, info, timeout, 1)
		}(info)
	}
	group.Wait()
}

func (l *Loop) probeAsync(ctx context.Context, info placement.FastletInfo) {
	if !l.beginReadinessProbe(info.ID, info.PodUID) {
		return
	}
	go func() {
		defer l.endReadinessProbe(info.ID, info.PodUID)
		l.probeUntilSchedulable(ctx, info)
	}()
}

func (l *Loop) beginReadinessProbe(id placement.FastletID, podUID string) bool {
	l.readinessProbeMu.Lock()
	defer l.readinessProbeMu.Unlock()
	if l.readinessProbes == nil {
		l.readinessProbes = make(map[placement.FastletID]string)
	}
	if l.readinessProbes[id] == podUID {
		return false
	}
	l.readinessProbes[id] = podUID
	return true
}

func (l *Loop) endReadinessProbe(id placement.FastletID, podUID string) {
	l.readinessProbeMu.Lock()
	defer l.readinessProbeMu.Unlock()
	if l.readinessProbes[id] == podUID {
		delete(l.readinessProbes, id)
	}
}

// probeUntilSchedulable closes the Pod-ready/registry-ready window without
// increasing the steady-state heartbeat frequency. Fastlet Infra preparation
// is asynchronous and does not produce another Kubernetes Pod update, so a
// first heartbeat can legitimately report InfraReady=false. In that case the
// normal jittered interval is too slow for a newly ready Pool.
func (l *Loop) probeUntilSchedulable(ctx context.Context, observed placement.FastletInfo) {
	retryInterval := l.ReadinessRetryInterval
	if retryInterval <= 0 {
		retryInterval = time.Second
	}
	maxRetryInterval := l.Interval
	if maxRetryInterval <= 0 {
		maxRetryInterval = 20 * time.Second
	}
	if maxRetryInterval < retryInterval {
		maxRetryInterval = retryInterval
	}
	semaphore := l.probeSemaphore
	if semaphore == nil {
		maxConcurrent := l.MaxConcurrent
		if maxConcurrent <= 0 {
			maxConcurrent = 8
		}
		semaphore = make(chan struct{}, maxConcurrent)
	}

	for attempt := 1; ; attempt++ {
		current, exists := l.Registry.GetFastletByID(observed.ID)
		if !exists || current.PodUID != observed.PodUID || !current.PodReady || current.DrainRequested || current.Draining {
			return
		}
		if current.RuntimeReady && current.InfraReady && current.Capacity() > 0 && !current.LastHeartbeat.IsZero() {
			return
		}

		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			return
		}
		timeout := l.RequestTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		settled := l.probeOne(ctx, current, timeout, attempt)
		<-semaphore
		if settled {
			return
		}
		// Keep the event-triggered retries inside the gap before the normal
		// heartbeat interval. If readiness still has not converged, the steady
		// loop takes over instead of leaving a second polling loop behind.
		nextRetryInterval := retryInterval * 2
		if nextRetryInterval >= maxRetryInterval {
			return
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			retryInterval = nextRetryInterval
		}
	}
}

// probeOne returns true once the heartbeat carries an observation the
// registry can act on: either the Fastlet is draining (the drain settle is
// recorded and no further startup probing is useful), or it reports a
// complete readiness observation — a full Fastlet may still be ineligible,
// but positive capacity proves readiness initialization has converged and
// steady-state polling can resume.
func (l *Loop) probeOne(ctx context.Context, info placement.FastletInfo, timeout time.Duration, attempt int) bool {
	logger := klog.Background().WithName("fastlet-heartbeat-loop")
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	heartbeat, err := l.FastletClient.Heartbeat(requestContext, info.PodIP, &fastletapi.HeartbeatRequest{
		Cache: fastletapi.CacheCursor{Epoch: info.CacheEpoch, Revision: info.CacheRevision},
	})
	if err != nil {
		// A partitioned fastlet would otherwise repeat an identical
		// ErrorS every retry interval; keep the first failure loud and
		// downgrade the repeats.
		if attempt <= 1 {
			logger.Error(err, "Fastlet Heartbeat failed", "pod", info.PodName, "podUID", info.PodUID)
		} else {
			logger.V(2).Info("fastlet heartbeat still failing", "pod", info.PodName, "podUID", info.PodUID, "consecutiveFailures", attempt, "err", err)
		}
		return false
	}
	if err := l.Registry.ApplyHeartbeat(info.ID, info.PodUID, heartbeat, time.Now()); err != nil {
		logger.Error(err, "Reject Fastlet Heartbeat", "pod", info.PodName, "podUID", info.PodUID)
		return false
	}
	return heartbeat.Draining || (heartbeat.RuntimeReady && heartbeat.InfraReady && heartbeat.Admission.Capacity > 0)
}
