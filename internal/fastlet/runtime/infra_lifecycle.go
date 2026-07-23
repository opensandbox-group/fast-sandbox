package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

type dataPlaneWorker struct {
	metadata *SandboxMetadata
	cancel   context.CancelFunc
}

const (
	initialDataPlaneRetry = 100 * time.Millisecond
	maxDataPlaneRetry     = 2 * time.Second
)

func (m *SandboxManager) initializeInfraInstance(ctx context.Context, metadata *SandboxMetadata) error {
	if m.infraManager == nil {
		return nil
	}
	var instance fastletinfra.PreparedInstance
	var err error
	if metadata.NetworkIP != "" {
		instance, err = m.infraManager.InitializeInstance(ctx, &metadata.SandboxSpec, metadata.NetworkIP)
	} else if provider, ok := m.runtime.(AccessDescriptorProvider); ok {
		var access fastletnetwork.AccessDescriptor
		access, err = provider.GetAccessDescriptor(metadata.SandboxID)
		if err == nil {
			switch access.Kind {
			case fastletnetwork.AccessKindDirectIP:
				instance, err = m.infraManager.InitializeInstance(ctx, &metadata.SandboxSpec, access.Address)
			case fastletnetwork.AccessKindLocalForward:
				endpoint := access.Address
				instance, err = m.infraManager.InitializeInstanceWithDialer(ctx, &metadata.SandboxSpec, func(ctx context.Context, targetPort uint32) (net.Conn, error) {
					connection, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
					if dialErr != nil {
						return nil, dialErr
					}
					preamble, encodeErr := fastletnetwork.EncodeLocalForwardPreamble(targetPort, access.Credential)
					if encodeErr == nil {
						encodeErr = fastletnetwork.WriteLocalForwardPreamble(connection, preamble)
					}
					if encodeErr != nil {
						_ = connection.Close()
						return nil, encodeErr
					}
					return connection, nil
				})
			default:
				err = fmt.Errorf("unsupported Infra access kind %q", access.Kind)
			}
		}
	} else {
		err = errors.New("runtime did not provide an Infra access descriptor")
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInfraUnavailable, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sandboxes[metadata.SandboxID]
	if current != metadata || metadata.Phase == "terminating" || metadata.Phase == "deleting" {
		return errors.New("Sandbox changed while Infra Components were initializing")
	}
	metadata.InfraServices = append(metadata.InfraServices[:0], instance.Services...)
	metadata.InfraUpstreamHeadersByPort = upstreamHeadersByServicePort(instance.Services, instance.UpstreamHeaders)
	metadata.InfraDiagnostics = append(metadata.InfraDiagnostics[:0], instance.Diagnostics...)
	return nil
}

func upstreamHeadersByServicePort(services []fastletinfra.ServiceEndpoint, headers map[string]string) map[uint32]map[string]string {
	if len(headers) == 0 {
		return nil
	}
	result := make(map[uint32]map[string]string, len(services))
	for _, service := range services {
		if service.Port == 0 {
			continue
		}
		result[service.Port] = cloneStringMap(headers)
	}
	return result
}

// startDataPlaneReconcile advances a runtime-ready Sandbox independently from
// the Create RPC. Capacity bounds the number of workers, and the metadata
// pointer plus instance fencing prevents an old worker from mutating a newer
// generation.
func (m *SandboxManager) startDataPlaneReconcile(metadata *SandboxMetadata, started time.Time) {
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata || !dataPlaneWorkPending(metadata.Phase) {
		m.mu.Unlock()
		return
	}
	if worker, found := m.dataPlaneWorkers[metadata.SandboxID]; found {
		if worker.metadata == metadata {
			m.mu.Unlock()
			return
		}
		worker.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.dataPlaneWorkers[metadata.SandboxID] = dataPlaneWorker{metadata: metadata, cancel: cancel}
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			if worker, found := m.dataPlaneWorkers[metadata.SandboxID]; found && worker.metadata == metadata {
				delete(m.dataPlaneWorkers, metadata.SandboxID)
			}
			m.mu.Unlock()
		}()

		retryDelay := initialDataPlaneRetry
		for {
			ready, err := m.reconcileDataPlaneOnce(ctx, metadata)
			if ready {
				m.mu.RLock()
				completed := m.sandboxes[metadata.SandboxID] == metadata && metadata.Phase == "running"
				m.mu.RUnlock()
				if err == nil && completed {
					observeDataPlaneReady(m.runtimeName, m.infraProfile, started, nil)
				}
				return
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			retryDelay = min(retryDelay*2, maxDataPlaneRetry)
		}
	}()
}

func dataPlaneWorkPending(phase string) bool {
	switch phase {
	case "infra-pending", "initializing-infra", "infra-unavailable", "route-pending", "publishing-route", "route-unavailable":
		return true
	default:
		return false
	}
}

func (m *SandboxManager) cancelDataPlaneReconcileLocked(metadata *SandboxMetadata) {
	worker, found := m.dataPlaneWorkers[metadata.SandboxID]
	if !found || worker.metadata != metadata {
		return
	}
	delete(m.dataPlaneWorkers, metadata.SandboxID)
	worker.cancel()
}

// reconcileDataPlaneOnce performs at most one Infra readiness attempt and one
// route publication attempt. A retryable failure is surfaced through the local
// phase while the runtime remains ready.
func (m *SandboxManager) reconcileDataPlaneOnce(ctx context.Context, metadata *SandboxMetadata) (bool, error) {
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata {
		m.mu.Unlock()
		return true, nil
	}
	switch metadata.Phase {
	case "running":
		m.mu.Unlock()
		return true, nil
	case "infra-pending", "infra-unavailable":
		metadata.Phase = "initializing-infra"
		m.mu.Unlock()
	case "route-pending", "route-unavailable":
		m.mu.Unlock()
		return m.publishDataPlaneRoute(ctx, metadata)
	case "terminating", "deleting", "delete-failed", "create-cleanup", "create-cleanup-failed":
		m.mu.Unlock()
		return true, ctx.Err()
	case "initializing-infra", "publishing-route":
		// Another recovery/reconnect path owns the transition.
		m.mu.Unlock()
		return false, nil
	default:
		phase := metadata.Phase
		m.mu.Unlock()
		return true, fmt.Errorf("runtime is in non-reconcilable phase %s", phase)
	}

	infraErr := m.initializeInfraInstance(ctx, metadata)
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "initializing-infra" {
		m.mu.Unlock()
		return true, nil
	}
	if infraErr != nil {
		metadata.Phase = "infra-unavailable"
		m.recordDiagnosticLocked(metadata.SandboxID, "error", "infra", "infra-unavailable", infraErr.Error())
		m.mu.Unlock()
		return false, infraErr
	}
	metadata.Phase = "route-pending"
	m.recordDiagnosticLocked(metadata.SandboxID, "info", "infra", "route-pending", "required Infra Components are ready; proxy route publication continues asynchronously")
	m.mu.Unlock()
	return m.publishDataPlaneRoute(ctx, metadata)
}

func (m *SandboxManager) publishDataPlaneRoute(ctx context.Context, metadata *SandboxMetadata) (bool, error) {
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata {
		m.mu.Unlock()
		return true, nil
	}
	if metadata.Phase != "route-pending" && metadata.Phase != "route-unavailable" {
		done := !dataPlaneWorkPending(metadata.Phase)
		m.mu.Unlock()
		return done, nil
	}
	metadata.Phase = "publishing-route"
	m.mu.Unlock()

	publishErr := m.publishRoute(ctx, metadata)
	routeApplied := publishErr == nil
	if publishErr == nil && m.routePublisher != nil {
		m.mu.RLock()
		restoreSnapshot := !m.routeReady
		m.mu.RUnlock()
		if restoreSnapshot {
			publishErr = m.ReconcileProxyRoutes(ctx)
		}
	}
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "publishing-route" {
		m.mu.Unlock()
		if routeApplied {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = m.removeRoute(cleanupCtx, metadata)
			cancel()
		}
		return true, nil
	}
	if publishErr != nil {
		metadata.Phase = "route-unavailable"
		m.recordDiagnosticLocked(metadata.SandboxID, "error", "route", "route-unavailable", publishErr.Error())
		m.mu.Unlock()
		return false, publishErr
	}
	metadata.Phase = "running"
	m.recordDiagnosticLocked(metadata.SandboxID, "info", "fastlet", "running", "runtime, private network, Infra Components, and proxy route are ready")
	m.mu.Unlock()
	return true, nil
}

// ReconcilePendingInfra is called after profile artifacts become Prepared and
// on subsequent recovery retries. New Create calls use the asynchronous worker
// above and never wait for this method.
func (m *SandboxManager) ReconcilePendingInfra(ctx context.Context) error {
	m.mu.RLock()
	pending := make([]*SandboxMetadata, 0)
	for _, metadata := range m.sandboxes {
		if dataPlaneWorkPending(metadata.Phase) {
			pending = append(pending, metadata)
		}
	}
	m.mu.RUnlock()
	var result error
	for _, metadata := range pending {
		ready, err := m.reconcileDataPlaneOnce(ctx, metadata)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("Sandbox %s: %w", metadata.SandboxID, err))
		} else if !ready {
			result = errors.Join(result, fmt.Errorf("Sandbox %s data plane is still initializing", metadata.SandboxID))
		}
	}
	return result
}
