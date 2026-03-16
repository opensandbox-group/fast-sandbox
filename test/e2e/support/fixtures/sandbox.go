package fixtures

import (
	"context"
	"fmt"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

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

func (f *FixtureClient) WaitForSandboxPhase(ctx context.Context, name types.NamespacedName, phases ...apiv1alpha1.SandboxPhase) (*apiv1alpha1.Sandbox, error) {
	allowed := make(map[string]struct{}, len(phases))
	for _, phase := range phases {
		allowed[string(phase)] = struct{}{}
	}

	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		sandbox := &apiv1alpha1.Sandbox{}
		if err := f.client.Get(ctx, name, sandbox); err == nil {
			if _, ok := allowed[sandbox.Status.Phase]; ok {
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
			return err
		}
		if sandbox.Status.AssignedPod != "" || sandbox.Status.SandboxID != "" {
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
