package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/klog/v2"

	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// ReconcileProxyRoutes is invoked after Fastlet Proxy reconnects. It rebuilds
// the volatile RouteStore from runtime-backed manager state and is the only
// operation that restores route readiness after a proxy control disconnect.
func (m *SandboxManager) ReconcileProxyRoutes(ctx context.Context) error {
	if m.routePublisher == nil {
		return nil
	}
	m.routeMu.Lock()
	defer m.routeMu.Unlock()
	m.mu.RLock()
	metadata := make([]SandboxMetadata, 0, len(m.sandboxes))
	owners := make([]*SandboxMetadata, 0, len(m.sandboxes))
	pendingInfra := false
	for _, sandbox := range m.sandboxes {
		if sandbox.RecoveryRequired {
			pendingInfra = true
		}
		if !m.runtimeWorkAllowedLocked(sandbox) {
			continue
		}
		switch sandbox.Phase {
		case sandboxStateInfraPending, sandboxStateInitializingInfra, sandboxStateInfraUnavailable:
			pendingInfra = true
			continue
		}
		snapshot := *sandbox
		snapshot.InfraServices = slices.Clone(sandbox.InfraServices)
		metadata = append(metadata, snapshot)
		owners = append(owners, sandbox)
	}
	m.mu.RUnlock()
	if pendingInfra {
		m.MarkProxyRouteUnavailable()
		return fmt.Errorf("reconcile proxy routes: %w", ErrInfraUnavailable)
	}
	publications := make([]RoutePublication, 0, len(metadata))
	publishedOwners := make([]*SandboxMetadata, 0, len(metadata))
	for index := range metadata {
		m.mu.RLock()
		allowed := m.runtimeWorkAllowedLocked(owners[index])
		m.mu.RUnlock()
		if !allowed {
			continue
		}
		publication, err := m.routePublication(&metadata[index])
		if err != nil {
			m.MarkProxyRouteUnavailable()
			return err
		}
		publications = append(publications, publication)
		publishedOwners = append(publishedOwners, owners[index])
	}
	if err := m.routePublisher.ReconcileRoutes(ctx, publications); err != nil {
		m.MarkProxyRouteUnavailable()
		return err
	}
	m.mu.Lock()
	for _, sandbox := range publishedOwners {
		if m.runtimeWorkAllowedLocked(sandbox) && (sandbox.Phase == sandboxStateRoutePending || sandbox.Phase == sandboxStateRouteUnavailable) {
			sandbox.Phase = sandboxStateRunning
		}
	}
	m.routeReady = true
	m.mu.Unlock()
	return nil
}

func (m *SandboxManager) MarkProxyRouteUnavailable() {
	if m.routePublisher == nil {
		return
	}
	m.mu.Lock()
	m.routeReady = false
	m.mu.Unlock()
}

func (m *SandboxManager) routePublication(metadata *SandboxMetadata) (RoutePublication, error) {
	if m.routePublisher == nil {
		return RoutePublication{}, nil
	}
	provider, ok := m.runtime.(AccessDescriptorProvider)
	if !ok {
		return RoutePublication{}, fmt.Errorf("runtime does not provide an AccessDescriptor")
	}
	identity := metadata.Config.Identity
	access, err := provider.GetAccessDescriptor(identity.SandboxUID)
	if err != nil {
		return RoutePublication{}, fmt.Errorf("resolve runtime AccessDescriptor: %w", err)
	}
	routeGeneration := identity.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	if identity.Namespace == "" || identity.SandboxUID == "" || identity.FastletPodUID == "" || identity.AssignmentAttempt <= 0 {
		return RoutePublication{}, fmt.Errorf("incomplete Sandbox route identity")
	}
	components := make(map[string]dataplane.ComponentRoute, len(metadata.InfraServices))
	for _, endpoint := range metadata.InfraServices {
		if endpoint.HostProcess {
			// Host-process components are reached through the Pod-local
			// egress route, never through a per-Sandbox component route.
			klog.InfoS("host-process component excluded from per-Sandbox component routes",
				"component", endpoint.Component, "port", endpoint.Port, "sandboxID", metadata.Config.Identity.SandboxUID)
			continue
		}
		components[endpoint.Component] = dataplane.ComponentRoute{
			Protocol: endpoint.Protocol, Port: endpoint.Port,
		}
	}
	return RoutePublication{
		Namespace: identity.Namespace, SandboxUID: identity.SandboxUID,
		FastletPodUID: identity.FastletPodUID, AssignmentAttempt: identity.AssignmentAttempt,
		RouteGeneration: routeGeneration, Access: access, Components: components,
	}, nil
}

