package fixtures

import (
	"context"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (f *FixtureClient) CreateSandboxPool(ctx context.Context, namespace string, pool *apiv1alpha1.SandboxPool) (*apiv1alpha1.SandboxPool, error) {
	if pool.Namespace == "" {
		pool.Namespace = namespace
	}
	if err := f.client.Create(ctx, pool); err != nil {
		return nil, err
	}
	return pool, nil
}

func (f *FixtureClient) WaitForReadyAgentPods(ctx context.Context, name types.NamespacedName, minReady int32) (*apiv1alpha1.SandboxPool, error) {
	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		pool := &apiv1alpha1.SandboxPool{}
		if err := f.client.Get(ctx, name, pool); err == nil {
			if pool.Status.ReadyPods >= minReady {
				return pool, nil
			}
			readyPods, err := f.countReadyAgentPods(ctx, name)
			if err == nil && readyPods >= minReady {
				return pool, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (f *FixtureClient) countReadyAgentPods(ctx context.Context, name types.NamespacedName) (int32, error) {
	podList := &corev1.PodList{}
	if err := f.client.List(ctx, podList,
		client.InNamespace(name.Namespace),
		client.MatchingLabels{"fast-sandbox.io/pool": name.Name},
	); err != nil {
		return 0, err
	}

	var ready int32
	for _, pod := range podList.Items {
		if isPodReady(pod) {
			ready++
		}
	}
	return ready, nil
}

func isPodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
