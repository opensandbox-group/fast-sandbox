package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type terminalReconnectPublisher struct {
	*admissionRoutePublisher
	entered chan struct{}
	release chan struct{}
}

func (p *terminalReconnectPublisher) ReconcileRoutes(ctx context.Context, routes []RoutePublication) error {
	if err := p.admissionRoutePublisher.ReconcileRoutes(ctx, routes); err != nil {
		return err
	}
	close(p.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		return nil
	}
}

type terminalInfraCallbackRuntime struct {
	*admissionRuntime
	entered chan struct{}
	release chan struct{}
}

type terminalReconnectAccessRuntime struct {
	*admissionRuntime
	entered chan string
	release chan struct{}
	once    sync.Once
}

func (r *terminalReconnectAccessRuntime) GetAccessDescriptor(id string) (dataplane.AccessDescriptor, error) {
	r.once.Do(func() {
		r.entered <- id
		<-r.release
	})
	return r.admissionRuntime.GetAccessDescriptor(id)
}

func (r *terminalInfraCallbackRuntime) GetAccessDescriptor(id string) (dataplane.AccessDescriptor, error) {
	close(r.entered)
	<-r.release
	return r.admissionRuntime.GetAccessDescriptor(id)
}

type terminalActionCallbackCaller struct {
	legacyOnlyActionCaller
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type terminalCleanupCallbackRuntime struct {
	*terminalFailureBarrierRuntime
}

func (*terminalCleanupCallbackRuntime) GetAccessDescriptor(string) (dataplane.AccessDescriptor, error) {
	return dataplane.AccessDescriptor{Kind: dataplane.AccessKindDirectIP, Address: "172.30.0.2"}, nil
}

type terminalCleanupRoutePublisher struct {
	*admissionRoutePublisher
	writeEntered  chan struct{}
	writeRelease  chan struct{}
	removeEntered chan struct{}
	removeRelease chan struct{}
	removeOnce    sync.Once
}

func (p *terminalCleanupRoutePublisher) ApplyRoute(ctx context.Context, route RoutePublication) error {
	close(p.writeEntered)
	<-p.writeRelease
	return p.admissionRoutePublisher.ApplyRoute(ctx, route)
}

func (p *terminalCleanupRoutePublisher) ReconcileRoutes(ctx context.Context, routes []RoutePublication) error {
	close(p.writeEntered)
	<-p.writeRelease
	return p.admissionRoutePublisher.ReconcileRoutes(ctx, routes)
}

func (p *terminalCleanupRoutePublisher) RemoveRoute(ctx context.Context, route RoutePublication) error {
	p.removeOnce.Do(func() { close(p.removeEntered) })
	<-p.removeRelease
	return p.admissionRoutePublisher.RemoveRoute(ctx, route)
}

func (c *terminalActionCallbackCaller) Invoke(ctx context.Context, _ int32, _ actionapi.Request) error {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return nil
	}
}

func terminalCallbackMetadata(id, phase string) *SandboxMetadata {
	request := ensureRequest(id, 7, 1)
	return &SandboxMetadata{
		Config: fastletapi.RuntimeSandboxConfig{Identity: request.Identity, Desired: request.Sandbox},
		Phase:  phase, AcceptedGeneration: 7, AppliedGeneration: 7,
	}
}

func reserveTerminalCallback(manager *SandboxManager, metadata *SandboxMetadata, state string) string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if state == "recording" {
		manager.terminalFailureWorkers[metadata.Config.Identity.SandboxUID] = metadata
	} else {
		metadata.Phase = state
	}
	return metadata.Phase
}

func terminalCallbackRelease(t *testing.T, release chan struct{}) func() {
	t.Helper()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return unblock
}

func waitTerminalCallback(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not reach the I/O barrier")
	}
}

