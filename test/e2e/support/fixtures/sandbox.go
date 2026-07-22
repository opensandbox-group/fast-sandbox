package fixtures

import (
	"context"
	"fmt"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultPollInterval = 100 * time.Millisecond

type Option func(*FixtureClient)

type FixtureClient struct {
	client       client.Client
	pollInterval time.Duration
}

func New(kubeClient client.Client, opts ...Option) *FixtureClient {
	fixture := &FixtureClient{
		client:       kubeClient,
		pollInterval: defaultPollInterval,
	}
	for _, opt := range opts {
		opt(fixture)
	}
	if fixture.pollInterval <= 0 {
		fixture.pollInterval = defaultPollInterval
	}
	return fixture
}

func WithPollInterval(interval time.Duration) Option {
	return func(fixture *FixtureClient) {
		fixture.pollInterval = interval
	}
}

func (f *FixtureClient) CreateSandbox(ctx context.Context, namespace string, sandbox *apiv1alpha1.Sandbox) (*apiv1alpha1.Sandbox, error) {
	if sandbox.Namespace == "" {
		sandbox.Namespace = namespace
	}
	if err := f.client.Create(ctx, sandbox); err != nil {
		return nil, err
	}
	return sandbox, nil
}

func (f *FixtureClient) WaitForSandboxRuntimeState(ctx context.Context, name types.NamespacedName, states ...apiv1alpha1.ObservedState) (*apiv1alpha1.Sandbox, error) {
	allowed := make(map[apiv1alpha1.ObservedState]struct{}, len(states))
	for _, state := range states {
		allowed[state] = struct{}{}
	}
	return f.WaitForSandbox(ctx, name, func(sandbox *apiv1alpha1.Sandbox) bool {
		_, ok := allowed[sandbox.Status.RuntimeState]
		return ok
	})
}

func (f *FixtureClient) WaitForSandbox(ctx context.Context, name types.NamespacedName, predicate func(*apiv1alpha1.Sandbox) bool) (*apiv1alpha1.Sandbox, error) {
	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		sandbox := &apiv1alpha1.Sandbox{}
		if err := f.client.Get(ctx, name, sandbox); err == nil {
			if predicate(sandbox) {
				return sandbox, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (f *FixtureClient) EnsureSandboxRemainsUnassigned(ctx context.Context, name types.NamespacedName, duration time.Duration) error {
	checkCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		sandbox := &apiv1alpha1.Sandbox{}
		if err := f.client.Get(checkCtx, name, sandbox); err != nil {
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			if checkCtx.Err() != nil {
				return checkCtx.Err()
			}
			return err
		}
		if sandbox.Status.Assignment != nil {
			return fmt.Errorf("sandbox %s/%s was assigned unexpectedly", name.Namespace, name.Name)
		}

		select {
		case <-checkCtx.Done():
			if checkCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return checkCtx.Err()
		case <-ticker.C:
		}
	}
}

func (f *FixtureClient) WaitForSandboxDeleted(ctx context.Context, name types.NamespacedName) error {
	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		sandbox := &apiv1alpha1.Sandbox{}
		err := f.client.Get(ctx, name, sandbox)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
