package sandbox

import (
	"context"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const (
	maxDiagnosticSandboxes = 1024
	maxDiagnosticEvents    = 128
	defaultDiagnosticLimit = 50
	// diagnosticLevelError marks a diagnostic event that reports a failure.
	diagnosticLevelError = "error"
)

func (m *SandboxManager) recordDiagnostic(sandboxID, level, source, phase, message string) {
	if sandboxID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordDiagnosticLocked(sandboxID, level, source, phase, message)
}

func (m *SandboxManager) recordDiagnosticLocked(sandboxID, level, source, phase, message string) {
	events, found := m.diagnostics[sandboxID]
	if !found {
		if len(m.diagnostics) >= maxDiagnosticSandboxes && len(m.diagnosticOrder) > 0 {
			oldest := m.diagnosticOrder[0]
			m.diagnosticOrder = m.diagnosticOrder[1:]
			delete(m.diagnostics, oldest)
		}
		m.diagnosticOrder = append(m.diagnosticOrder, sandboxID)
	}
	events = append(events, fastletapi.SandboxDiagnosticEvent{
		Timestamp: m.clock.Now(), Level: level, Source: source, Phase: phase, Message: message,
	})
	if len(events) > maxDiagnosticEvents {
		events = append([]fastletapi.SandboxDiagnosticEvent(nil), events[len(events)-maxDiagnosticEvents:]...)
	}
	m.diagnostics[sandboxID] = events
	m.signalReadinessChangedLocked()
}

func (m *SandboxManager) signalReadinessChangedLocked() {
	if m.readinessChanged == nil {
		m.readinessChanged = make(chan struct{})
		return
	}
	close(m.readinessChanged)
	m.readinessChanged = make(chan struct{})
}

// waitUntilSandboxReady is the single Fastlet-local READY completion barrier.
// It derives readiness from the complete current observation rather than
// accepting a second caller-provided component or Binding selector.
func (m *SandboxManager) waitUntilSandboxReady(ctx context.Context, identity fastletapi.SandboxIdentity, expectedGeneration int64) (*fastletapi.SandboxStatus, error) {
	if failure := m.validateIdentityTarget(&identity); failure != nil {
		return nil, failure
	}
	for {
		m.mu.Lock()
		metadata := m.sandboxes[identity.SandboxUID]
		if metadata == nil {
			m.mu.Unlock()
			failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
			return nil, failure
		}
		if failure := validateIdentityFence(m.fastletPodUID, metadata, identity); failure != nil {
			m.mu.Unlock()
			return nil, failure
		}
		status := m.sandboxStatusLocked(metadata)
		if m.actionManager != nil {
			status.ActionBindings, status.AppliedGeneration = m.actionManager.Statuses(identity.SandboxUID)
		}
		ready := sandboxObservationReady(&status, metadata, expectedGeneration, m.routePublisher == nil || m.routeReady)
		if ready {
			m.mu.Unlock()
			return &status, nil
		}
		switch metadata.Phase {
		case sandboxStateTerminating, sandboxStateDeleting, sandboxStateDeleteFailed, sandboxStateCreateCleanup, sandboxStateCreateCleanupFailed:
			m.mu.Unlock()
			failure := fastletError(fastletapi.ErrorConflict, "Sandbox stopped before overall Ready", false)
			return &status, failure
		case sandboxStateCreateFailed, sandboxStateRuntimeFailureCleanup, sandboxStateRuntimeFailed:
			message := m.runtimeMessages[identity.SandboxUID]
			if message == "" {
				message = "Sandbox failed before overall Ready"
			}
			m.mu.Unlock()
			failure := fastletError(fastletapi.ErrorActionUnavailable, message, false)
			return &status, failure
		}
		changed := m.readinessChanged
		if changed == nil {
			m.readinessChanged = make(chan struct{})
			changed = m.readinessChanged
		}
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func sandboxObservationReady(status *fastletapi.SandboxStatus, metadata *SandboxMetadata, expectedGeneration int64, routeReady bool) bool {
	if status == nil || metadata == nil || metadata.Phase != sandboxStateRunning || !routeReady || status.AppliedGeneration < expectedGeneration {
		return false
	}
	for _, component := range status.InfraComponents {
		if component.State != string(apiv1alpha2.InfraComponentReady) || component.ObservedRouteGeneration != metadata.Config.Identity.RouteGeneration {
			return false
		}
	}
	return actionStatusesReady(status.ActionBindings)
}

func (m *SandboxManager) SandboxDiagnostics(req *fastletapi.SandboxDiagnosticsRequest) (*fastletapi.SandboxDiagnosticsResponse, error) {
	if failure := m.validateIdentityTarget(diagnosticsIdentity(req)); failure != nil {
		return &fastletapi.SandboxDiagnosticsResponse{Error: failure}, failure
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	identity := req.Identity
	metadata := m.sandboxes[identity.SandboxUID]
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, identity); failure != nil {
			return &fastletapi.SandboxDiagnosticsResponse{Error: failure}, failure
		}
	} else {
		tombstone, deleted := m.tombstones[identity.SandboxUID]
		if !deleted || tombstone.InstanceGeneration != identity.InstanceGeneration ||
			tombstone.AssignmentAttempt != identity.AssignmentAttempt || tombstone.RuntimeInstanceID != identity.RuntimeInstanceID {
			failure := fastletError(fastletapi.ErrorNotFound, "Sandbox diagnostics are not retained by this Fastlet", false)
			return &fastletapi.SandboxDiagnosticsResponse{Error: failure}, failure
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultDiagnosticLimit
	}
	if limit > maxDiagnosticEvents {
		limit = maxDiagnosticEvents
	}
	events := m.diagnostics[identity.SandboxUID]
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	response := &fastletapi.SandboxDiagnosticsResponse{Events: append([]fastletapi.SandboxDiagnosticEvent(nil), events...)}
	if metadata != nil {
		status := m.sandboxStatusLocked(metadata)
		response.Sandbox = &status
	}
	return response, nil
}

func diagnosticsIdentity(req *fastletapi.SandboxDiagnosticsRequest) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}