func TestTerminalFailureReconnectOmitsDeadIdentities(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &admissionRoutePublisher{}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 4, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	live := terminalCallbackMetadata("live", sandboxStateRouteUnavailable)
	manager.sandboxes["live"], runtime.sandboxes["live"] = live, live
	for _, state := range []string{"recording", sandboxStateRuntimeFailureCleanup, sandboxStateRuntimeFailed} {
		metadata := terminalCallbackMetadata(state, sandboxStateRunning)
		manager.sandboxes[state] = metadata
		reserveTerminalCallback(manager, metadata, state)
		// Physical cleanup can already have removed the AccessDescriptor.
		// Reconnect must not resolve or republish any of these identities.
	}
	require.NoError(t, manager.ReconcileProxyRoutes(t.Context()))
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	require.Len(t, publisher.reconciled, 1)
	require.Len(t, publisher.reconciled[0], 1)
	require.Equal(t, "live", publisher.reconciled[0][0].SandboxUID)
	require.Equal(t, sandboxStateRunning, live.Phase)
}

func TestTerminalFailureReconnectOnlyPromotesItsLiveSnapshot(t *testing.T) {
	runtime := newAdmissionRuntime()
	publisher := &terminalReconnectPublisher{admissionRoutePublisher: &admissionRoutePublisher{}, entered: make(chan struct{}), release: make(chan struct{})}
	release := terminalCallbackRelease(t, publisher.release)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 4, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	for _, id := range []string{"live", "replaced", "recording"} {
		metadata := terminalCallbackMetadata(id, sandboxStateRouteUnavailable)
		manager.sandboxes[id], runtime.sandboxes[id] = metadata, metadata
	}
	done := make(chan error, 1)
	go func() { done <- manager.ReconcileProxyRoutes(t.Context()) }()
	waitTerminalCallback(t, publisher.entered)
	manager.mu.Lock()
	newer := terminalCallbackMetadata("replaced", sandboxStateRoutePending)
	newer.Config.Identity = ensureRequest("replaced", 8, 1).Identity
	manager.sandboxes["replaced"] = newer
	late := terminalCallbackMetadata("late", sandboxStateRoutePending)
	manager.sandboxes["late"] = late
	recording := manager.sandboxes["recording"]
	manager.terminalFailureWorkers["recording"] = recording
	manager.mu.Unlock()
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not finish")
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	require.Equal(t, sandboxStateRunning, manager.sandboxes["live"].Phase)
	require.Equal(t, sandboxStateRoutePending, newer.Phase, "G8 was not in the acknowledged route snapshot")
	require.Equal(t, sandboxStateRoutePending, late.Phase, "an entry added during reconnect was not published")
	require.Equal(t, sandboxStateRouteUnavailable, recording.Phase, "terminal persistence owns this selected entry now")
}

func TestTerminalFailureReconnectDoesNotPromoteSkippedOwnerAfterRecordRetry(t *testing.T) {
	runtime := &terminalReconnectAccessRuntime{admissionRuntime: newAdmissionRuntime(), entered: make(chan string, 1), release: make(chan struct{})}
	releaseAccess := terminalCallbackRelease(t, runtime.release)
	publisher := &terminalReconnectPublisher{admissionRoutePublisher: &admissionRoutePublisher{}, entered: make(chan struct{}), release: make(chan struct{})}
	releasePublish := terminalCallbackRelease(t, publisher.release)
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 2, FastletPodUID: "pod-uid-a", RoutePublisher: publisher})
	require.NoError(t, err)
	for _, id := range []string{"first", "second"} {
		metadata := terminalCallbackMetadata(id, sandboxStateRouteUnavailable)
		manager.sandboxes[id], runtime.sandboxes[id] = metadata, metadata
	}
	done := make(chan error, 1)
	go func() { done <- manager.ReconcileProxyRoutes(t.Context()) }()
	var selected string
	select {
	case selected = <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not begin resolving its snapshot")
	}
	skipped := "first"
	if selected == skipped {
		skipped = "second"
	}
	reserveTerminalCallback(manager, manager.sandboxes[skipped], "recording")
	releaseAccess()
	waitTerminalCallback(t, publisher.entered)
	publisher.mu.Lock()
	require.Len(t, publisher.reconciled, 1)
	require.Len(t, publisher.reconciled[0], 1)
	require.Equal(t, selected, publisher.reconciled[0][0].SandboxUID)
	publisher.mu.Unlock()
	// A failed terminal write relinquishes its reservation for retry while
	// the proxy is still acknowledging a snapshot that omitted this owner.
	manager.mu.Lock()
	delete(manager.terminalFailureWorkers, skipped)
	manager.mu.Unlock()
	releasePublish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not finish")
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	require.Equal(t, sandboxStateRunning, manager.sandboxes[selected].Phase)
	require.Equal(t, sandboxStateRouteUnavailable, manager.sandboxes[skipped].Phase, "an owner omitted from the route RPC cannot become running when its terminal write is retried")
}