func (m *SandboxManager) publishRoute(ctx context.Context, metadata *SandboxMetadata) error {
	if m.routePublisher == nil {
		return nil
	}
	m.routeMu.Lock()
	defer m.routeMu.Unlock()
	m.mu.RLock()
	if !m.runtimeWorkAllowedLocked(metadata) {
		m.mu.RUnlock()
		return fmt.Errorf("Sandbox is no longer accepting route publication")
	}
	snapshot := *metadata
	snapshot.InfraServices = slices.Clone(metadata.InfraServices)
	m.mu.RUnlock()
	publication, err := m.routePublication(&snapshot)
	if err != nil {
		m.MarkProxyRouteUnavailable()
		return err
	}
	if publisher, ok := m.routePublisher.(dataplane.RecoveryRoutePublisher); ok {
		if err := publisher.ResumeRoute(ctx, publication); err != nil {
			m.MarkProxyRouteUnavailable()
			return err
		}
	}
	if err := m.routePublisher.ApplyRoute(ctx, publication); err != nil {
		m.MarkProxyRouteUnavailable()
		return err
	}
	return nil
}

func (m *SandboxManager) removeRoute(ctx context.Context, metadata *SandboxMetadata) error {
	if m.routePublisher == nil {
		return nil
	}
	m.routeMu.Lock()
	defer m.routeMu.Unlock()
	// Removing a route only needs its fenced identity. Runtime deletion may
	// already have removed the AccessDescriptor when a previous cleanup attempt
	// failed later (for example while releasing a network slot). Re-resolving
	// runtime access here would turn an otherwise idempotent retry into a
	// permanent delete-failed loop.
	identity := metadata.Config.Identity
	publication := RoutePublication{
		Namespace: identity.Namespace, SandboxUID: identity.SandboxUID,
		FastletPodUID: identity.FastletPodUID, AssignmentAttempt: identity.AssignmentAttempt,
		RouteGeneration: identity.RouteGeneration,
	}
	if publication.RouteGeneration <= 0 {
		publication.RouteGeneration = 1
	}
	return m.routePublisher.RemoveRoute(ctx, publication)
}

// refreshRuntimeRecovery withdraws access to a fenced live runtime without
// declaring it dead. Heartbeats retry recovery only after the snapshot worker
// has finished; status-only callers never initiate VM operations.
func (m *SandboxManager) refreshRuntimeRecovery(ctx context.Context, identity fastletapi.SandboxIdentity, retry bool) {
	if _, err := m.observeRuntimeRecovery(ctx, identity, retry); err != nil {
		klog.V(2).InfoS("runtime recovery observation unavailable", "sandboxID", identity.SandboxUID, "err", err)
	}
}

func (m *SandboxManager) observeRuntimeRecovery(ctx context.Context, identity fastletapi.SandboxIdentity, retry bool) (observed *SandboxMetadata, inspectErr error) {
	reconciler, supported := m.runtime.(runtimecontract.RecoveryReconciler)
	if !supported {
		return observed, inspectErr
	}
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	m.mu.RLock()
	metadata := m.sandboxes[identity.SandboxUID]
	if metadata == nil || metadata.Config.Identity != identity || !runtimeCreatedPhase(metadata.Phase) {
		m.mu.RUnlock()
		return observed, inspectErr
	}
	required := metadata.RecoveryRequired
	for _, task := range m.snapshots {
		if task.identity.Sandbox.SandboxUID == identity.SandboxUID && holdsPauseWindow(task.phase) {
			retry = false
		}
	}
	m.mu.RUnlock()
	observed, inspectErr = m.runtime.InspectSandbox(ctx, identity.SandboxUID)
	if inspectErr != nil {
		return nil, inspectErr
	}
	if observed == nil || !runtimecontract.SameRuntimeIdentity(observed.Config.Identity, identity) {
		return nil, errors.New("runtime inspection returned no matching owner")
	}
	if !observed.RecoveryRequired && observed.Phase == sandboxStateRuntimeFailed {
		// The driver has durably confirmed process exit. Use the existing
		// owner-retirement and terminal cleanup path, never republish access.
		m.preserveExitedRuntime(ctx, identity.SandboxUID, identity, observed.RuntimeMessage)
		return observed, inspectErr
	}
	if required && !observed.RecoveryRequired && !strings.EqualFold(observed.Phase, "running") {
		return observed, inspectErr
	}
	m.mu.Lock()
	if m.sandboxes[identity.SandboxUID] != metadata || metadata.Config.Identity != identity || !runtimeCreatedPhase(metadata.Phase) {
		m.mu.Unlock()
		return observed, inspectErr
	}
	wasRequired := metadata.RecoveryRequired
	metadata.RecoveryRequired = observed.RecoveryRequired
	if observed.RecoveryRequired {
		metadata.RuntimeMessage = observed.RuntimeMessage
	} else if wasRequired {
		metadata.RuntimeMessage = ""
		// Re-run Infra checks and route publication after thaw before readiness.
		metadata.Phase = sandboxStateInfraPending
	}
	restartDataPlane := !observed.RecoveryRequired && dataPlaneWorkPending(metadata.Phase)
	m.mu.Unlock()
	if observed.RecoveryRequired {
		if err := m.suspendRecoveryRoute(ctx, metadata); err != nil {
			m.MarkProxyRouteUnavailable()
			klog.ErrorS(err, "cannot withdraw route during runtime recovery", "sandboxID", identity.SandboxUID)
		}
		if retry {
			m.startRuntimeRecovery(metadata, reconciler)
		}
	} else if restartDataPlane {
		m.startDataPlaneReconcile(metadata, m.clock.Now())
	}
	return observed, inspectErr
}

