package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"k8s.io/klog/v2"
)

const (
	requestPath     = "/_fastlet/v1/actions"
	statusPath      = "/_fastlet/v1/actions/status"
	defaultTimeout  = 5 * time.Second
	maxResponseSize = 32 << 10
)

type Caller interface {
	Status(context.Context, int32) (actionapi.HandlerStatus, error)
	Invoke(context.Context, int32, actionapi.Request) error
}

type HTTPCaller struct{ client *http.Client }

func NewHTTPCaller() *HTTPCaller {
	return &HTTPCaller{client: &http.Client{
		Timeout: defaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *HTTPCaller) Status(ctx context.Context, port int32) (actionapi.HandlerStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(port, statusPath), nil)
	if err != nil {
		return actionapi.HandlerStatus{}, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return actionapi.HandlerStatus{}, err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return actionapi.HandlerStatus{}, err
	}
	if response.StatusCode != http.StatusOK {
		return actionapi.HandlerStatus{}, fmt.Errorf("action Handler returned HTTP %d: %s", response.StatusCode, truncate(strings.TrimSpace(string(body)), 512))
	}
	var result actionapi.HandlerStatus
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("decode Action Handler status: %w", err)
	}
	if result.APIVersion != actionapi.APIVersion || result.InstanceID == "" || !result.Ready {
		return result, fmt.Errorf("action Handler is not ready: %s", result.Message)
	}
	return result, nil
}

func (c *HTTPCaller) Invoke(ctx context.Context, port int32, invocation actionapi.Request) error {
	body, err := json.Marshal(invocation)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(port, requestPath), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("action Handler returned HTTP %d: %s", response.StatusCode, truncate(strings.TrimSpace(string(responseBody)), 512))
	}
	return nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseSize {
		return nil, errors.New("Action Handler response exceeds 32 KiB")
	}
	return body, nil
}

func endpoint(port int32, path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

type DesiredInput struct {
	Handler string
	Input   string
	Digest  string
}

type Attachment struct {
	ID                 string
	SandboxUID         string
	SandboxName        string
	Namespace          string
	InstanceGeneration int64
	AssignmentAttempt  int64
	RuntimeInstanceID  string
	RouteGeneration    int64
	IP                 string
	Gateway            string
	PrivateCIDR        string
	HostVeth           string
}

type HookCheckpoint struct {
	Name     actionapi.LifecycleHook
	Sequence int64
}

type bindingState struct {
	status            fastletapi.ActionBindingStatus
	input             string
	digest            string
	appliedGeneration int64
	attachmentID      string
	handlerInstanceID string
	appliedHooks      map[actionapi.LifecycleHook]int64
}

type sandboxState struct {
	op chan struct{}
	mu sync.RWMutex

	bindings          map[string]*bindingState
	order             []string
	desired           []DesiredInput
	desiredGeneration int64
	desiredSignature  string
	appliedGeneration int64
	attachment        Attachment
	reachedHooks      []HookCheckpoint
	terminating       bool
}

func newSandboxState() *sandboxState {
	state := &sandboxState{op: make(chan struct{}, 1), bindings: make(map[string]*bindingState)}
	state.op <- struct{}{}
	return state
}

func (s *sandboxState) lockOperations(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.op:
		return true
	}
}

func (s *sandboxState) unlockOperations() { s.op <- struct{}{} }

type handlerObservation struct {
	instanceID string
	err        error
}

type Manager struct {
	mu       sync.RWMutex
	handlers []apiv1alpha2.ActionHandler
	byName   map[string]apiv1alpha2.ActionHandler
	caller   Caller
	states   map[string]*sandboxState
	terminal map[string]Attachment
	observed map[string]handlerObservation
	ready    bool
	message  string

	startOnce sync.Once
	probeMu   sync.Mutex
	notifyMu  sync.RWMutex
	notify    func(string)
}