func TestTerminalFailureFencesLateRoutePublication(t *testing.T) {
	for _, state := range []string{"recording", sandboxStateRuntimeFailureCleanup, sandboxStateRuntimeFailed} {
		t.Run(state, func(t *testing.T) {
			runtime := newAdmissionRuntime()
			publisher := &admissionRoutePublisher{applyEntered: make(chan struct{}, 1), applyBlock: make(chan struct{})}
			release := terminalCallbackRelease(t, publisher.applyBlock)
			manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
				Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
			})
			require.NoError(t, err)
			metadata := terminalCallbackMetadata("late-route", sandboxStateRoutePending)
			manager.sandboxes["late-route"], runtime.sandboxes["late-route"] = metadata, metadata
			manager.routeReady = true
			done := make(chan struct{})
			go func() {
				manager.publishDataPlaneRoute(t.Context(), metadata)
				close(done)
			}()
			waitTerminalCallback(t, publisher.applyEntered)
			phase := reserveTerminalCallback(manager, metadata, state)
			release()
			waitTerminalCallback(t, done)
			manager.mu.RLock()
			require.Equal(t, phase, metadata.Phase, "late route success cannot revive a stopped runtime")
			manager.mu.RUnlock()
			publisher.mu.Lock()
			require.NotEmpty(t, publisher.removed, "a successfully applied stale route must be removed")
			removed := publisher.removed[len(publisher.removed)-1]
			require.Equal(t, metadata.Config.Identity.SandboxUID, removed.SandboxUID)
			require.Equal(t, metadata.Config.Identity.AssignmentAttempt, removed.AssignmentAttempt)
			require.Equal(t, metadata.Config.Identity.RouteGeneration, removed.RouteGeneration)
			publisher.mu.Unlock()
		})
	}
}

func TestTerminalFailureCleanupWaitsForRouteWrites(t *testing.T) {
	for _, operation := range []string{"apply", "reconnect"} {
		t.Run(operation, func(t *testing.T) {
			cleanupRelease := make(chan struct{})
			close(cleanupRelease)
			runtime := &terminalCleanupCallbackRuntime{terminalFailureBarrierRuntime: &terminalFailureBarrierRuntime{
				terminalFailureMockRuntime: &terminalFailureMockRuntime{MockRuntime: NewMockRuntime()},
				cleanupEntered:             make(chan struct{}), cleanupRelease: cleanupRelease,
			}}
			publisher := &terminalCleanupRoutePublisher{
				admissionRoutePublisher: &admissionRoutePublisher{},
				writeEntered:            make(chan struct{}), writeRelease: make(chan struct{}),
				removeEntered: make(chan struct{}), removeRelease: make(chan struct{}),
			}
			releaseWrite := terminalCallbackRelease(t, publisher.writeRelease)
			releaseRemove := terminalCallbackRelease(t, publisher.removeRelease)
			manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
				Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
			})
			require.NoError(t, err)
			metadata := terminalCallbackMetadata("terminal-route-order", sandboxStateRoutePending)
			manager.sandboxes[metadata.Config.Identity.SandboxUID] = metadata
			persisted := *metadata
			persisted.Phase = "Stopped"
			runtime.sandboxes[metadata.Config.Identity.SandboxUID] = &persisted
			manager.routeReady = true
			done := make(chan struct{})
			go func() {
				if operation == "apply" {
					manager.publishDataPlaneRoute(t.Context(), metadata)
				} else {
					manager.ReconcileProxyRoutes(t.Context())
				}
				close(done)
			}()
			waitTerminalCallback(t, publisher.writeEntered)
			manager.preserveExitedRuntime(t.Context(), metadata.Config.Identity.SandboxUID, metadata.Config.Identity, "VMM stopped")
			manager.mu.RLock()
			require.Equal(t, sandboxStateRuntimeFailureCleanup, metadata.Phase)
			manager.mu.RUnlock()
			select {
			case <-publisher.removeEntered:
				t.Fatal("route removal must wait for the in-flight route write to finish")
			case <-time.After(50 * time.Millisecond):
			}
			admission, _, _ := manager.State()
			require.Equal(t, 1, admission.Used, "the network slot must remain reserved while a route write is in flight")
			select {
			case <-runtime.cleanupEntered:
				t.Fatal("physical cleanup overtook the in-flight route write")
			default:
			}

			releaseWrite()
			waitTerminalCallback(t, publisher.removeEntered)
			select {
			case <-runtime.cleanupEntered:
				t.Fatal("physical cleanup must also wait for successful route removal")
			default:
			}
			admission, _, _ = manager.State()
			require.Equal(t, 1, admission.Used)
			releaseRemove()
			waitTerminalCallback(t, runtime.cleanupEntered)
			waitTerminalCallback(t, done)
			require.Eventually(t, func() bool {
				admission, _, _ := manager.State()
				return admission.Used == 0
			}, time.Second, time.Millisecond)
			manager.mu.RLock()
			require.Same(t, metadata, manager.sandboxes[metadata.Config.Identity.SandboxUID], "terminal identity must survive physical cleanup")
			require.Equal(t, sandboxStateRuntimeFailed, metadata.Phase)
			manager.mu.RUnlock()
		})
	}
}

