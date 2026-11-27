package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/template"
	"time"

	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const (
	execLocalInstanceID    = "exec-local"
	defaultExecTimeout     = 15 * time.Second
	defaultExecInitialWait = 50 * time.Millisecond
	defaultExecMaxWait     = time.Second
	defaultExecWaitDelay   = time.Second
	maxExecCapturedBytes   = 8 << 10

	execOpApply = "apply"

	execResultSuccess = "success"
	execResultError   = "error"
	execResultTimeout = "timeout"
)

// ExecTemplateData exposes only platform-trusted metadata to exec argv
// templates. Binding input is intentionally excluded to prevent command
// injection; callers receive it strictly via stdin.
type ExecTemplateData struct {
	Sandbox    ExecTemplateSandbox
	Revision   ExecTemplateRevision
	Attachment ExecTemplateAttachment
	Sensitive  ExecTemplateSensitive
}

type ExecTemplateSensitive struct {
	File string
}

type ExecTemplateSandbox struct {
	UID       string
	Name      string
	Namespace string
}

type ExecTemplateRevision struct {
	InstanceGeneration int64
	SpecGeneration     int64
	RuntimeInstanceID  string
	AttachmentID       string
	RouteGeneration    int64
}

type ExecTemplateAttachment struct {
	Restored  bool
	Recovered bool
	Network   ExecTemplateNetwork
}

type ExecTemplateNetwork struct {
	IP          string
	Gateway     string
	NetNSPath   string
	PrivateCIDR string
	HostVeth    string
}

// ExecRequest is one rendered, shell-free execve invocation.
type ExecRequest struct {
	Command []string
	Stdin   []byte
}

// ExecResult captures the bounded outcome of one execve invocation.
type ExecResult struct {
	ExitCode int
	TimedOut bool
	Stdout   string
	Stderr   string
	Err      error
}

// ExecRunner executes a rendered argv directly via execve.
type ExecRunner interface {
	Run(ctx context.Context, req ExecRequest) ExecResult
}

// OSExecRunner executes commands directly via os/exec without a shell.
type OSExecRunner struct{}

func (OSExecRunner) Run(ctx context.Context, req ExecRequest) ExecResult {
	if len(req.Command) == 0 {
		return ExecResult{ExitCode: -1, Err: errors.New("exec command is empty")}
	}
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...) //nolint:gosec // argv is operator-declared on SandboxPool and never interpolates binding.input
	// Run each helper in its own process group and bound pipe drain with
	// WaitDelay so a descendant that inherits stdout/stderr cannot hang Wait.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = defaultExecWaitDelay
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	var stdout, stderr limitedBuffer
	stdout.limit = maxExecCapturedBytes
	stderr.limit = maxExecCapturedBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Err:    err,
	}
	if err == nil {
		return result
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result
	}
	result.ExitCode = -1
	return result
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	if b.limit <= 0 {
		return total, nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return total, nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func execTemplateData(generation int64, attachment Attachment, sensitiveFile string) ExecTemplateData {
	return ExecTemplateData{
		Sandbox: ExecTemplateSandbox{
			UID:       attachment.SandboxUID,
			Name:      attachment.SandboxName,
			Namespace: attachment.Namespace,
		},
		Revision: ExecTemplateRevision{
			InstanceGeneration: attachment.InstanceGeneration,
			SpecGeneration:     generation,
			RuntimeInstanceID:  attachment.RuntimeInstanceID,
			AttachmentID:       attachment.ID,
			RouteGeneration:    attachment.RouteGeneration,
		},
		Attachment: ExecTemplateAttachment{
			Restored:  attachment.Restored,
			Recovered: attachment.Recovered,
			Network: ExecTemplateNetwork{
				IP:          attachment.IP,
				Gateway:     attachment.Gateway,
				NetNSPath:   attachment.NetNSPath,
				PrivateCIDR: attachment.PrivateCIDR,
				HostVeth:    attachment.HostVeth,
			},
		},
		Sensitive: ExecTemplateSensitive{
			File: sensitiveFile,
		},
	}
}

func argvUsesSensitive(args []string) bool {
	for _, arg := range args {
		if strings.Contains(arg, ".Sensitive") {
			return true
		}
	}
	return false
}

// materializeSensitiveFile writes create-time sensitive material to a private
// 0600 temporary file (preferring /dev/shm when available) so exec helpers like
// `curl -H @{{.Sensitive.File}}` can consume it without shell expansion, argv
// exposure, or CRD persistence.
func materializeSensitiveFile(content string) (string, func(), error) {
	dir := ""
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		dir = "/dev/shm"
	}
	file, err := os.CreateTemp(dir, "fsb-action-sensitive-*")
	if err != nil && dir != "" {
		file, err = os.CreateTemp("", "fsb-action-sensitive-*")
	}
	if err != nil {
		return "", nil, fmt.Errorf("create sensitive action file: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("chmod sensitive action file: %w", err)
	}
	if content != "" {
		if _, err := file.WriteString(content); err != nil {
			_ = file.Close()
			cleanup()
			return "", nil, fmt.Errorf("write sensitive action file: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close sensitive action file: %w", err)
	}
	return path, cleanup, nil
}

func renderExecCommand(args []string, data ExecTemplateData) ([]string, error) {
	rendered := make([]string, len(args))
	var buf bytes.Buffer
	for index, raw := range args {
		parsed, err := template.New(fmt.Sprintf("arg[%d]", index)).Option("missingkey=error").Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse exec argv[%d]: %w", index, err)
		}
		buf.Reset()
		if err := parsed.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("render exec argv[%d]: %w", index, err)
		}
		rendered[index] = buf.String()
	}
	return rendered, nil
}