func (m *SandboxManager) refreshRecoveryMetadata(ctx context.Context, metadata SandboxMetadata) (SandboxMetadata, *SandboxMetadata) {
	observed, err := m.observeRuntimeRecovery(ctx, metadata.Config.Identity, true)
	if observed == nil && err == nil {
		// Runtimes without recovery support and terminal phases still need one probe.
		observed, err = m.runtime.InspectSandbox(ctx, metadata.Config.Identity.SandboxUID)
	}
	if err != nil {
		observed = nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if current := m.sandboxes[metadata.Config.Identity.SandboxUID]; current != nil && current.Config.Identity == metadata.Config.Identity {
		return *current, observed
	}
	return metadata, observed
}

func applyRecoveryObservation(metadata *SandboxMetadata, runtime fastletapi.RuntimeObservation, dataPlane fastletapi.DataPlaneObservation) (fastletapi.RuntimeObservation, fastletapi.DataPlaneObservation) {
	if metadata.RecoveryRequired {
		return fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateUnavailable, Message: metadata.RuntimeMessage},
			fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateUnavailable, Message: "runtime recovery required"}
	}
	return runtime, dataPlane
}

// A slow Guest must not stall heartbeats for the entire Fastlet. One bounded
// worker per runtime identity retries under the driver's durable lifecycle lock.
func (m *SandboxManager) startRuntimeRecovery(metadata *SandboxMetadata, reconciler runtimecontract.RecoveryReconciler) {
	m.mu.Lock()
	identity := metadata.Config.Identity
	id := identity.SandboxUID
	if m.sandboxes[id] != metadata || !metadata.RecoveryRequired || !runtimeCreatedPhase(metadata.Phase) || m.recoveryWorkers[id] != nil {
		m.mu.Unlock()
		return
	}
	if m.recoveryWorkers == nil {
		m.recoveryWorkers = make(map[string]*SandboxMetadata)
	}
	m.recoveryWorkers[id] = metadata
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.recoveryWorkers, id)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := reconciler.ReconcileSandbox(ctx, identity)
		cancel()
		if err != nil {
			klog.V(2).InfoS("runtime recovery remains pending", "sandboxID", id, "err", err)
		}
		probe, cancelProbe := context.WithTimeout(context.Background(), time.Second)
		defer cancelProbe()
		m.refreshRuntimeRecovery(probe, identity, false)
	}()
}

func (m *SandboxManager) suspendRecoveryRoute(ctx context.Context, metadata *SandboxMetadata) error {
	if m.routePublisher == nil {
		return nil
	}
	m.routeMu.Lock()
	defer m.routeMu.Unlock()
	m.mu.RLock()
	current := m.sandboxes[metadata.Config.Identity.SandboxUID] == metadata && runtimeCreatedPhase(metadata.Phase)
	identity := metadata.Config.Identity
	m.mu.RUnlock()
	// Accepted deletion owns irreversible withdrawal.
	if !current {
		return nil
	}
	publisher, ok := m.routePublisher.(dataplane.RecoveryRoutePublisher)
	if !ok {
		return errors.New("proxy does not support reversible recovery suspension")
	}
	generation := identity.RouteGeneration
	if generation <= 0 {
		generation = 1
	}
	return publisher.SuspendRoute(ctx, RoutePublication{SandboxUID: identity.SandboxUID, RouteGeneration: generation})
}
