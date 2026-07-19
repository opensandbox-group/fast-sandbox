package janitor

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
)

func (j *Janitor) doCleanup(ctx context.Context, task CleanupTask) error {
	resource := task.Resource
	decision, err := j.cleanupDecision(ctx, resource)
	if err != nil {
		return fmt.Errorf("revalidate %s: %w", resource.String(), err)
	}
	if !decision.Eligible {
		klog.InfoS("Skipping resource after pre-delete revalidation", "backend", resource.Backend, "resource", resource.ResourceID, "reason", decision.Reason)
		return nil
	}
	backend := j.backend(resource.Backend)
	if backend == nil {
		return fmt.Errorf("cleanup backend %q is not configured", resource.Backend)
	}
	if err := backend.Cleanup(ctx, resource); err != nil {
		return fmt.Errorf("cleanup %s: %w", resource.String(), err)
	}
	klog.InfoS("Cleaned orphan node resource", "backend", resource.Backend, "resource", resource.ResourceID, "reason", decision.Reason)
	return nil
}

func (j *Janitor) backend(name ResourceBackend) CleanupBackend {
	for _, backend := range j.backends {
		if backend.Name() == name {
			return backend
		}
	}
	return nil
}
