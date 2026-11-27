package sandbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type stubExecRunner struct {
	fn func(ctx context.Context, req fastletaction.ExecRequest) fastletaction.ExecResult
}

func (s stubExecRunner) Run(ctx context.Context, req fastletaction.ExecRequest) fastletaction.ExecResult {
	return s.fn(ctx, req)
}

func TestCreateSandboxExecActionFailSandboxWakesWaiterWithActionFailed(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.allocation = fastletapi.RuntimeAllocation{
		Network: fastletapi.NetworkAllocation{IP: "172.30.0.10", Gateway: "172.30.0.1", NamespacePath: "/run/netns/sb-1"},
	}

	actionManager, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{
		Name:  "runtime-init",
		Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady},
		Exec: &apiv1alpha2.ExecActionHandler{
			MaxRetries: 2,
			Command:    []string{"/usr/bin/curl", "http://{{.Attachment.Network.IP}}:44772/internal/init"},
		},
	}}, nil)
	require.NoError(t, err)
	actionManager.SetExecBackoff(time.Millisecond, 2*time.Millisecond)
	actionManager.SetExecRunner(stubExecRunner{
		fn: func(_ context.Context, _ fastletaction.ExecRequest) fastletaction.ExecResult {
			return fastletaction.ExecResult{ExitCode: 22, Stdout: `{"error":"bad init"}`, Err: errors.New("exit status 22")}
		},
	})

	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 2, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, createErr := manager.CreateSandbox(ctx, &fastletapi.CreateSandboxRequest{
		RequestID:      "req-fail-sandbox",
		SpecGeneration: 1,
		Completion:     fastletapi.CreateCompletionReady,
		Identity: fastletapi.SandboxIdentity{
			SandboxUID: "sb-exec-fail", Namespace: "default", Name: "sb-exec-fail",
			FastletPodUID: "pod-uid-a", InstanceGeneration: 1, AssignmentAttempt: 1, RuntimeInstanceID: "rt-1",
		},
		Sandbox: fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
		ActionBindings: []fastletapi.ActionBindingInput{
			{Handler: "runtime-init", Input: `{"sandboxId":"sb-exec-fail"}`},
		},
	})
	require.Error(t, createErr)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, createErr, &failure)
	require.Equal(t, fastletapi.ErrorActionFailed, failure.Code)
	require.False(t, failure.Retryable)
	require.Contains(t, failure.Message, "bad init")

	// Verify cleanup transitions to RuntimeFailed after DeleteSandbox finishes.
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		meta := manager.sandboxes["sb-exec-fail"]
		return meta != nil && meta.Phase == sandboxStateRuntimeFailed
	}, 2*time.Second, 10*time.Millisecond)
}

func TestCreateSandboxExecActionReadySucceeds(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.allocation = fastletapi.RuntimeAllocation{
		Network: fastletapi.NetworkAllocation{IP: "172.30.0.11", Gateway: "172.30.0.1", NamespacePath: "/run/netns/sb-2"},
	}

	var captured fastletaction.ExecRequest
	actionManager, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{
		Name:  "runtime-init",
		Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady},
		Exec: &apiv1alpha2.ExecActionHandler{
			Command: []string{"/usr/bin/init-helper", "{{.Attachment.Network.IP}}", "{{.Attachment.Network.NetNSPath}}"},
		},
	}}, nil)
	require.NoError(t, err)
	actionManager.SetExecRunner(stubExecRunner{
		fn: func(_ context.Context, req fastletaction.ExecRequest) fastletaction.ExecResult {
			captured = req
			return fastletaction.ExecResult{ExitCode: 0}
		},
	})

	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 2, FastletPodUID: "pod-uid-a", ActionManager: actionManager,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, createErr := manager.CreateSandbox(ctx, &fastletapi.CreateSandboxRequest{
		RequestID:      "req-ok",
		SpecGeneration: 1,
		Completion:     fastletapi.CreateCompletionReady,
		Identity: fastletapi.SandboxIdentity{
			SandboxUID: "sb-exec-ok", Namespace: "default", Name: "sb-exec-ok",
			FastletPodUID: "pod-uid-a", InstanceGeneration: 1, AssignmentAttempt: 1, RuntimeInstanceID: "rt-2",
		},
		Sandbox: fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
		ActionBindings: []fastletapi.ActionBindingInput{
			{Handler: "runtime-init", Input: `{"sandboxId":"sb-exec-ok"}`},
		},
	})
	require.NoError(t, createErr)
	require.NotNil(t, resp.Sandbox)
	require.Equal(t, []string{"/usr/bin/init-helper", "172.30.0.11", "/run/netns/sb-2"}, captured.Command)
	require.Equal(t, `{"sandboxId":"sb-exec-ok"}`, string(captured.Stdin))
}

