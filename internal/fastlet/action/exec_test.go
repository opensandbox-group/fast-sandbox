package action

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	actionapi "fast-sandbox/internal/protocol/action"
)

type recordingExecRunner struct {
	mu       sync.Mutex
	requests []ExecRequest
	fn       func(ctx context.Context, attempt int, req ExecRequest) ExecResult
}

func (r *recordingExecRunner) Run(ctx context.Context, req ExecRequest) ExecResult {
	r.mu.Lock()
	attempt := len(r.requests) + 1
	r.requests = append(r.requests, req)
	fn := r.fn
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx, attempt, req)
	}
	return ExecResult{}
}

func (r *recordingExecRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recordingExecRunner) last() ExecRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

func runtimeInitHandler() apiv1alpha2.ActionHandler {
	return apiv1alpha2.ActionHandler{
		Name:  "runtime-init",
		Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady},
		Exec: &apiv1alpha2.ExecActionHandler{
			Stdin:                 apiv1alpha2.ExecActionStdinBindingInput,
			SkipOnRestore:         true,
			TimeoutSeconds:        15,
			NonRetryableExitCodes: []apiv1alpha2.ExecActionExitCode{22},
			MaxRetries:            3,
			FailurePolicy:         apiv1alpha2.ExecActionFailurePolicyFailSandbox,
			Command: []string{
				"/usr/bin/curl",
				"-fsS",
				"--fail-with-body",
				"-X", "POST",
				"--data-binary", "@-",
				"http://{{.Attachment.Network.IP}}:44772/internal/init",
			},
		},
	}
}

type stubHTTPCaller struct {
	status   actionapi.HandlerStatus
	onInvoke func(int32, actionapi.Request) error
}

func (c *stubHTTPCaller) Status(context.Context, int32, string) (actionapi.HandlerStatus, error) {
	return c.status, nil
}

func (c *stubHTTPCaller) Invoke(_ context.Context, port int32, req actionapi.Request) error {
	if c.onInvoke != nil {
		return c.onInvoke(port, req)
	}
	return nil
}

func (c *stubHTTPCaller) Relay(context.Context, int32, string, actionapi.Owner, string, string, []byte) (int, []byte, error) {
	return 200, nil, nil
}

func TestExecActionOrdersAfterHTTPAndPassesInputOnStdin(t *testing.T) {
	var events []string
	var eventsMu sync.Mutex
	recordEvent := func(ev string) {
		eventsMu.Lock()
		events = append(events, ev)
		eventsMu.Unlock()
	}

	httpCaller := &stubHTTPCaller{
		status: actionapi.HandlerStatus{APIVersion: actionapi.APIVersion, InstanceID: "egress-1", Ready: true},
		onInvoke: func(_ int32, req actionapi.Request) error {
			recordEvent("http:" + string(req.Operation))
			return nil
		},
	}
	execRunner := &recordingExecRunner{
		fn: func(_ context.Context, _ int, _ ExecRequest) ExecResult {
			recordEvent("exec:runtime-init")
			return ExecResult{}
		},
	}

	handlers := []apiv1alpha2.ActionHandler{
		{Name: "egress", TargetHTTPPort: 18080, Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady}},
		runtimeInitHandler(),
	}
	manager, err := NewManager(handlers, httpCaller)
	require.NoError(t, err)
	manager.SetExecRunner(execRunner)
	manager.SetExecBackoff(time.Millisecond, 5*time.Millisecond)
	manager.probe(context.Background())

	att := attachment("sb-order")
	att.NetNSPath = "/run/netns/sb-order"
	// Shell metacharacters must pass verbatim via stdin and never reach argv.
	payload := `{"sandboxId":"sb-order","envs":{"EVIL":"$(touch /tmp/pwned); 'quote'\nline2"}}`

	_, _, err = manager.Reconcile(context.Background(), att, 1, []DesiredInput{
		{Handler: "egress", Input: `{"denyAll":true}`},
		{Handler: "runtime-init", Input: payload},
	})
	require.NoError(t, err)
	require.Equal(t, 0, execRunner.count(), "hook-subscribed exec handler must wait for its hook")

	require.NoError(t, manager.ReachHook(context.Background(), att.SandboxUID, att, actionapi.LifecycleHookDataPlaneReady, 2))
	require.Equal(t, 1, execRunner.count())
	require.Equal(t, []string{"http:SET_BINDING", "http:LIFECYCLE_HOOK", "exec:runtime-init"}, events)

	req := execRunner.last()
	require.Equal(t, "http://172.30.0.2:44772/internal/init", req.Command[len(req.Command)-1])
	require.Equal(t, payload, string(req.Stdin))

	statuses, appliedGen := manager.Statuses(att.SandboxUID)
	require.Equal(t, int64(1), appliedGen)
	require.Len(t, statuses, 2)
	require.Equal(t, string(apiv1alpha2.ActionReady), statuses[0].State)
	require.Equal(t, string(apiv1alpha2.ActionReady), statuses[1].State)

	// Relay must be rejected on exec handlers.
	_, _, relayErr := manager.RelayHandler(context.Background(), att, "runtime-init", "POST", "/x", nil)
	require.ErrorContains(t, relayErr, "does not support HTTP relay")
}