func effectiveExecTimeout(cfg *apiv1alpha2.ExecActionHandler) time.Duration {
	if cfg == nil || cfg.TimeoutSeconds <= 0 {
		return defaultExecTimeout
	}
	return time.Duration(cfg.TimeoutSeconds) * time.Second
}

func execOutputSummary(result ExecResult) string {
	stderr := strings.TrimSpace(result.Stderr)
	stdout := strings.TrimSpace(result.Stdout)
	switch {
	case stderr != "" && stdout != "":
		return truncate(stderr+" | stdout: "+stdout, 512)
	case stderr != "":
		return truncate(stderr, 512)
	case stdout != "":
		return truncate(stdout, 512)
	case result.Err != nil:
		return truncate(result.Err.Error(), 512)
	default:
		return ""
	}
}

func execSkipReason(attachment Attachment) (string, string, bool) {
	if attachment.Restored {
		return "skipped on checkpoint restore", "skipped_restore", true
	}
	if attachment.Recovered {
		return "skipped on Fastlet recovery", "skipped_recovered", true
	}
	return "", "", false
}

type execInvocationSpec struct {
	state        *sandboxState
	handler      apiv1alpha2.ActionHandler
	op           string
	hookName     string
	attachment   Attachment
	argv         []string
	stdin        []byte
	hasSensitive bool
	timeout      time.Duration
	maxAttempts  int
}

func (m *Manager) runExecAction(
	ctx context.Context,
	state *sandboxState,
	handler apiv1alpha2.ActionHandler,
	op string,
	hookName string,
	generation int64,
	attachment Attachment,
	input string,
	sensitiveInput string,
	argvTemplate []string,
) error {
	cfg := handler.Exec
	sensitiveFile := ""
	if sensitiveInput != "" || argvUsesSensitive(argvTemplate) {
		path, cleanup, err := materializeSensitiveFile(sensitiveInput)
		if err != nil {
			return err
		}
		defer cleanup()
		sensitiveFile = path
	}
	argv, err := renderExecCommand(argvTemplate, execTemplateData(generation, attachment, sensitiveFile))
	if err != nil {
		klog.ErrorS(err, "render exec action command failed",
			"sandboxUID", attachment.SandboxUID, "namespace", attachment.Namespace, "sandboxName", attachment.SandboxName,
			"handler", handler.Name, "op", op, "hook", hookName)
		return err
	}
	maxAttempts := 1 + int(cfg.MaxRetries)
	spec := execInvocationSpec{
		state: state, handler: handler, op: op, hookName: hookName, attachment: attachment,
		argv: argv, stdin: []byte(input), hasSensitive: sensitiveInput != "",
		timeout: effectiveExecTimeout(cfg), maxAttempts: maxAttempts,
	}
	backoff, maxBackoff := m.execBackoffBounds()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleepWithContext(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, maxBackoff)
		} else if ctx.Err() != nil {
			return ctx.Err()
		}

		attemptErr, stop := m.runExecAttempt(ctx, &spec, attempt)
		if attemptErr == nil {
			return nil
		}
		lastErr = attemptErr
		if stop {
			return attemptErr
		}
	}
	return lastErr
}

