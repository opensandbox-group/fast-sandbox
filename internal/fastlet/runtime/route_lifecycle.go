package runtime

import (
	"context"
	"fmt"
)

// ReconcileProxyRoutes is invoked after Fastlet Proxy reconnects. It rebuilds
// the volatile RouteStore from runtime-backed manager state and is the only
// operation that restores route readiness after a proxy control disconnect.
func (m *SandboxManager) ReconcileProxyRoutes(ctx context.Context) error {
	if m.routePublisher == nil {
		return nil
	}
	m.mu.RLock()
	metadata := make([]SandboxMetadata, 0, len(m.sandboxes))
	for _, sandbox := range m.sandboxes {
		switch sandbox.Phase {
		case "terminating", "deleting", "delete-failed":
			continue
		}
		metadata = append(metadata, *sandbox)
	}
	m.mu.RUnlock()
	publications := make([]RoutePublication, 0, len(metadata))
	for index := range metadata {
		publication, err := m.routePublication(&metadata[index])
		if err != nil {
			m.MarkProxyRouteUnavailable()
			return err
		}
		publications = append(publications, publication)
	}
	if err := m.routePublisher.ReconcileRoutes(ctx, publications); err != nil {
		m.MarkProxyRouteUnavailable()
		return err
	}
	m.mu.Lock()
	for _, sandbox := range m.sandboxes {
		if sandbox.Phase == "route-pending" {
			sandbox.Phase = "running"
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
	access, err := provider.GetAccessDescriptor(metadata.SandboxID)
	if err != nil {
		return RoutePublication{}, fmt.Errorf("resolve runtime AccessDescriptor: %w", err)
	}
	routeGeneration := metadata.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	if metadata.ClaimNamespace == "" || metadata.SandboxID == "" || metadata.FastletPodUID == "" || metadata.AssignmentAttempt <= 0 {
		return RoutePublication{}, fmt.Errorf("incomplete Sandbox route identity")
	}
	return RoutePublication{
		Namespace: metadata.ClaimNamespace, SandboxUID: metadata.SandboxID,
		FastletPodUID: metadata.FastletPodUID, AssignmentAttempt: metadata.AssignmentAttempt,
		RouteGeneration: routeGeneration, Access: access,
	}, nil
}

func (m *SandboxManager) publishRoute(ctx context.Context, metadata *SandboxMetadata) error {
	if m.routePublisher == nil {
		return nil
	}
	publication, err := m.routePublication(metadata)
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
	publication, err := m.routePublication(metadata)
	if err != nil {
		return err
	}
	return m.routePublisher.RemoveRoute(ctx, publication)
}