func NewManager(handlers []apiv1alpha2.ActionHandler, caller Caller) (*Manager, error) {
	spec := apiv1alpha2.SandboxPoolSpec{ActionHandlers: handlers}
	if err := spec.ValidateActionHandlers(); err != nil {
		return nil, err
	}
	if caller == nil {
		caller = NewHTTPCaller()
	}
	byName := make(map[string]apiv1alpha2.ActionHandler, len(handlers))
	for _, handler := range handlers {
		byName[handler.Name] = handler
	}
	return &Manager{
		handlers: append([]apiv1alpha2.ActionHandler(nil), handlers...),
		byName:   byName, caller: caller, states: make(map[string]*sandboxState),
		terminal: make(map[string]Attachment),
		observed: make(map[string]handlerObservation), ready: len(handlers) == 0,
	}, nil
}

func (m *Manager) SetChangeNotifier(notify func(string)) {
	m.notifyMu.Lock()
	m.notify = notify
	m.notifyMu.Unlock()
}

func (m *Manager) notifyChanged(sandboxUID string) {
	m.notifyMu.RLock()
	notify := m.notify
	m.notifyMu.RUnlock()
	if notify != nil {
		notify(sandboxUID)
	}
}

func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		if len(m.handlers) == 0 {
			return
		}
		go func() {
			m.probe(ctx)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.probe(ctx)
				}
			}
		}()
	})
}

func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

func (m *Manager) Required() bool { return len(m.handlers) > 0 }

func (m *Manager) probe(ctx context.Context) {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	type result struct {
		handler apiv1alpha2.ActionHandler
		status  actionapi.HandlerStatus
		err     error
	}
	results := make([]result, 0, len(m.handlers))
	for _, handler := range m.handlers {
		probeCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
		status, err := m.caller.Status(probeCtx, handler.TargetHTTPPort)
		cancel()
		results = append(results, result{handler: handler, status: status, err: err})
	}

	m.mu.Lock()
	ready, message := true, ""
	states := make([]*sandboxState, 0, len(m.states))
	for _, state := range m.states {
		states = append(states, state)
	}
	for _, result := range results {
		m.observed[result.handler.Name] = handlerObservation{instanceID: result.status.InstanceID, err: result.err}
		if result.err != nil {
			ready = false
			if message == "" {
				message = fmt.Sprintf("Action Handler %s: %v", result.handler.Name, result.err)
			}
		}
	}
	m.ready, m.message = ready, message
	m.mu.Unlock()

	for _, state := range states {
		changed := false
		state.mu.Lock()
		for _, result := range results {
			binding := state.bindings[result.handler.Name]
			if binding == nil {
				continue
			}
			switch {
			case result.err != nil && binding.handlerInstanceID != "":
				binding.handlerInstanceID = ""
				binding.appliedHooks = make(map[actionapi.LifecycleHook]int64)
				setBindingStatus(binding, apiv1alpha2.ActionFailed, fmt.Sprintf("Action Handler is unavailable: %v", result.err))
				changed = true
			case result.err == nil && binding.handlerInstanceID != "" && binding.handlerInstanceID != result.status.InstanceID:
				binding.handlerInstanceID = ""
				binding.appliedHooks = make(map[actionapi.LifecycleHook]int64)
				setBindingStatus(binding, apiv1alpha2.ActionPending, "Action Handler restarted; SetBinding and reached Hooks will be replayed")
				changed = true
			}
		}
		uid := state.attachment.SandboxUID
		state.mu.Unlock()
		if changed && uid != "" {
			m.notifyChanged(uid)
		}
		go m.retryState(ctx, state)
	}
}

func (m *Manager) Reconcile(ctx context.Context, attachment Attachment, specGeneration int64, desired []DesiredInput) ([]fastletapi.ActionBindingStatus, int64, error) {
	if specGeneration <= 0 || attachment.SandboxUID == "" || attachment.ID == "" || attachment.RuntimeInstanceID == "" {
		return nil, 0, errors.New("positive specGeneration, Sandbox UID, attachment ID, and runtime instance ID are required")
	}
	canonical, signature, err := m.validateDesired(desired)
	if err != nil {
		return nil, 0, err
	}
	state, err := m.getOrCreateState(attachment)
	if err != nil {
		return nil, 0, err
	}
	if err := m.registerDesired(state, attachment, specGeneration, canonical, signature); err != nil {
		return m.stateResult(state, canonical, err)
	}
	if !state.lockOperations(ctx) {
		return m.stateResult(state, canonical, ctx.Err())
	}
	defer state.unlockOperations()

	err = m.convergeLocked(ctx, state)
	statuses, generation := m.Statuses(attachment.SandboxUID)
	return statusesForDesired(statuses, canonical), generation, err
}

