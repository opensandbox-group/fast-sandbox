package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
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
					preamble, encodeErr := fastletnetwork.EncodeLocalForwardPreamble(targetPort)
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

// ReconcilePendingInfra is called after profile artifacts become Prepared and
// on subsequent retries. Recovered runtimes remain unrouted until their
// instance init/readiness succeeds.
func (m *SandboxManager) ReconcilePendingInfra(ctx context.Context) error {
	m.mu.RLock()
	pending := make([]*SandboxMetadata, 0)
	for _, metadata := range m.sandboxes {
		if metadata.Phase == "infra-pending" {
			pending = append(pending, metadata)
		}
	}
	m.mu.RUnlock()
	var result error
	for _, metadata := range pending {
		m.mu.Lock()
		if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "infra-pending" {
			m.mu.Unlock()
			continue
		}
		metadata.Phase = "initializing-infra"
		m.mu.Unlock()
		if err := m.initializeInfraInstance(ctx, metadata); err != nil {
			m.mu.Lock()
			if m.sandboxes[metadata.SandboxID] == metadata && metadata.Phase == "initializing-infra" {
				metadata.Phase = "infra-pending"
			}
			m.mu.Unlock()
			result = errors.Join(result, fmt.Errorf("Sandbox %s: %w", metadata.SandboxID, err))
			continue
		}
		m.mu.Lock()
		if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "initializing-infra" {
			m.mu.Unlock()
			continue
		}
		metadata.Phase = "route-pending"
		m.mu.Unlock()
		if err := m.publishRoute(ctx, metadata); err != nil {
			result = errors.Join(result, fmt.Errorf("Sandbox %s route: %w", metadata.SandboxID, err))
			continue
		}
		m.mu.Lock()
		if m.sandboxes[metadata.SandboxID] == metadata && metadata.Phase == "route-pending" {
			metadata.Phase = "running"
		}
		m.mu.Unlock()
	}
	return result
}
