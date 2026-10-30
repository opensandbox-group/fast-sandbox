package action

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	actionapi "fast-sandbox/internal/protocol/action"
)

// Signal the exact point where Delete has captured its owner and is waiting
// for an earlier operation. No sleeps or scheduler assumptions are needed.
type operationWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *operationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestDeleteWaitingForOperationDoesNotPoisonSuccessor(t *testing.T) {
	for _, version := range []string{actionapi.APIVersion, actionapi.APIVersionV2} {
		t.Run(version, func(t *testing.T) {
			caller := &fakeCaller{instanceID: "handler-1", fail: make(map[actionapi.Operation]int)}
			manager := readyManagerWithAPIVersion(t, []apiv1alpha2.ActionHandler{handler("egress", 18080)}, version, caller)
			old := attachment("a")
			old.APIVersion, old.FastletPodUID = version, "pod-a"
			_, _, err := manager.Reconcile(context.Background(), old, 1, []DesiredInput{input("egress", "v1")})
			require.NoError(t, err)
			state := manager.getState("a")
			require.True(t, state.lockOperations(context.Background()))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			waiting := &operationWaitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if version == actionapi.APIVersionV2 {
					done <- manager.DeleteOwned(waiting, old)
				} else {
					done <- manager.Delete(waiting, "a")
				}
			}()
			<-waiting.waiting
			next := old
			next.ID = "attachment-next"
			next.RuntimeInstanceID = "runtime-next"
			next.InstanceGeneration++
			err = manager.registerDesired(state, next, 2, nil, "")
			state.unlockOperations()
			require.NoError(t, err)
			require.ErrorContains(t, <-done, "owner changed")
			state.mu.RLock()
			terminating := state.terminating
			current := state.attachment
			state.mu.RUnlock()
			require.False(t, terminating)
			require.Equal(t, next, current)
			require.NoError(t, manager.registerDesired(state, next, 3, nil, ""))
			for _, r := range caller.snapshot() {
				require.NotEqual(t, actionapi.OperationRemoveBinding, r.request.Operation)
			}
		})
	}
}

type lostSetReplyCaller struct {
	*fakeCaller
	failPort int32
}

func (c *lostSetReplyCaller) Invoke(ctx context.Context, port int32, request actionapi.Request) error {
	if err := c.fakeCaller.Invoke(ctx, port, request); err != nil {
		return err
	}
	if port == c.failPort && request.Operation == actionapi.OperationSetBinding {
		return errors.New("SET delivered but reply lost")
	}
	return nil
}

func TestActionsV2RetiresAllDeclaredHandlersDespitePartialApply(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "lost-set-ack"}[lostAck], func(t *testing.T) {
			caller := &lostSetReplyCaller{fakeCaller: &fakeCaller{instanceID: "handler-1", fail: make(map[actionapi.Operation]int)}}
			manager, err := NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{handler("first", 18080), handler("second", 18081)}, actionapi.APIVersionV2, caller)
			require.NoError(t, err)
			manager.probe(context.Background())
			require.True(t, manager.Ready())
			owner := attachment("a")
			owner.APIVersion = actionapi.APIVersionV2
			owner.FastletPodUID = "pod-a"
			_, _, err = manager.Reconcile(context.Background(), owner, 1, []DesiredInput{input("first", "v1")})
			require.NoError(t, err)
			if lostAck {
				caller.failPort = 18081
				_, _, err = manager.Reconcile(context.Background(), owner, 2, []DesiredInput{input("first", "v1"), input("second", "v2")})
				require.Error(t, err)
				requests := caller.snapshot()
				require.Equal(t, int32(18081), requests[len(requests)-1].port)
				require.Equal(t, actionapi.OperationSetBinding, requests[len(requests)-1].request.Operation)
				state := manager.getState("a")
				state.mu.RLock()
				require.Zero(t, state.bindings["second"].appliedGeneration)
				require.Equal(t, []string{"first"}, state.order)
				state.mu.RUnlock()
			}
			body, err := json.Marshal(actionapi.OwnerObservation{State: actionapi.OwnerStateCurrent, Owner: ownerFromAttachment(owner)})
			require.NoError(t, err)
			caller.mu.Lock()
			caller.relayBody = body
			caller.mu.Unlock()
			require.NoError(t, manager.DeleteOwned(context.Background(), owner))
			var ports []int32
			for _, r := range caller.snapshot() {
				if r.request.Operation == actionapi.OperationRemoveBinding {
					ports = append(ports, r.port)
				}
			}
			require.Equal(t, []int32{18081, 18080}, ports)
			require.Nil(t, manager.getState("a"))
		})
	}
}