func TestWaitUntilSandboxReadyPreservesRuntimeFailureCode(t *testing.T) {
	runtime := newAdmissionRuntime()
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 2, FastletPodUID: "pod-uid-a",
	})
	require.NoError(t, err)

	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sb-vmm-exit", Namespace: "default", Name: "sb-vmm-exit",
		FastletPodUID: "pod-uid-a", InstanceGeneration: 1, AssignmentAttempt: 1, RuntimeInstanceID: "rt-vmm",
	}
	manager.mu.Lock()
	manager.sandboxes[identity.SandboxUID] = &SandboxMetadata{
		Config: fastletapi.RuntimeSandboxConfig{Identity: identity},
		Phase:  sandboxStateRuntimeFailureCleanup,
	}
	manager.runtimeMessages[identity.SandboxUID] = "vmm exited unexpectedly"
	manager.mu.Unlock()

	_, waitErr := manager.waitUntilSandboxReady(context.Background(), identity, 1)
	require.Error(t, waitErr)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, waitErr, &failure)
	require.Equal(t, fastletapi.ErrorRuntimeUnavailable, failure.Code)
	require.Contains(t, failure.Message, "vmm exited unexpectedly")
}

type failingRemoveRoutePublisher struct {
	failRemove atomic.Bool
}

func (p *failingRemoveRoutePublisher) ApplyRoute(context.Context, RoutePublication) error { return nil }
func (p *failingRemoveRoutePublisher) ReconcileRoutes(context.Context, []RoutePublication) error {
	return nil
}
func (p *failingRemoveRoutePublisher) RemoveRoute(context.Context, RoutePublication) error {
	if p.failRemove.Load() {
		return errors.New("proxy control socket unavailable")
	}
	return nil
}

func TestReclaimFailedActionSandboxRetainsSlotWhenRemoveRouteFails(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &failingRemoveRoutePublisher{}
	publisher.failRemove.Store(true)

	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 2, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)

	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sb-route-fail", Namespace: "default", Name: "sb-route-fail",
		FastletPodUID: "pod-uid-a", InstanceGeneration: 1, AssignmentAttempt: 1, RuntimeInstanceID: "rt-rf", RouteGeneration: 1,
	}
	meta := &SandboxMetadata{
		Config: fastletapi.RuntimeSandboxConfig{
			Identity: identity,
			Desired:  fastletapi.SandboxDesiredSpec{Image: "alpine:latest"},
		},
		Phase:                sandboxStateRuntimeFailureCleanup,
		ActionTerminalFailed: true,
	}
	runtime.sandboxes[identity.SandboxUID] = meta
	manager.mu.Lock()
	manager.sandboxes[identity.SandboxUID] = meta
	manager.terminalFailureWorkers[identity.SandboxUID] = meta
	manager.mu.Unlock()

	// When RemoveRoute fails, DeleteSandbox must NOT run and phase stays in cleanup.
	manager.reclaimFailedActionSandbox(identity.SandboxUID, meta)
	runtime.mu.Lock()
	_, stillInRuntime := runtime.sandboxes[identity.SandboxUID]
	runtime.mu.Unlock()
	require.True(t, stillInRuntime, "runtime slot must be retained while route withdrawal fails")
	manager.mu.RLock()
	require.Equal(t, sandboxStateRuntimeFailureCleanup, meta.Phase)
	manager.mu.RUnlock()

	// Once RemoveRoute recovers, retry via startTerminalFailureCleanup reclaims runtime.
	publisher.failRemove.Store(false)
	manager.startTerminalFailureCleanup(identity.SandboxUID, meta)
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return meta.Phase == sandboxStateRuntimeFailed
	}, 2*time.Second, 10*time.Millisecond)
	runtime.mu.Lock()
	_, stillInRuntime = runtime.sandboxes[identity.SandboxUID]
	runtime.mu.Unlock()
	require.False(t, stillInRuntime)
}