func TestExecActionImmutabilityAllowsSiblingUpdatesAndRejectsInputChange(t *testing.T) {
	httpCaller := &stubHTTPCaller{
		status: actionapi.HandlerStatus{APIVersion: actionapi.APIVersion, InstanceID: "egress-1", Ready: true},
	}
	execRunner := &recordingExecRunner{}
	handlers := []apiv1alpha2.ActionHandler{
		{Name: "egress", TargetHTTPPort: 18080, Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookDataPlaneReady}},
		runtimeInitHandler(),
	}
	manager, err := NewManager(handlers, httpCaller)
	require.NoError(t, err)
	manager.SetExecRunner(execRunner)
	manager.probe(context.Background())

	att := attachment("sb-immut")
	_, _, err = manager.Reconcile(context.Background(), att, 1, []DesiredInput{
		{Handler: "egress", Input: `{"v":1}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"sb-immut"}`},
	})
	require.NoError(t, err)
	require.NoError(t, manager.ReachHook(context.Background(), att.SandboxUID, att, actionapi.LifecycleHookDataPlaneReady, 1))
	require.Equal(t, 1, execRunner.count())

	// Updating sibling HTTP binding at generation 2 keeps exec Ready without re-running it.
	statuses, appliedGen, err := manager.Reconcile(context.Background(), att, 2, []DesiredInput{
		{Handler: "egress", Input: `{"v":2}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"sb-immut"}`},
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), appliedGen)
	require.Equal(t, string(apiv1alpha2.ActionReady), statuses[1].State)
	require.Equal(t, 1, execRunner.count())

	// Mutating the exec binding input at generation 3 must be rejected.
	_, _, err = manager.Reconcile(context.Background(), att, 3, []DesiredInput{
		{Handler: "egress", Input: `{"v":2}`},
		{Handler: "runtime-init", Input: `{"sandboxId":"mutated"}`},
	})
	require.ErrorContains(t, err, "immutable")
	require.Equal(t, 1, execRunner.count())
}

func TestExecActionRetriesUntilSuccess(t *testing.T) {
	execRunner := &recordingExecRunner{
		fn: func(_ context.Context, attempt int, _ ExecRequest) ExecResult {
			if attempt <= 2 {
				return ExecResult{ExitCode: 7, Stderr: "curl: (7) Failed to connect", Err: errors.New("exit status 7")}
			}
			return ExecResult{ExitCode: 0, Stdout: `{"status":"initialized"}`}
		},
	}

	manager, err := NewManager([]apiv1alpha2.ActionHandler{runtimeInitHandler()}, nil)
	require.NoError(t, err)
	require.True(t, manager.Ready(), "exec-only manager is immediately ready without HTTP probes")
	manager.SetExecRunner(execRunner)
	manager.SetExecBackoff(time.Millisecond, 2*time.Millisecond)

	att := attachment("sb-retry")
	_, _, err = manager.Reconcile(context.Background(), att, 1, []DesiredInput{{Handler: "runtime-init", Input: `{"sandboxId":"sb-retry"}`}})
	require.NoError(t, err)
	require.NoError(t, manager.ReachHook(context.Background(), att.SandboxUID, att, actionapi.LifecycleHookDataPlaneReady, 1))

	require.Equal(t, 3, execRunner.count())
}