func (m *Manager) execBackoffBounds() (time.Duration, time.Duration) {
	m.mu.RLock()
	initial, maxWait := m.execInitialBackoff, m.execMaxBackoff
	m.mu.RUnlock()
	if initial <= 0 {
		initial = defaultExecInitialWait
	}
	if maxWait <= 0 {
		maxWait = defaultExecMaxWait
	}
	return initial, maxWait
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) runExecAttempt(ctx context.Context, spec *execInvocationSpec, attempt int) (error, bool) {
	att := spec.attachment
	attemptCtx, cancel := context.WithTimeout(ctx, spec.timeout)
	klog.V(2).InfoS("fastlet action exec starting",
		"sandboxUID", att.SandboxUID, "namespace", att.Namespace, "sandboxName", att.SandboxName,
		"handler", spec.handler.Name, "op", spec.op, "hook", spec.hookName, "attempt", attempt, "maxAttempts", spec.maxAttempts, "argv", spec.argv)

	started := time.Now()
	res := m.execRunner.Run(attemptCtx, ExecRequest{
		Command: spec.argv,
		Stdin:   spec.stdin,
	})
	duration := time.Since(started)
	cancel()

	if res.Err == nil {
		klog.InfoS("fastlet action exec completed",
			"sandboxUID", att.SandboxUID, "namespace", att.Namespace, "sandboxName", att.SandboxName,
			"handler", spec.handler.Name, "op", spec.op, "hook", spec.hookName, "attempt", attempt, "exitCode", 0,
			"durationMs", duration.Milliseconds(), "outcome", execResultSuccess)
		return nil, true
	}

	// If the caller's parent context was canceled or expired, abort this
	// reconcile without tripping terminal failure or discarding sensitiveInput
	// so a subsequent reconcile with a fresh context can resume cleanly.
	if ctx.Err() != nil {
		klog.V(2).InfoS("fastlet action exec interrupted by caller context",
			"sandboxUID", att.SandboxUID, "namespace", att.Namespace, "sandboxName", att.SandboxName,
			"handler", spec.handler.Name, "op", spec.op, "hook", spec.hookName, "attempt", attempt,
			"durationMs", duration.Milliseconds(), "err", ctx.Err())
		return ctx.Err(), true
	}

	resultLabel := execResultError
	if res.TimedOut {
		resultLabel = execResultTimeout
	}
	output := ""
	if !spec.hasSensitive {
		output = execOutputSummary(res)
	}
	attemptErr := formatExecAttemptError(spec, attempt, res, output)

	terminal := attempt == spec.maxAttempts
	logExecFailure(spec, attempt, res, terminal, duration, resultLabel, output, attemptErr)
	m.recordExecFailureStatus(spec, attempt, res.ExitCode, terminal, attemptErr)

	if terminal {
		m.notifyTerminalFailure(att.SandboxUID, att, spec.handler.Name, attemptErr.Error())
		return attemptErr, true
	}
	return attemptErr, false
}

func formatExecAttemptError(spec *execInvocationSpec, attempt int, res ExecResult, output string) error {
	if res.TimedOut {
		if output == "" {
			return fmt.Errorf("exec action %s timed out after %s (attempt %d/%d)", spec.handler.Name, spec.timeout, attempt, spec.maxAttempts)
		}
		return fmt.Errorf("exec action %s timed out after %s (attempt %d/%d): %s", spec.handler.Name, spec.timeout, attempt, spec.maxAttempts, output)
	}
	if output == "" {
		return fmt.Errorf("exec action %s failed (attempt %d/%d, exit=%d)", spec.handler.Name, attempt, spec.maxAttempts, res.ExitCode)
	}
	return fmt.Errorf("exec action %s failed (attempt %d/%d, exit=%d): %s", spec.handler.Name, attempt, spec.maxAttempts, res.ExitCode, output)
}

func logExecFailure(spec *execInvocationSpec, attempt int, res ExecResult, terminal bool, duration time.Duration, resultLabel, output string, attemptErr error) {
	att := spec.attachment
	if attempt == 1 || terminal {
		fields := []any{
			"sandboxUID", att.SandboxUID, "namespace", att.Namespace, "sandboxName", att.SandboxName,
			"handler", spec.handler.Name, "op", spec.op, "hook", spec.hookName, "attempt", attempt, "maxAttempts", spec.maxAttempts,
			"exitCode", res.ExitCode, "timedOut", res.TimedOut,
			"durationMs", duration.Milliseconds(), "outcome", resultLabel,
		}
		if !spec.hasSensitive && output != "" {
			fields = append(fields, "output", output)
		}
		klog.ErrorS(attemptErr, "fastlet action exec failed", fields...)
		return
	}
	fields := []any{
		"sandboxUID", att.SandboxUID, "namespace", att.Namespace, "sandboxName", att.SandboxName,
		"handler", spec.handler.Name, "op", spec.op, "hook", spec.hookName, "attempt", attempt, "maxAttempts", spec.maxAttempts,
		"exitCode", res.ExitCode, "timedOut", res.TimedOut,
		"durationMs", duration.Milliseconds(), "outcome", resultLabel,
	}
	if !spec.hasSensitive && output != "" {
		fields = append(fields, "output", output)
	}
	klog.V(2).InfoS("fastlet action exec retryable failure", fields...)
}