// RegisterDesired records a complete ordered Binding list without waiting for
// Handler I/O. Fastlet uses it before runtime creation so SetBinding can begin
// immediately while runtime and data-plane work continue independently.
func (m *Manager) RegisterDesired(attachment Attachment, specGeneration int64, desired []DesiredInput) error {
	if specGeneration <= 0 || attachment.SandboxUID == "" || attachment.ID == "" || attachment.RuntimeInstanceID == "" {
		return errors.New("positive specGeneration, Sandbox UID, attachment ID, and runtime instance ID are required")
	}
	canonical, signature, err := m.validateDesired(desired)
	if err != nil {
		return err
	}
	state, err := m.getOrCreateState(attachment)
	if err != nil {
		return err
	}
	if err := m.registerDesired(state, attachment, specGeneration, canonical, signature); err != nil {
		return err
	}
	go m.retryState(context.Background(), state)
	return nil
}

func (m *Manager) registerDesired(state *sandboxState, attachment Attachment, generation int64, desired []DesiredInput, signature string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.terminating {
		return errors.New("Action Binding state is terminating")
	}
	if state.desiredGeneration > generation ||
		(state.desiredGeneration == generation && state.desiredSignature != "" && state.desiredSignature != signature) {
		return fmt.Errorf("action Binding request is stale or conflicts at Sandbox generation %d", generation)
	}
	if state.attachment.ID != "" && state.attachment.ID != attachment.ID {
		if !attachmentAfter(attachment, state.attachment) {
			return errors.New("Action Binding request targets a stale runtime attachment")
		}
		state.reachedHooks = nil
		for _, binding := range state.bindings {
			binding.handlerInstanceID = ""
			binding.appliedHooks = make(map[actionapi.LifecycleHook]int64)
		}
	}
	state.attachment = attachment
	state.desired = appendDesired(nil, desired)
	state.desiredGeneration, state.desiredSignature = generation, signature
	return nil
}

func (m *Manager) ReachHook(ctx context.Context, sandboxUID string, attachment Attachment, hook actionapi.LifecycleHook, sequence int64) error {
	if err := m.RecordHook(sandboxUID, attachment, hook, sequence); err != nil {
		return err
	}
	state := m.getState(sandboxUID)
	if !state.lockOperations(ctx) {
		return ctx.Err()
	}
	defer state.unlockOperations()
	return m.convergeLocked(ctx, state)
}

// RecordHook persists a locally observed lifecycle checkpoint and schedules
// delivery without making runtime or data-plane progress wait on Handler I/O.
func (m *Manager) RecordHook(sandboxUID string, attachment Attachment, hook actionapi.LifecycleHook, sequence int64) error {
	if sequence <= 0 {
		return errors.New("positive lifecycle sequence is required")
	}
	if sandboxUID == "" || attachment.SandboxUID != sandboxUID || attachment.ID == "" {
		return errors.New("Sandbox UID and attachment ID are required for lifecycle Hook")
	}
	if hook != actionapi.LifecycleHookRuntimeReady && hook != actionapi.LifecycleHookDataPlaneReady {
		return fmt.Errorf("unsupported lifecycle Hook %q", hook)
	}
	state, err := m.getOrCreateState(attachment)
	if err != nil {
		return err
	}
	state.mu.Lock()
	if state.terminating {
		state.mu.Unlock()
		return errors.New("Action Binding state is terminating")
	}
	if state.attachment.ID == "" {
		state.attachment = attachment
	} else if state.attachment.ID != attachment.ID {
		state.mu.Unlock()
		return errors.New("lifecycle Hook targets a stale runtime attachment")
	}
	found := false
	for index := range state.reachedHooks {
		if state.reachedHooks[index].Name == hook {
			if sequence < state.reachedHooks[index].Sequence {
				state.mu.Unlock()
				return errors.New("lifecycle Hook sequence is stale")
			}
			state.reachedHooks[index].Sequence = sequence
			found = true
			break
		}
	}
	if !found {
		state.reachedHooks = append(state.reachedHooks, HookCheckpoint{Name: hook, Sequence: sequence})
		sort.SliceStable(state.reachedHooks, func(left, right int) bool {
			return state.reachedHooks[left].Sequence < state.reachedHooks[right].Sequence
		})
	}
	for name, binding := range state.bindings {
		if m.subscribes(name, hook) && binding.appliedHooks[hook] < sequence {
			setBindingStatus(binding, apiv1alpha2.ActionPending, fmt.Sprintf("Lifecycle Hook %s is pending", hook))
		}
	}
	state.mu.Unlock()
	m.notifyChanged(sandboxUID)
	go m.retryState(context.Background(), state)
	return nil
}

