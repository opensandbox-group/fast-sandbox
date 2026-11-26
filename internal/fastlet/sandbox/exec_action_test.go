package sandbox

import (
	"context"
	"errors"
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
			Stdin:                 apiv1alpha2.ExecActionStdinBindingInput,
			NonRetryableExitCodes: []apiv1alpha2.ExecActionExitCode{22},
			MaxRetries:            2,
			FailurePolicy:         apiv1alpha2.ExecActionFailurePolicyFailSandbox,
			Command:               []string{"/usr/bin/curl", "http://{{.Attachment.Network.IP}}:44772/internal/init"},
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
			Stdin:         apiv1alpha2.ExecActionStdinBindingInput,
			FailurePolicy: apiv1alpha2.ExecActionFailurePolicyFailSandbox,
			Command:       []string{"/usr/bin/init-helper", "{{.Attachment.Network.IP}}", "{{.Attachment.Network.NetNSPath}}"},
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
