package fixtures

import (
	"context"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
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