func (m *Manager) convergeLocked(ctx context.Context, state *sandboxState) error {
	state.mu.RLock()
	desired := appendDesired(nil, state.desired)
	generation := state.desiredGeneration
	attachment := state.attachment
	oldOrder := append([]string(nil), state.order...)
	state.mu.RUnlock()
	if generation <= 0 {
		return nil
	}
	desiredByName := make(map[string]DesiredInput, len(desired))
	for _, input := range desired {
		desiredByName[input.Handler] = input
	}

	for index := len(oldOrder) - 1; index >= 0; index-- {
		name := oldOrder[index]
		if _, found := desiredByName[name]; found {
			continue
		}
		if err := m.setBinding(ctx, state, name, generation, attachment, nil, "sha256:removed", false); err != nil {
			m.notifyChanged(attachment.SandboxUID)
			return fmt.Errorf("remove live Action Binding %s: %w", name, err)
		}
		state.mu.Lock()
		delete(state.bindings, name)
		state.mu.Unlock()
	}

	for _, input := range desired {
		instanceID, instanceErr := m.handlerInstance(input.Handler)
		state.mu.Lock()
		binding := state.bindings[input.Handler]
		if binding == nil {
			binding = &bindingState{status: fastletapi.ActionBindingStatus{Handler: input.Handler}, appliedHooks: make(map[actionapi.LifecycleHook]int64)}
			state.bindings[input.Handler] = binding
		}
		needsSet := binding.appliedGeneration != generation || binding.digest != input.Digest ||
			binding.attachmentID != attachment.ID || binding.handlerInstanceID != instanceID
		replayHooks := binding.handlerInstanceID == "" || binding.handlerInstanceID != instanceID || binding.attachmentID != attachment.ID
		state.mu.Unlock()
		if instanceErr != nil {
			state.mu.Lock()
			setBindingStatus(binding, apiv1alpha2.ActionFailed, instanceErr.Error())
			state.mu.Unlock()
			m.notifyChanged(attachment.SandboxUID)
			return instanceErr
		}
		if needsSet {
			value := input.Input
			if err := m.setBinding(ctx, state, input.Handler, generation, attachment, &value, input.Digest, replayHooks); err != nil {
				m.notifyChanged(attachment.SandboxUID)
				return fmt.Errorf("setBinding %s: %w", input.Handler, err)
			}
		}
	}

	state.mu.RLock()
	reached := append([]HookCheckpoint(nil), state.reachedHooks...)
	state.mu.RUnlock()
	for _, checkpoint := range reached {
		for _, input := range desired {
			if !m.subscribes(input.Handler, checkpoint.Name) {
				continue
			}
			state.mu.RLock()
			binding := state.bindings[input.Handler]
			alreadyApplied := binding != nil && binding.appliedHooks[checkpoint.Name] >= checkpoint.Sequence
			state.mu.RUnlock()
			if alreadyApplied {
				continue
			}
			if err := m.invokeHook(ctx, state, input.Handler, generation, attachment, checkpoint); err != nil {
				m.notifyChanged(attachment.SandboxUID)
				return fmt.Errorf("lifecycle Hook %s for %s: %w", checkpoint.Name, input.Handler, err)
			}
		}
	}

	state.mu.Lock()
	state.order = bindingOrder(desired)
	allReady := true
	for _, input := range desired {
		binding := state.bindings[input.Handler]
		m.refreshBindingStatusLocked(state, input.Handler, binding)
		allReady = allReady && binding.status.State == string(apiv1alpha2.ActionReady)
	}
	if allReady {
		state.appliedGeneration = generation
	}
	state.mu.Unlock()
	m.notifyChanged(attachment.SandboxUID)
	return nil
}