func TestTerminalFailureFencesLateInfraCompletion(t *testing.T) {
	for _, state := range []string{"recording", sandboxStateRuntimeFailureCleanup, sandboxStateRuntimeFailed} {
		t.Run(state, func(t *testing.T) {
			storeRoot := t.TempDir()
			store, err := fastletinfra.NewArtifactStore(storeRoot, storeRoot)
			require.NoError(t, err)
			infra, err := fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{Store: store, Resolver: fastletinfra.NewPlatformResolver(nil)})
			require.NoError(t, err)
			require.NoError(t, infra.Prepare(t.Context()))
			runtime := &terminalInfraCallbackRuntime{admissionRuntime: newAdmissionRuntime(), entered: make(chan struct{}), release: make(chan struct{})}
			release := terminalCallbackRelease(t, runtime.release)
			manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a", InfraManager: infra})
			require.NoError(t, err)
			metadata := terminalCallbackMetadata("late-infra", sandboxStateInitializingInfra)
			metadata.InfraServices = []fastletinfra.ServiceEndpoint{{Component: "execd", Protocol: "HTTP", Port: 44772}}
			metadata.InfraDiagnostics = []fastletinfra.ComponentDiagnostic{{Component: "execd", State: "Failed", Message: "runtime exited"}}
			manager.sandboxes["late-infra"], runtime.sandboxes["late-infra"] = metadata, metadata
			done := make(chan error, 1)
			go func() { done <- manager.initializeInfraInstance(t.Context(), metadata) }()
			waitTerminalCallback(t, runtime.entered)
			phase := reserveTerminalCallback(manager, metadata, state)
			release()
			select {
			case err := <-done:
				require.Error(t, err, "the completed Infra operation belongs to a stopped runtime")
			case <-time.After(time.Second):
				t.Fatal("Infra initialization did not finish")
			}
			manager.mu.RLock()
			defer manager.mu.RUnlock()
			require.Equal(t, phase, metadata.Phase)
			require.Equal(t, []fastletinfra.ServiceEndpoint{{Component: "execd", Protocol: "HTTP", Port: 44772}}, metadata.InfraServices)
			require.Equal(t, []fastletinfra.ComponentDiagnostic{{Component: "execd", State: "Failed", Message: "runtime exited"}}, metadata.InfraDiagnostics)
		})
	}
}

