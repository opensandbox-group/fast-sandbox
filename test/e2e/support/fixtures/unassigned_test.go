package fixtures

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

type unassignedObservationClient struct {
	client.Client
	get func(context.Context, *apiv1alpha2.Sandbox) error
}

func (c *unassignedObservationClient) Get(ctx context.Context, _ client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	return c.get(ctx, object.(*apiv1alpha2.Sandbox))
}

func TestUnassignedObservationAllowsRateLimitWaitPastWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := 0
	fixture := New(&unassignedObservationClient{get: func(requestCtx context.Context, _ *apiv1alpha2.Sandbox) error {
		calls++
		if calls == 1 {
			limiter := rate.NewLimiter(20, 1)
			if !limiter.Allow() {
				t.Fatal("expected the initial token")
			}
			return limiter.Wait(requestCtx)
		}
		return nil
	}}, WithPollInterval(time.Millisecond))
	if err := fixture.EnsureSandboxRemainsUnassigned(ctx, types.NamespacedName{Name: "pending"}, 10*time.Millisecond); err != nil {
		t.Fatalf("rate-limit wait must use the caller deadline: %v", err)
	}
	if calls < 2 {
		t.Fatal("a delayed response must be followed by a read after the observation window")
	}
}

func TestUnassignedObservationChecksFinalSample(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		name := "unassigned"
		if assigned {
			name = "assigned"
		}
		t.Run(name, func(t *testing.T) {
			const duration = 10 * time.Millisecond
			started := time.Now()
			calls := 0
			fixture := New(&unassignedObservationClient{get: func(_ context.Context, sandbox *apiv1alpha2.Sandbox) error {
				calls++
				if calls == 1 {
					time.Sleep(2 * duration)
				} else if assigned {
					sandbox.Status.Placement.FastletName = "unexpected-fastlet"
				}
				return nil
			}})
			err := fixture.EnsureSandboxRemainsUnassigned(t.Context(), types.NamespacedName{Name: "pending"}, duration)
			if assigned {
				if err == nil || !strings.Contains(err.Error(), "assigned unexpectedly") {
					t.Fatalf("final assignment must fail the observation: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unassigned observation failed: %v", err)
			}
			if calls != 2 || time.Since(started) < duration {
				t.Fatalf("expected a complete window and a final sample, got %d reads", calls)
			}
		})
	}
}

func TestUnassignedObservationPropagatesErrors(t *testing.T) {
	apiError := errors.New("API read failed")
	for _, test := range []struct {
		name string
		want error
	}{
		{name: "api", want: apiError},
		{name: "parent-deadline", want: context.DeadlineExceeded},
		{name: "parent-cancel", want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			fixture := New(&unassignedObservationClient{get: func(requestCtx context.Context, _ *apiv1alpha2.Sandbox) error {
				if test.want == apiError {
					return apiError
				}
				if test.want == context.Canceled {
					cancel()
				}
				<-requestCtx.Done()
				return requestCtx.Err()
			}})
			if err := fixture.EnsureSandboxRemainsUnassigned(ctx, types.NamespacedName{Name: "pending"}, time.Second); !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
		})
	}
}