func (m *Manager) setBinding(ctx context.Context, state *sandboxState, handlerName string, generation int64, attachment Attachment, input *string, digest string, replayHooks bool) error {
	handler := m.byName[handlerName]
	instanceID, err := m.handlerInstance(handlerName)
	state.mu.Lock()
	binding := state.bindings[handlerName]
	if binding == nil {
		binding = &bindingState{status: fastletapi.ActionBindingStatus{Handler: handlerName}, appliedHooks: make(map[actionapi.LifecycleHook]int64)}
		state.bindings[handlerName] = binding
	}
	setBindingStatus(binding, apiv1alpha2.ActionApplying, "")
	binding.status.ObservedSpecGeneration = generation
	binding.status.DesiredInputDigest = digest
	state.mu.Unlock()
	if err != nil {
		state.mu.Lock()
		setBindingStatus(binding, apiv1alpha2.ActionFailed, err.Error())
		state.mu.Unlock()
		return err
	}
	request := buildRequest(actionapi.OperationSetBinding, stableInvocationID(actionapi.OperationSetBinding, handlerName, instanceID, generation, attachment, digest, ""), generation, attachment)
	request.Binding = &actionapi.BindingPayload{Input: input}
	if err := m.caller.Invoke(ctx, handler.TargetHTTPPort, request); err != nil {
		state.mu.Lock()
		setBindingStatus(binding, apiv1alpha2.ActionFailed, err.Error())
		state.mu.Unlock()
		return err
	}
	if current, currentErr := m.handlerInstance(handlerName); currentErr != nil || current != instanceID {
		state.mu.Lock()
		setBindingStatus(binding, apiv1alpha2.ActionPending, "Handler instance changed while SetBinding was in flight")
		state.mu.Unlock()
		return errors.New("Handler instance changed while SetBinding was in flight")
	}
	state.mu.Lock()
	if state.terminating || state.desiredGeneration != generation || state.attachment.ID != attachment.ID {
		setBindingStatus(binding, apiv1alpha2.ActionPending, "SetBinding result belongs to an obsolete Sandbox revision")
		state.mu.Unlock()
		return errors.New("SetBinding result belongs to an obsolete Sandbox revision")
	}
	binding.input = ""
	if input != nil {
		binding.input = *input
	}
	binding.digest = digest
	binding.appliedGeneration = generation
	binding.attachmentID = attachment.ID
	binding.handlerInstanceID = instanceID
	binding.status.AppliedInputDigest = digest
	if replayHooks {
		binding.appliedHooks = make(map[actionapi.LifecycleHook]int64)
	} else {
		for _, checkpoint := range state.reachedHooks {
			if m.subscribes(handlerName, checkpoint.Name) {
				binding.appliedHooks[checkpoint.Name] = checkpoint.Sequence
			}
		}
	}
	m.refreshBindingStatusLocked(state, handlerName, binding)
	state.mu.Unlock()
	return nil
}

