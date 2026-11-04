package fixtures

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
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

func (f *FixtureClient) CreateSandbox(ctx context.Context, namespace string, sandbox *apiv1alpha2.Sandbox) (*apiv1alpha2.Sandbox, error) {
	if sandbox.Namespace == "" {
		sandbox.Namespace = namespace
	}
	if err := f.client.Create(ctx, sandbox); err != nil {
		return nil, err
	}
	return sandbox, nil
}

func (f *FixtureClient) WaitForSandboxRuntimeState(ctx context.Context, name types.NamespacedName, states ...apiv1alpha2.RuntimeState) (*apiv1alpha2.Sandbox, error) {
	allowed := make(map[apiv1alpha2.RuntimeState]struct{}, len(states))
	for _, state := range states {
		allowed[state] = struct{}{}
	}
	return f.WaitForSandbox(ctx, name, func(sandbox *apiv1alpha2.Sandbox) bool {
		_, ok := allowed[sandbox.Status.Runtime.State]
		return ok
	})
}

func (f *FixtureClient) WaitForSandbox(ctx context.Context, name types.NamespacedName, predicate func(*apiv1alpha2.Sandbox) bool) (*apiv1alpha2.Sandbox, error) {
	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		sandbox := &apiv1alpha2.Sandbox{}
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
	until := time.Now().Add(duration)
	observation := time.NewTimer(duration)
	defer observation.Stop()

	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The observation window must not become the API request deadline:
		// client-go can reject a rate-limit wait before that deadline expires.
		// Finish with a successful read started after the observation window.
		finalSample := !time.Now().Before(until)
		sandbox := &apiv1alpha2.Sandbox{}
		if err := f.client.Get(ctx, name, sandbox); err != nil {
			return err
		}
		if sandbox.Status.Placement.FastletName != "" {
			return fmt.Errorf("sandbox %s/%s was assigned unexpectedly", name.Namespace, name.Name)
		}
		if finalSample {
			return ctx.Err()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-observation.C:
		case <-ticker.C:
		}
	}
}

func (f *FixtureClient) WaitForSandboxDeleted(ctx context.Context, name types.NamespacedName) error {
	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()

	for {
		sandbox := &apiv1alpha2.Sandbox{}
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