func allSubscribedExecHooksApplied(handler apiv1alpha2.ActionHandler, binding *bindingState) bool {
	if binding == nil {
		return true
	}
	for _, hook := range handler.Hooks {
		if binding.appliedHooks[actionapi.LifecycleHook(hook)] <= 0 {
			return false
		}
	}
	return true
}

func clearSensitiveInputLocked(state *sandboxState, handlerName string) {
	if state == nil {
		return
	}
	if binding := state.bindings[handlerName]; binding != nil {
		binding.sensitiveInput = ""
	}
	for index := range state.desired {
		if state.desired[index].Handler == handlerName {
			state.desired[index].SensitiveInput = ""
		}
	}
}

func (m *Manager) recordExecFailureStatus(spec *execInvocationSpec, attempt, exitCode int, terminal bool, attemptErr error) {
	if spec.state == nil {
		return
	}
	spec.state.mu.Lock()
	if binding := spec.state.bindings[spec.handler.Name]; binding != nil {
		switch {
		case terminal:
			binding.terminalFailed = true
			clearSensitiveInputLocked(spec.state, spec.handler.Name)
			setBindingStatus(binding, apiv1alpha2.ActionFailed, attemptErr.Error())
		case attempt < spec.maxAttempts:
			setBindingStatus(binding, apiv1alpha2.ActionApplying, fmt.Sprintf("exec attempt %d/%d failed (exit=%d), retrying", attempt, spec.maxAttempts, exitCode))
		default:
			setBindingStatus(binding, apiv1alpha2.ActionFailed, attemptErr.Error())
		}
	}
	spec.state.mu.Unlock()
	m.notifyChanged(spec.attachment.SandboxUID)
}

func (m *Manager) setExecBinding(
	ctx context.Context,
	state *sandboxState,
	handler apiv1alpha2.ActionHandler,
	generation int64,
	attachment Attachment,
	input *string,
	sensitiveInput string,
	digest string,
	replayHooks bool,
) error {
	handlerName := handler.Name
	cfg := handler.Exec

	// Removal of an exec binding is a local no-op.
	if input == nil {
		return nil
	}

	state.mu.Lock()
	binding := state.bindings[handlerName]
	if binding == nil {
		binding = &bindingState{status: fastletapi.ActionBindingStatus{Handler: handlerName}, appliedHooks: make(map[actionapi.LifecycleHook]int64)}
		state.bindings[handlerName] = binding
	}
	// Exec ActionBindings are create-only and immutable for a live attachment.
	// Reject input changes instead of overwriting AppliedInputDigest, while
	// still allowing SpecGeneration to advance when an unrelated HTTP binding
	// updates.
	if binding.attachmentID == attachment.ID && binding.digest != "" {
		if binding.digest != digest {
			err := fmt.Errorf("exec Action Binding %q is immutable after creation", handlerName)
			setBindingStatus(binding, apiv1alpha2.ActionFailed, err.Error())
			state.mu.Unlock()
			return err
		}
		binding.status.ObservedSpecGeneration = generation
		binding.appliedGeneration = generation
		if sensitiveInput != "" && binding.status.State != string(apiv1alpha2.ActionReady) {
			binding.sensitiveInput = sensitiveInput
		}
		m.refreshBindingStatusLocked(state, handlerName, binding)
		state.mu.Unlock()
		return nil
	}
	binding.status.ObservedSpecGeneration = generation
	binding.status.DesiredInputDigest = digest

	// Hook-subscribed exec handlers defer command execution until their
	// subscribed lifecycle hook fires so binding order (e.g., [egress, runtime-init])
	// is preserved at the hook boundary.
	if len(handler.Hooks) > 0 {
		binding.input = *input
		binding.sensitiveInput = sensitiveInput
		binding.digest = digest
		binding.appliedGeneration = generation
		binding.attachmentID = attachment.ID
		binding.handlerInstanceID = execLocalInstanceID
		binding.status.AppliedInputDigest = digest
		if replayHooks {
			binding.appliedHooks = make(map[actionapi.LifecycleHook]int64)
			binding.readyMessage = ""
		}
		m.refreshBindingStatusLocked(state, handlerName, binding)
		state.mu.Unlock()
		return nil
	}

	if skipMsg, skipOutcome, skip := execSkipReason(attachment); skip {
		binding.input = *input
		clearSensitiveInputLocked(state, handlerName)
		binding.digest = digest
		binding.appliedGeneration = generation
		binding.attachmentID = attachment.ID
		binding.handlerInstanceID = execLocalInstanceID
		binding.status.AppliedInputDigest = digest
		binding.readyMessage = skipMsg
		m.refreshBindingStatusLocked(state, handlerName, binding)
		state.mu.Unlock()
		klog.InfoS("fastlet action exec skipped",
			"sandboxUID", attachment.SandboxUID, "namespace", attachment.Namespace, "sandboxName", attachment.SandboxName,
			"handler", handlerName, "op", execOpApply, "outcome", skipOutcome)
		return nil
	}

	setBindingStatus(binding, apiv1alpha2.ActionApplying, "")
	binding.attachmentID = attachment.ID
	state.mu.Unlock()

	if err := m.runExecAction(ctx, state, handler, execOpApply, "", generation, attachment, *input, sensitiveInput, cfg.Command); err != nil {
		return err
	}

	state.mu.Lock()
	if state.terminating || state.desiredGeneration != generation || state.attachment.ID != attachment.ID {
		setBindingStatus(binding, apiv1alpha2.ActionPending, "SetBinding result belongs to an obsolete Sandbox revision")
		state.mu.Unlock()
		return errors.New("SetBinding result belongs to an obsolete Sandbox revision")
	}
	binding.input = *input
	clearSensitiveInputLocked(state, handlerName)
	binding.digest = digest
	binding.appliedGeneration = generation
	binding.attachmentID = attachment.ID
	binding.handlerInstanceID = execLocalInstanceID
	binding.status.AppliedInputDigest = digest
	binding.readyMessage = ""
	m.refreshBindingStatusLocked(state, handlerName, binding)
	state.mu.Unlock()
	return nil
}