func (m *Manager) invokeHook(ctx context.Context, state *sandboxState, handlerName string, generation int64, attachment Attachment, checkpoint HookCheckpoint) error {
	handler := m.byName[handlerName]
	instanceID, err := m.handlerInstance(handlerName)
	if err != nil {
		return err
	}
	state.mu.Lock()
	binding := state.bindings[handlerName]
	if binding == nil || binding.appliedGeneration != generation || binding.handlerInstanceID != instanceID {
		state.mu.Unlock()
		return errors.New("SetBinding is not current for lifecycle Hook")
	}
	setBindingStatus(binding, apiv1alpha2.ActionApplying, "")
	state.mu.Unlock()
	extra := fmt.Sprintf("%s/%d", checkpoint.Name, checkpoint.Sequence)
	request := buildRequest(actionapi.OperationLifecycleHook, stableInvocationID(actionapi.OperationLifecycleHook, handlerName, instanceID, generation, attachment, binding.digest, extra), generation, attachment)
	request.Hook = &actionapi.LifecycleHookPayload{Name: checkpoint.Name, Sequence: checkpoint.Sequence}
	if err := m.caller.Invoke(ctx, handler.TargetHTTPPort, request); err != nil {
		state.mu.Lock()
		setBindingStatus(binding, apiv1alpha2.ActionFailed, err.Error())
		state.mu.Unlock()
		return err
	}
	if current, currentErr := m.handlerInstance(handlerName); currentErr != nil || current != instanceID {
		state.mu.Lock()
		setBindingStatus(binding, apiv1alpha2.ActionPending, "Handler instance changed while lifecycle Hook was in flight")
		state.mu.Unlock()
		return errors.New("Handler instance changed while lifecycle Hook was in flight")
	}
	state.mu.Lock()
	if state.terminating || state.desiredGeneration != generation || state.attachment.ID != attachment.ID {
		setBindingStatus(binding, apiv1alpha2.ActionPending, "Lifecycle Hook result belongs to an obsolete Sandbox revision")
		state.mu.Unlock()
		return errors.New("Lifecycle Hook result belongs to an obsolete Sandbox revision")
	}
	binding.appliedHooks[checkpoint.Name] = checkpoint.Sequence
	m.refreshBindingStatusLocked(state, handlerName, binding)
	state.mu.Unlock()
	return nil
}

func (m *Manager) Delete(ctx context.Context, sandboxUID string) error {
	state := m.getState(sandboxUID)
	if state == nil {
		return nil
	}
	if !state.lockOperations(ctx) {
		return ctx.Err()
	}
	defer state.unlockOperations()
	state.mu.Lock()
	if state.terminating {
		state.mu.Unlock()
		return nil
	}
	state.terminating = true
	order := append([]string(nil), state.order...)
	seen := make(map[string]struct{}, len(order))
	for _, name := range order {
		seen[name] = struct{}{}
	}
	for _, desired := range state.desired {
		binding := state.bindings[desired.Handler]
		if binding != nil && binding.appliedGeneration > 0 {
			if _, found := seen[desired.Handler]; !found {
				order = append(order, desired.Handler)
				seen[desired.Handler] = struct{}{}
			}
		}
	}
	attachment, generation := state.attachment, state.desiredGeneration
	state.mu.Unlock()
	var result error
	for index := len(order) - 1; index >= 0; index-- {
		name := order[index]
		handler := m.byName[name]
		instanceID, _ := m.handlerInstance(name)
		request := buildRequest(actionapi.OperationRemoveBinding, stableInvocationID(actionapi.OperationRemoveBinding, name, instanceID, generation, attachment, "", "terminal"), generation, attachment)
		result = errors.Join(result, m.caller.Invoke(ctx, handler.TargetHTTPPort, request))
		if ctx.Err() != nil {
			result = errors.Join(result, ctx.Err())
			break
		}
	}
	m.mu.Lock()
	if m.states[sandboxUID] == state {
		delete(m.states, sandboxUID)
		current, found := m.terminal[sandboxUID]
		if !found || attachmentAfter(attachment, current) {
			m.terminal[sandboxUID] = attachment
		}
	}
	m.mu.Unlock()
	m.notifyChanged(sandboxUID)
	return result
}