func TestTerminalFailureRecordingFencesHealthRegression(t *testing.T) {
	publisher := &admissionRoutePublisher{}
	manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-uid-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	metadata := terminalCallbackMetadata("late-health", sandboxStateRunning)
	manager.sandboxes["late-health"] = metadata
	reserveTerminalCallback(manager, metadata, "recording")
	manager.markDataPlaneUnhealthy(metadata, errors.New("late health probe failure"))
	manager.mu.RLock()
	require.Equal(t, sandboxStateRunning, metadata.Phase, "a health callback cannot steal the terminal writer's phase")
	manager.mu.RUnlock()
	publisher.mu.Lock()
	require.Empty(t, publisher.removed, "terminal cleanup owns route retirement while persistence is pending")
	publisher.mu.Unlock()
}

func TestTerminalFailureRejectsNewActionReconcile(t *testing.T) {
	for _, state := range []string{"recording", sandboxStateRuntimeFailureCleanup, sandboxStateRuntimeFailed} {
		t.Run(state, func(t *testing.T) {
			manager := newAdmissionManager(t, newAdmissionRuntime(), 1)
			metadata := terminalCallbackMetadata("terminal-actions", sandboxStateActionPending)
			manager.sandboxes["terminal-actions"] = metadata
			phase := reserveTerminalCallback(manager, metadata, state)
			_, err := manager.ReconcileBindings(t.Context(), &fastletapi.ReconcileBindingsRequest{Identity: metadata.Config.Identity, SpecGeneration: 8})
			require.Error(t, err)
			require.Equal(t, phase, metadata.Phase)
			require.Equal(t, int64(7), metadata.AcceptedGeneration)
			require.Equal(t, int64(7), metadata.AppliedGeneration)
		})
	}
}

func TestTerminalFailureFencesLateActionReconcileAndNotifier(t *testing.T) {
	for _, state := range []string{"recording", sandboxStateRuntimeFailureCleanup, sandboxStateRuntimeFailed} {
		t.Run(state, func(t *testing.T) {
			caller := &terminalActionCallbackCaller{entered: make(chan struct{}), release: make(chan struct{})}
			release := terminalCallbackRelease(t, caller.release)
			actions, err := fastletaction.NewManager([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, caller)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			actions.Start(ctx)
			require.Eventually(t, actions.Ready, time.Second, time.Millisecond)
			manager, err := NewSandboxManagerWithConfig(newAdmissionRuntime(), SandboxManagerConfig{Capacity: 1, FastletPodUID: "pod-uid-a", ActionManager: actions})
			require.NoError(t, err)
			metadata := terminalCallbackMetadata("late-action", sandboxStateActionPending)
			metadata.Allocation.Network.IP = "172.30.0.2"
			manager.sandboxes["late-action"] = metadata
			done := make(chan error, 1)
			go func() {
				_, err := manager.ReconcileBindings(ctx, &fastletapi.ReconcileBindingsRequest{
					Identity: metadata.Config.Identity, SpecGeneration: 8,
					ActionBindings: []fastletapi.ActionBindingInput{{Handler: "egress", Input: `{}`}},
				})
				done <- err
			}()
			waitTerminalCallback(t, caller.entered)
			phase := reserveTerminalCallback(manager, metadata, state)
			manager.mu.RLock()
			accepted, applied := metadata.AcceptedGeneration, metadata.AppliedGeneration
			statuses := append([]fastletapi.ActionBindingStatus(nil), metadata.ActionBindingStatuses...)
			manager.mu.RUnlock()
			release()
			select {
			case err := <-done:
				require.Error(t, err, "an in-flight Action response cannot commit after terminal persistence starts")
			case <-time.After(time.Second):
				t.Fatal("Action reconciliation did not finish")
			}
			manager.actionStateChanged(metadata.Config.Identity.SandboxUID)
			manager.mu.RLock()
			defer manager.mu.RUnlock()
			require.Equal(t, phase, metadata.Phase)
			require.Equal(t, accepted, metadata.AcceptedGeneration)
			require.Equal(t, applied, metadata.AppliedGeneration)
			require.Equal(t, statuses, metadata.ActionBindingStatuses)
		})
	}
}
