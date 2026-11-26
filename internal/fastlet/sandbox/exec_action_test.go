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

func TestCreateSandboxExecActionFailSandboxWakesWaiter(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.allocation = fastletapi.RuntimeAllocation{
		Network: fastletapi.NetworkAllocation{IP: "172.30.0.10", Gateway: "172.30.0.1", NamespacePath: "/run/netns/sb-1"},
	}

	actionManager, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{
		Name:  "runtime-init",
		Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady},
		Exec: &apiv1alpha2.ExecActionHandler{
			Stdin:                 apiv1alpha2.ExecActionStdinBindingInput,
			NonRetryableExitCodes: []int32{22},
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
	require.ErrorContains(t, createErr, "bad init")
}

func TestCreateSandboxExecActionReadySucceeds(t *testing.T) {
	runtime := newAdmissionRuntime()
	runtime.allocation = fastletapi.RuntimeAllocation{
		Network:      fastletapi.NetworkAllocation{IP: "172.30.0.11", Gateway: "172.30.0.1", NamespacePath: "/run/netns/sb-2"},
		VSockUDSPath: "/run/vsock/sb-2.sock",
	}

	var captured fastletaction.ExecRequest
	actionManager, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{
		Name:  "runtime-init",
		Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady},
		Exec: &apiv1alpha2.ExecActionHandler{
			Stdin:         apiv1alpha2.ExecActionStdinBindingInput,
			FailurePolicy: apiv1alpha2.ExecActionFailurePolicyFailSandbox,
			Command:       []string{"/usr/bin/init-helper", "{{.Attachment.Network.IP}}", "{{.Attachment.Network.NetNSPath}}", "{{.Attachment.VSock.UDSPath}}"},
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
	require.Equal(t, []string{"/usr/bin/init-helper", "172.30.0.11", "/run/netns/sb-2", "/run/vsock/sb-2.sock"}, captured.Command)
	require.Equal(t, `{"sandboxId":"sb-exec-ok"}`, string(captured.Stdin))
}