func (m *Manager) invokeExecHook(
	ctx context.Context,
	state *sandboxState,
	handler apiv1alpha2.ActionHandler,
	generation int64,
	attachment Attachment,
	checkpoint HookCheckpoint,
) error {
	handlerName := handler.Name
	cfg := handler.Exec

	state.mu.Lock()
	binding := state.bindings[handlerName]
	if binding == nil || binding.appliedGeneration != generation || binding.handlerInstanceID != execLocalInstanceID {
		state.mu.Unlock()
		return errors.New("SetBinding is not current for lifecycle Hook")
	}
	if skipMsg, skipOutcome, skip := execSkipReason(attachment); skip {
		binding.appliedHooks[checkpoint.Name] = checkpoint.Sequence
		if allSubscribedExecHooksApplied(handler, binding) {
			clearSensitiveInputLocked(state, handlerName)
		}
		binding.readyMessage = skipMsg
		m.refreshBindingStatusLocked(state, handlerName, binding)
		state.mu.Unlock()
		klog.InfoS("fastlet action exec skipped",
			"sandboxUID", attachment.SandboxUID, "namespace", attachment.Namespace, "sandboxName", attachment.SandboxName,
			"handler", handlerName, "op", execOpApply, "hook", string(checkpoint.Name), "outcome", skipOutcome)
		return nil
	}
	bindingInput := binding.input
	sensitiveInput := binding.sensitiveInput
	setBindingStatus(binding, apiv1alpha2.ActionApplying, "")
	state.mu.Unlock()

	if err := m.runExecAction(ctx, state, handler, execOpApply, string(checkpoint.Name), generation, attachment, bindingInput, sensitiveInput, cfg.Command); err != nil {
		return err
	}

	state.mu.Lock()
	if state.terminating || state.desiredGeneration != generation || state.attachment.ID != attachment.ID {
		setBindingStatus(binding, apiv1alpha2.ActionPending, "Lifecycle Hook result belongs to an obsolete Sandbox revision")
		state.mu.Unlock()
		return errors.New("Lifecycle Hook result belongs to an obsolete Sandbox revision")
	}
	binding.appliedHooks[checkpoint.Name] = checkpoint.Sequence
	if allSubscribedExecHooksApplied(handler, binding) {
		clearSensitiveInputLocked(state, handlerName)
	}
	binding.readyMessage = ""
	m.refreshBindingStatusLocked(state, handlerName, binding)
	state.mu.Unlock()
	return nil
}