func TestCreateFenceRejectsInvalidContextBeforeIO(t *testing.T) {
	for _, version := range []string{actionapi.APIVersion, actionapi.APIVersionV2} {
		caller := &fakeCaller{instanceID: "handler-1"}
		manager := readyManagerWithAPIVersion(t, []apiv1alpha2.ActionHandler{handler("egress", 18080)}, version, caller)
		owner := attachment("a")
		owner.APIVersion = version // deliberately missing Pod UID
		require.Error(t, manager.CheckCreateOwner(context.Background(), owner))
		require.Empty(t, caller.snapshotRelay())
	}
	caller := NewHTTPCaller()
	_, _, err := caller.Relay(context.Background(), 18080, actionapi.APIVersion, actionapi.Owner{}, http.MethodGet, "/policy/a", nil)
	require.ErrorContains(t, err, "requires a Sandbox UID")
}

// Each probe announces entry before waiting, making serial execution fail
// deterministically rather than comparing noisy wall-clock timings.
type gatedProbeCaller struct {
	*fakeCaller
	entered        chan int32
	release        <-chan struct{}
	failPort       int32
	failureRelease <-chan struct{}
	exited         chan int32
}

func (c *gatedProbeCaller) Relay(ctx context.Context, port int32, _ string, owner actionapi.Owner, _, _ string, _ []byte) (int, []byte, error) {
	c.entered <- port
	defer func() { c.exited <- port }()
	if port == c.failPort {
		select {
		case <-c.failureRelease:
			return 0, nil, errors.New("injected probe failure")
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	select {
	case <-c.release:
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	body, err := json.Marshal(actionapi.OwnerObservation{State: actionapi.OwnerStateAbsent, Owner: owner})
	return http.StatusOK, body, err
}

func TestCreateOwnerProbesRunConcurrentlyAndWaitForAll(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "all-pass", true: "probe-fails"}[fail], func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			caller := &gatedProbeCaller{fakeCaller: &fakeCaller{instanceID: "handler-1"}, entered: make(chan int32, 3), release: release, exited: make(chan int32, 3)}
			if fail {
				caller.failPort = 18081
				caller.failureRelease = release
			}
			manager, err := NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{handler("one", 18080), handler("two", 18081), handler("three", 18082)}, actionapi.APIVersionV2, caller)
			require.NoError(t, err)
			owner := attachment("a")
			owner.APIVersion = actionapi.APIVersionV2
			owner.FastletPodUID = "pod-a"
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- manager.CheckCreateOwner(ctx, owner) }()
			var entered []int32
			for range 3 {
				select {
				case port := <-caller.entered:
					entered = append(entered, port)
				case <-ctx.Done():
					t.Fatal("all probes must start before any completes")
				}
			}
			require.ElementsMatch(t, []int32{18080, 18081, 18082}, entered)
			select {
			case <-done:
				t.Fatal("admission returned before probes completed")
			default:
			}
			unblock()
			err = <-done
			if fail {
				require.ErrorContains(t, err, "injected probe failure")
			} else {
				require.NoError(t, err)
			}
			require.Len(t, caller.exited, 3, "no probe is left running after admission returns")
		})
	}
}

func TestCreateOwnerProbeDeadlineCancelsAllWorkers(t *testing.T) {
	caller := &gatedProbeCaller{fakeCaller: &fakeCaller{instanceID: "handler-1"}, entered: make(chan int32, 2), release: make(chan struct{}), exited: make(chan int32, 2)}
	manager, err := NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{handler("one", 18080), handler("two", 18081)}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	owner := attachment("a")
	owner.APIVersion = actionapi.APIVersionV2
	owner.FastletPodUID = "pod-a"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.CheckCreateOwner(ctx, owner), context.DeadlineExceeded)
	require.Len(t, caller.exited, 2)
}

func TestCreateOwnerFailedProbeCancelsBlockedPeers(t *testing.T) {
	failure := make(chan struct{})
	caller := &gatedProbeCaller{fakeCaller: &fakeCaller{instanceID: "handler-1"}, entered: make(chan int32, 2), release: make(chan struct{}), failureRelease: failure, failPort: 18081, exited: make(chan int32, 2)}
	manager, err := NewManagerWithAPIVersion([]apiv1alpha2.ActionHandler{handler("blocked", 18080), handler("fails", 18081)}, actionapi.APIVersionV2, caller)
	require.NoError(t, err)
	owner := attachment("a")
	owner.APIVersion = actionapi.APIVersionV2
	owner.FastletPodUID = "pod-a"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.CheckCreateOwner(ctx, owner) }()
	for range 2 {
		select {
		case <-caller.entered:
		case <-ctx.Done():
			t.Fatal("probe did not enter")
		}
	}
	close(failure)
	err = <-done
	require.ErrorContains(t, err, "injected probe failure")
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, ctx.Err(), "peer cancellation must not wait for the outer deadline")
	require.Len(t, caller.exited, 2)
}