func (m *Manager) Statuses(sandboxUID string) ([]fastletapi.ActionBindingStatus, int64) {
	state := m.getState(sandboxUID)
	if state == nil {
		return nil, 0
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return statusesLocked(state, state.desired), state.appliedGeneration
}

func (m *Manager) retryState(parent context.Context, state *sandboxState) {
	ctx, cancel := context.WithTimeout(parent, defaultTimeout*time.Duration(max(1, len(m.handlers))))
	defer cancel()
	if !state.lockOperations(ctx) {
		return
	}
	defer state.unlockOperations()
	state.mu.RLock()
	terminating := state.terminating
	state.mu.RUnlock()
	if !terminating {
		if convergeErr := m.convergeLocked(ctx, state); convergeErr != nil {
			// The retry loop swallows nothing else; without this line a
			// persistently failing hook converge is invisible.
			klog.V(2).InfoS("sandbox action retry converge failed", "sandbox", state.attachment.SandboxUID, "err", convergeErr)
		}
	}
}

func (m *Manager) handlerInstance(name string) (string, error) {
	m.mu.RLock()
	observation, found := m.observed[name]
	m.mu.RUnlock()
	if !found || observation.err != nil || observation.instanceID == "" {
		if found && observation.err != nil {
			return "", fmt.Errorf("action Handler %s is unavailable: %w", name, observation.err)
		}
		return "", fmt.Errorf("action Handler %s has not reported Ready", name)
	}
	return observation.instanceID, nil
}

func (m *Manager) subscribes(handlerName string, hook actionapi.LifecycleHook) bool {
	for _, configured := range m.byName[handlerName].Hooks {
		if string(configured) == string(hook) {
			return true
		}
	}
	return false
}

func (m *Manager) refreshBindingStatusLocked(state *sandboxState, handlerName string, binding *bindingState) {
	if binding == nil || binding.appliedGeneration != state.desiredGeneration || binding.attachmentID != state.attachment.ID || binding.handlerInstanceID == "" {
		if binding != nil && binding.status.State != string(apiv1alpha2.ActionFailed) {
			setBindingStatus(binding, apiv1alpha2.ActionPending, "Binding has not reached the current Sandbox revision")
		}
		return
	}
	for _, checkpoint := range state.reachedHooks {
		if m.subscribes(handlerName, checkpoint.Name) && binding.appliedHooks[checkpoint.Name] < checkpoint.Sequence {
			setBindingStatus(binding, apiv1alpha2.ActionPending, fmt.Sprintf("Lifecycle Hook %s is pending", checkpoint.Name))
			return
		}
	}
	setBindingStatus(binding, apiv1alpha2.ActionReady, "")
}

func (m *Manager) validateDesired(desired []DesiredInput) ([]DesiredInput, string, error) {
	if len(desired) > 16 {
		return nil, "", errors.New("at most 16 Action Bindings are allowed")
	}
	seen := make(map[string]struct{}, len(desired))
	result := make([]DesiredInput, 0, len(desired))
	total := 0
	hash := sha256.New()
	for _, input := range desired {
		if _, found := m.byName[input.Handler]; !found {
			return nil, "", fmt.Errorf("action Handler %q is not configured on this Fastlet", input.Handler)
		}
		if _, found := seen[input.Handler]; found {
			return nil, "", fmt.Errorf("duplicate Action Binding for Handler %q", input.Handler)
		}
		seen[input.Handler] = struct{}{}
		if len(input.Input) > apiv1alpha2.MaxActionBindingInputBytes {
			return nil, "", fmt.Errorf("action Binding %s input exceeds %d bytes", input.Handler, apiv1alpha2.MaxActionBindingInputBytes)
		}
		total += len(input.Input)
		if total > apiv1alpha2.MaxSandboxActionBindingInputBytes {
			return nil, "", fmt.Errorf("action Binding inputs exceed %d bytes", apiv1alpha2.MaxSandboxActionBindingInputBytes)
		}
		digestBytes := sha256.Sum256([]byte(input.Input))
		digest := "sha256:" + hex.EncodeToString(digestBytes[:])
		input.Digest = digest
		result = append(result, input)
		_, _ = hash.Write([]byte(input.Handler))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(digest))
		_, _ = hash.Write([]byte{0})
	}
	return result, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func (m *Manager) getOrCreateState(attachment Attachment) (*sandboxState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if terminalAttachment, found := m.terminal[attachment.SandboxUID]; found {
		if !attachmentAfter(attachment, terminalAttachment) {
			return nil, errors.New("Action Binding state is terminal for this runtime attachment")
		}
	}
	state := m.states[attachment.SandboxUID]
	if state == nil {
		state = newSandboxState()
		m.states[attachment.SandboxUID] = state
	}
	return state, nil
}

func attachmentAfter(candidate, current Attachment) bool {
	if candidate.InstanceGeneration != current.InstanceGeneration {
		return candidate.InstanceGeneration > current.InstanceGeneration
	}
	if candidate.AssignmentAttempt != current.AssignmentAttempt {
		return candidate.AssignmentAttempt > current.AssignmentAttempt
	}
	if candidate.RouteGeneration != current.RouteGeneration {
		return candidate.RouteGeneration > current.RouteGeneration
	}
	return false
}

func (m *Manager) getState(uid string) *sandboxState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.states[uid]
}

