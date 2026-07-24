package sandbox

import fastletapi "fast-sandbox/internal/protocol/fastlet"

const (
	maxDiagnosticSandboxes = 1024
	maxDiagnosticEvents    = 128
	defaultDiagnosticLimit = 50
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
		status := sandboxStatus(metadata)
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