func TestExecActionNonRetryableFailSandboxTriggersCallback(t *testing.T) {
	execRunner := &recordingExecRunner{
		fn: func(_ context.Context, _ int, _ ExecRequest) ExecResult {
			return ExecResult{ExitCode: 22, Stdout: `{"error":"invalid token hash"}`, Err: errors.New("exit status 22")}
		},
	}
	manager, err := NewManager([]apiv1alpha2.ActionHandler{runtimeInitHandler()}, nil)
	require.NoError(t, err)
	manager.SetExecRunner(execRunner)
	manager.SetExecBackoff(time.Millisecond, 2*time.Millisecond)

	failCh := make(chan string, 1)
	manager.SetTerminalFailureCallback(func(sandboxUID string, _ Attachment, handlerName, message string) {
		failCh <- sandboxUID + ":" + handlerName + ":" + message
	})

	att := attachment("sb-fail-22")
	_, _, err = manager.Reconcile(context.Background(), att, 1, []DesiredInput{{Handler: "runtime-init", Input: `{}`}})
	require.NoError(t, err)
	err = manager.ReachHook(context.Background(), att.SandboxUID, att, actionapi.LifecycleHookDataPlaneReady, 1)
	require.ErrorContains(t, err, "exit=22")
	require.Equal(t, 1, execRunner.count(), "non-retryable exit code must not retry")

	select {
	case msg := <-failCh:
		require.Contains(t, msg, "sb-fail-22:runtime-init:")
		require.Contains(t, msg, "invalid token hash")
	case <-time.After(2 * time.Second):
		t.Fatal("expected FailSandbox terminal failure callback")
	}

	// Subsequent converge must not re-run a terminally failed exec binding.
	_, _, err = manager.Reconcile(context.Background(), att, 1, []DesiredInput{{Handler: "runtime-init", Input: `{}`}})
	require.Error(t, err)
	require.Equal(t, 1, execRunner.count())
}

func TestExecActionSkipOnRestoreAndRecovery(t *testing.T) {
	var runs atomic.Int32
	execRunner := &recordingExecRunner{
		fn: func(_ context.Context, _ int, _ ExecRequest) ExecResult {
			runs.Add(1)
			return ExecResult{}
		},
	}

	mgr, err := NewManager([]apiv1alpha2.ActionHandler{runtimeInitHandler()}, nil)
	require.NoError(t, err)
	mgr.SetExecRunner(execRunner)

	// 1. Pause -> Resume (Restored=true) skips execution.
	restoredAtt := attachment("sb-restored")
	restoredAtt.Restored = true
	_, _, err = mgr.Reconcile(context.Background(), restoredAtt, 1, []DesiredInput{{Handler: "runtime-init", Input: `{}`}})
	require.NoError(t, err)
	require.NoError(t, mgr.ReachHook(context.Background(), restoredAtt.SandboxUID, restoredAtt, actionapi.LifecycleHookDataPlaneReady, 1))
	require.Equal(t, int32(0), runs.Load())

	statuses, _ := mgr.Statuses(restoredAtt.SandboxUID)
	require.Equal(t, string(apiv1alpha2.ActionReady), statuses[0].State)
	require.Contains(t, statuses[0].Message, "skipped on checkpoint restore")

	// 2. In-place Fastlet restart (Recovered=true) skips execution without disk state.
	recoveredAtt := attachment("sb-recovered")
	recoveredAtt.Recovered = true
	_, _, err = mgr.Reconcile(context.Background(), recoveredAtt, 1, []DesiredInput{{Handler: "runtime-init", Input: `{}`}})
	require.NoError(t, err)
	require.NoError(t, mgr.ReachHook(context.Background(), recoveredAtt.SandboxUID, recoveredAtt, actionapi.LifecycleHookDataPlaneReady, 1))
	require.Equal(t, int32(0), runs.Load())

	statuses, _ = mgr.Statuses(recoveredAtt.SandboxUID)
	require.Equal(t, string(apiv1alpha2.ActionReady), statuses[0].State)
	require.Contains(t, statuses[0].Message, "skipped on Fastlet recovery")
}

func TestOSExecRunnerDirectExecveAndProcessGroupTimeout(t *testing.T) {
	runner := OSExecRunner{}
	res := runner.Run(context.Background(), ExecRequest{
		Command: []string{"/bin/cat"},
		Stdin:   []byte(`{"hello":"$(not_executed)"}`),
	})
	require.NoError(t, res.Err)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, `{"hello":"$(not_executed)"}`, res.Stdout)

	// Verify a background descendant inheriting stdout does not hang timeout.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	timedOut := runner.Run(timeoutCtx, ExecRequest{
		Command: []string{"/bin/sh", "-c", "sleep 30 & wait"},
	})
	require.Error(t, timedOut.Err)
	require.True(t, timedOut.TimedOut)
	require.Equal(t, -1, timedOut.ExitCode)
	require.Less(t, time.Since(started), 3*time.Second)
}