func (m *Manager) stateResult(state *sandboxState, desired []DesiredInput, err error) ([]fastletapi.ActionBindingStatus, int64, error) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return statusesLocked(state, desired), state.appliedGeneration, err
}

func statusesLocked(state *sandboxState, desired []DesiredInput) []fastletapi.ActionBindingStatus {
	result := make([]fastletapi.ActionBindingStatus, 0, len(state.bindings))
	seen := make(map[string]struct{}, len(desired))
	for _, input := range desired {
		seen[input.Handler] = struct{}{}
		if binding := state.bindings[input.Handler]; binding != nil {
			result = append(result, binding.status)
		} else {
			result = append(result, fastletapi.ActionBindingStatus{Handler: input.Handler, State: string(apiv1alpha2.ActionPending)})
		}
	}
	for _, name := range state.order {
		if _, found := seen[name]; found {
			continue
		}
		if binding := state.bindings[name]; binding != nil {
			result = append(result, binding.status)
		}
	}
	return result
}

func statusesForDesired(statuses []fastletapi.ActionBindingStatus, _ []DesiredInput) []fastletapi.ActionBindingStatus {
	return statuses
}

func appendDesired(destination, source []DesiredInput) []DesiredInput {
	for _, input := range source {
		destination = append(destination, input)
	}
	return destination
}

func bindingOrder(desired []DesiredInput) []string {
	result := make([]string, 0, len(desired))
	for _, input := range desired {
		result = append(result, input.Handler)
	}
	return result
}

func setBindingStatus(binding *bindingState, state apiv1alpha2.ActionState, message string) {
	if binding.status.State != string(state) || binding.status.Message != message {
		binding.status.LastTransitionTime = time.Now().UTC()
	}
	binding.status.State = string(state)
	binding.status.Message = truncate(message, 512)
}

func buildRequest(operation actionapi.Operation, invocationID string, generation int64, attachment Attachment) actionapi.Request {
	return actionapi.Request{
		APIVersion: actionapi.APIVersion, Operation: operation, InvocationID: invocationID,
		Sandbox: actionapi.SandboxIdentity{UID: attachment.SandboxUID, Name: attachment.SandboxName, Namespace: attachment.Namespace},
		Revision: actionapi.Revision{
			SpecGeneration: generation, RuntimeInstanceID: attachment.RuntimeInstanceID,
			AttachmentID: attachment.ID, RouteGeneration: attachment.RouteGeneration,
		},
		Attachment: actionapi.Attachment{Network: actionapi.NetworkAttachment{
			IP: attachment.IP, Gateway: attachment.Gateway, PrivateCIDR: attachment.PrivateCIDR, HostVeth: attachment.HostVeth,
		}},
	}
}

func stableInvocationID(operation actionapi.Operation, handler, handlerInstance string, generation int64, attachment Attachment, digest, extra string) string {
	payload := strings.Join([]string{
		string(operation), attachment.SandboxUID, handler, handlerInstance,
		fmt.Sprintf("%d", generation), attachment.RuntimeInstanceID, attachment.ID,
		fmt.Sprintf("%d", attachment.RouteGeneration), digest, extra,
	}, "\x00")
	digestBytes := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(digestBytes[:])
}

func truncate(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
