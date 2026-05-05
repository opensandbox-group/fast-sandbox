package fastletcontrol

import (
	"context"
	"sync"
	"time"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/fastletpool"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Loop periodically syncs desired sandboxes with fastlets and updates claim status.
type Loop struct {
	Client        client.Client
	Registry      fastletpool.FastletRegistry
	FastletClient *api.FastletClient
	Interval      time.Duration
}

// NewLoop creates a new FastletControlLoop with a default interval.
func NewLoop(c client.Client, reg fastletpool.FastletRegistry, fastletClient *api.FastletClient) *Loop {
	return &Loop{
		Client:        c,
		Registry:      reg,
		FastletClient: fastletClient,
		Interval:      2 * time.Second,
	}
}

// Start runs the loop until the context is cancelled.
func (l *Loop) Start(ctx context.Context) {
	logger := klog.Background().WithName("fastlet-control-loop")
	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	syncInProgress := false
	var syncMu sync.Mutex

	for {
		select {
		case <-ctx.Done():
			logger.Info("fastlet control loop stopped")
			return
		case <-ticker.C:
			syncMu.Lock()
			if syncInProgress {
				syncMu.Unlock()
				logger.Info("Previous sync still in progress, skipping this tick")
				continue
			}
			syncInProgress = true
			syncMu.Unlock()

			go func() {
				defer func() {
					syncMu.Lock()
					syncInProgress = false
					syncMu.Unlock()
				}()

				start := time.Now()
				if err := l.syncOnce(ctx); err != nil {
					logger.Error(err, "fastlet control loop sync failed")
				}
				duration := time.Since(start)
				if duration > l.Interval {
					logger.Info("Sync took longer than interval", "duration", duration, "interval", l.Interval)
				}
			}()
		}
	}
}

const (
	perFastletTimeout   = 5 * time.Second
	staleFastletTimeout = 15 * time.Second
)

func (l *Loop) syncOnce(ctx context.Context) error {
	logger := klog.Background().WithName("fastlet-control-loop")

	syncCtx, cancel := context.WithTimeout(ctx, l.Interval*2)
	defer cancel()

	var podList corev1.PodList
	if err := l.Client.List(syncCtx, &podList, client.MatchingLabels{"app": "sandbox-fastlet"}); err != nil {
		return err
	}

	seenFastlets := make(map[fastletpool.FastletID]bool)

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}

		fastletID := fastletpool.FastletID(pod.Name)
		seenFastlets[fastletID] = true

		fastletCtx, fastletCancel := context.WithTimeout(syncCtx, perFastletTimeout)
		status, err := l.FastletClient.GetFastletStatus(fastletCtx, pod.Status.PodIP)
		fastletCancel()

		if err != nil {
			logger.Error(err, "Failed to probe fastlet", "pod", pod.Name, "ip", pod.Status.PodIP)
			continue
		}

		sbStatuses := make(map[string]api.SandboxStatus)
		for _, s := range status.SandboxStatuses {
			sbStatuses[s.SandboxID] = s
		}

		info := fastletpool.FastletInfo{
			ID:              fastletID,
			Namespace:       pod.Namespace,
			PodName:         pod.Name,
			PodIP:           pod.Status.PodIP,
			NodeName:        pod.Spec.NodeName,
			PoolName:        pod.Labels["fast-sandbox.io/pool"],
			Capacity:        status.Capacity,
			Images:          status.Images,
			SandboxStatuses: sbStatuses,
			LastHeartbeat:   time.Now(),
		}
		l.Registry.RegisterOrUpdate(info)
	}

	allFastlets := l.Registry.GetAllFastlets()
	for _, a := range allFastlets {
		if !seenFastlets[a.ID] {
			logger.Info("Removing stale fastlet from registry (Pod not found)", "fastlet", a.ID, "pool", a.PoolName)
			l.Registry.Remove(a.ID)
		}
	}

	cleaned := l.Registry.CleanupStaleFastlets(staleFastletTimeout)
	if cleaned > 0 {
		logger.Info("Cleaned up stale fastlets by heartbeat timeout", "count", cleaned)
	}
	return nil
}
