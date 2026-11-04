package sandbox

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/klog/v2"

	dataplane "fast-sandbox/internal/dataplane/contract"
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
