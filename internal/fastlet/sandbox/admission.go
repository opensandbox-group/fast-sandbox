package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (m *SandboxManager) CreateSandbox(ctx context.Context, req *fastletapi.CreateSandboxRequest) (_ *fastletapi.CreateSandboxResponse, resultErr error) {
	if req != nil {
		ctx = observability.WithIdentity(ctx, observability.Identity{
			RequestID: req.Identity.RequestID, Namespace: req.Sandbox.ClaimNamespace, SandboxName: req.Sandbox.ClaimName,
			SandboxUID: req.Identity.SandboxUID, FastletPodUID: req.Identity.FastletPodUID,
			InstanceGeneration: req.Identity.InstanceGeneration, AssignmentAttempt: req.Identity.AssignmentAttempt,
			RouteGeneration: req.Identity.RouteGeneration,
		})
	}
	ctx, span := observability.Start(ctx, "fastlet.create Sandbox")
	started := time.Now()
	defer func() {
		observability.End(span, resultErr)
		recordAdmission("create", resultErr)
	}()
	if failure := m.validateCreateRequest(req); failure != nil {
		return createFailure(failure, fastletapi.AdmissionStatus{})
	}
	spec := req.Sandbox
	spec.SandboxID = req.Identity.SandboxUID
	spec.RequestID = req.Identity.RequestID
	spec.InstanceGeneration = req.Identity.InstanceGeneration
	spec.RuntimeInstanceID = req.Identity.RuntimeInstanceID
	spec.AssignmentAttempt = req.Identity.AssignmentAttempt
	spec.RouteGeneration = req.Identity.RouteGeneration
	if spec.RouteGeneration <= 0 {
		spec.RouteGeneration = 1
	}
	spec.FastletPodUID = req.Identity.FastletPodUID
	if err := m.validateProfiles(&spec); err != nil {
		return createFailure(fastletErrorWithOutcome(fastletapi.ErrorProfileMismatch, err.Error(), false, fastletapi.OutcomeRejectedBeforeSideEffects), fastletapi.AdmissionStatus{})
	}

	m.mu.Lock()
	if m.recovering || !m.runtimeReady {
		response, err := createFailure(fastletErrorWithOutcome(fastletapi.ErrorRuntimeUnavailable, "Fastlet runtime recovery/capability probe is incomplete", true, fastletapi.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !m.infraReady {
		message := m.infraMessage
		if message == "" {
			message = "required InfraProfile artifacts are still preparing"
		}
		response, err := createFailure(fastletErrorWithOutcome(fastletapi.ErrorInfraUnavailable, message, true, fastletapi.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if existing := m.sandboxes[spec.SandboxID]; existing != nil {
		if existing.Phase == "create-cleanup-failed" {
			return m.retryFailedCreateCleanup(ctx, req, &spec, existing)
		}
		response, err := m.createExistingLocked(existing, &spec)
		m.mu.Unlock()
		return response, err
	}
	if tombstone, found := m.tombstones[spec.SandboxID]; found && identityAtOrBefore(spec.InstanceGeneration, spec.AssignmentAttempt, tombstone) {
		response, err := createFailure(fastletErrorWithOutcome(fastletapi.ErrorGenerationFenced, "Sandbox generation was already deleted", false, fastletapi.OutcomeGenerationFenced), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if m.draining {
		response, err := createFailure(fastletErrorWithOutcome(fastletapi.ErrorDraining, m.drainReason, true, fastletapi.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if len(m.sandboxes) >= m.capacity {
		response, err := createFailure(fastletErrorWithOutcome(fastletapi.ErrorCapacityRejected, "Fastlet admission capacity is exhausted", true, fastletapi.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !m.runtimeResourceAvailable() {
		response, err := createFailure(fastletErrorWithOutcome(fastletapi.ErrorNetworkUnavailable, "Fastlet has no clean runtime/network resource available", true, fastletapi.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}

	placeholder := &SandboxMetadata{SandboxSpec: spec, Phase: "creating", CreatedAt: m.clock.Now().Unix()}
	m.sandboxes[spec.SandboxID] = placeholder
	admission := m.admissionStatusLocked()
	m.mu.Unlock()
	m.recordDiagnostic(spec.SandboxID, "info", "admission", "creating", "Fastlet admission accepted; atomic runtime creation started")

	runtimeStarted := time.Now()
	metadata, err := m.runtime.EnsureSandbox(ctx, &spec)
	observeRuntimeCreate(m.runtimeName, runtimeStarted, err)
	observeUserProcessStart(m.runtimeName, m.infraProfile, started, metadata)
	if err != nil {
		m.cacheProtection.ProtectHotUntil(spec.Image, m.clock.Now().Add(time.Hour))
		cleanupErr := m.runtime.DeleteSandbox(ctx, spec.SandboxID)
		m.mu.Lock()
		outcome := fastletapi.OutcomeRejectedBeforeSideEffects
		if cleanupErr == nil && m.sandboxes[spec.SandboxID] == placeholder {
			delete(m.sandboxes, spec.SandboxID)
		} else if m.sandboxes[spec.SandboxID] == placeholder {
			placeholder.Phase = "create-cleanup-failed"
			outcome = fastletapi.OutcomeFailedNeedsCleanup
		}
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		code := fastletapi.ErrorRuntimeUnavailable
		if errors.Is(err, ErrNetworkUnavailable) {
			code = fastletapi.ErrorNetworkUnavailable
		} else if errors.Is(err, ErrInfraUnavailable) {
			code = fastletapi.ErrorInfraUnavailable
		}
		failureMessage := err.Error()
		if cleanupErr != nil {
			failureMessage = fmt.Sprintf("%s; cleanup failed: %v", failureMessage, cleanupErr)
		}
		m.recordDiagnostic(spec.SandboxID, "error", "runtime", string(outcome), failureMessage)
		return createFailure(fastletErrorWithCauseAndOutcome(code, failureMessage, true, errors.Join(err, cleanupErr), outcome), admission)
	}
	runtimeSpec := metadata.SandboxSpec
	metadata.Phase = "infra-pending"
	metadata.SandboxSpec = spec
	metadata.NetworkSlotID = runtimeSpec.NetworkSlotID
	metadata.NetworkNamespacePath = runtimeSpec.NetworkNamespacePath
	metadata.NetworkIP = runtimeSpec.NetworkIP
	metadata.NetworkGateway = runtimeSpec.NetworkGateway
	metadata.NetworkDNSPath = runtimeSpec.NetworkDNSPath
	m.cacheProtection.Protect(spec.Image, fastletcache.ProtectActive)
	m.mu.Lock()
	if placeholder.Phase == "terminating" {
		metadata.Phase = "terminating"
		m.sandboxes[spec.SandboxID] = metadata
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		go m.asyncDelete(spec.SandboxID, metadata)
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox was deleted while creation was in progress", false), admission)
	}
	m.sandboxes[spec.SandboxID] = metadata
	if m.infraManager == nil && m.routePublisher == nil {
		metadata.Phase = "running"
		m.recordDiagnosticLocked(spec.SandboxID, "info", "fastlet", "running", "runtime is ready; no asynchronous data-plane initialization is required")
	}
	status := sandboxStatus(metadata)
	admission = m.admissionStatusLocked()
	dataPlaneReady := metadata.Phase == "running"
	m.mu.Unlock()
	if dataPlaneReady {
		observeDataPlaneReady(m.runtimeName, m.infraProfile, started, nil)
	} else {
		m.recordDiagnostic(spec.SandboxID, "info", "runtime", "infra-pending", "runtime and private network are ready; Infra Component initialization continues asynchronously")
		m.startDataPlaneReconcile(metadata, started)
	}
	return &fastletapi.CreateSandboxResponse{Accepted: true, Created: true, Sandbox: &status, Admission: admission}, nil
}

// retryFailedCreateCleanup resumes only cleanup that belongs to a failed
// Create attempt. A user-requested delete uses the distinct delete-failed
// phase and can never be resurrected by a delayed Create retry.
//
// m.mu must be held on entry. This method releases it before returning.
func (m *SandboxManager) retryFailedCreateCleanup(ctx context.Context, req *fastletapi.CreateSandboxRequest, requested *fastletapi.SandboxSpec, existing *SandboxMetadata) (*fastletapi.CreateSandboxResponse, error) {
	identity := fastletapi.SandboxIdentity{
		RequestID: requested.RequestID, SandboxUID: requested.SandboxID,
		InstanceGeneration: requested.InstanceGeneration, RuntimeInstanceID: requested.RuntimeInstanceID,
		AssignmentAttempt: requested.AssignmentAttempt, RouteGeneration: requested.RouteGeneration,
		FastletPodUID: requested.FastletPodUID,
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
		response, err := createFailure(failure, m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !sameSandboxClaim(existing, requested) {
		response, err := createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	existing.Phase = "create-cleanup"
	m.mu.Unlock()

	cleanupErr := m.runtime.DeleteSandbox(ctx, requested.SandboxID)
	m.mu.Lock()
	if m.sandboxes[requested.SandboxID] != existing {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox changed while failed Create cleanup was retried", true), admission)
	}
	if cleanupErr != nil {
		existing.Phase = "create-cleanup-failed"
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		message := fmt.Sprintf("retry failed Create cleanup: %v", cleanupErr)
		m.recordDiagnostic(requested.SandboxID, "error", "runtime", string(fastletapi.OutcomeFailedNeedsCleanup), message)
		return createFailure(fastletErrorWithCauseAndOutcome(fastletapi.ErrorRuntimeUnavailable, message, true, cleanupErr, fastletapi.OutcomeFailedNeedsCleanup), admission)
	}
	delete(m.sandboxes, requested.SandboxID)
	m.mu.Unlock()
	m.recordDiagnostic(requested.SandboxID, "info", "runtime", "cleanup-recovered", "failed Create cleanup converged; retrying the same runtime identity")
	return m.CreateSandbox(ctx, req)
}

func (m *SandboxManager) InspectSandboxV2(req *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
	if failure := m.validateIdentityTarget(reqIdentity(req)); failure != nil {
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata == nil {
		failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	status := sandboxStatus(metadata)
	return &fastletapi.InspectSandboxResponse{Sandbox: &status}, nil
}

func (m *SandboxManager) DeleteSandboxV2(req *fastletapi.DeleteSandboxV2Request) (*fastletapi.DeleteSandboxV2Response, error) {
	if failure := m.validateIdentityTarget(deleteIdentity(req)); failure != nil {
		return &fastletapi.DeleteSandboxV2Response{Error: failure}, failure
	}
	m.mu.Lock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
			m.mu.Unlock()
			return &fastletapi.DeleteSandboxV2Response{Error: failure}, failure
		}
	}
	m.recordTombstoneLocked(req.Identity)
	m.recordDiagnosticLocked(req.Identity.SandboxUID, "info", "admission", "terminating", "declarative deletion accepted")
	m.mu.Unlock()
	m.beginDelete(req.Identity.SandboxUID)
	return &fastletapi.DeleteSandboxV2Response{Accepted: true}, nil
}

func (m *SandboxManager) Recover(ctx context.Context) error {
	m.mu.Lock()
	m.recovering = true
	m.runtimeReady = false
	m.routeReady = m.routePublisher == nil
	m.mu.Unlock()

	managed, err := m.runtime.ListManagedSandboxes(ctx)
	if err != nil {
		return err
	}
	report := m.runtime.ProbeCapabilities(ctx)
	if report.State != runtimecatalog.CapabilityReady {
		return fmt.Errorf("runtime capability is not ready: %s: %s", report.Reason, report.Message)
	}
	if recoverer, ok := m.runtime.(RuntimeResourceRecoverer); ok {
		if err := recoverer.RecoverRuntimeResources(ctx, managed); err != nil {
			return fmt.Errorf("recover runtime resources: %w", err)
		}
	}
	recovered := make(map[string]*SandboxMetadata, len(managed))
	for _, metadata := range managed {
		if metadata == nil || metadata.SandboxID == "" {
			continue
		}
		if m.fastletPodUID != "" && metadata.FastletPodUID != "" && metadata.FastletPodUID != m.fastletPodUID {
			continue
		}
		if metadata.InstanceGeneration <= 0 {
			metadata.InstanceGeneration = 1
		}
		if metadata.RuntimeInstanceID == "" {
			metadata.RuntimeInstanceID = "legacy-" + metadata.SandboxID
		}
		if metadata.AssignmentAttempt <= 0 {
			metadata.AssignmentAttempt = 1
		}
		if metadata.RouteGeneration <= 0 {
			metadata.RouteGeneration = 1
		}
		if metadata.Phase == "" {
			metadata.Phase = "unknown"
		}
		if m.infraManager != nil {
			if metadata.InfraProfile != m.infraProfile || metadata.InfraProfileHash != m.infraProfileHash {
				return fmt.Errorf("recovered Sandbox %s InfraProfile does not match Fastlet", metadata.SandboxID)
			}
			metadata.Phase = "infra-pending"
		}
		recovered[metadata.SandboxID] = metadata
	}
	if len(recovered) > m.capacity {
		return fmt.Errorf("recovered %d Sandboxes exceeds Fastlet capacity %d", len(recovered), m.capacity)
	}
	publications := make([]RoutePublication, 0, len(recovered))
	pendingInfra := false
	for _, metadata := range recovered {
		if m.infraManager != nil {
			pendingInfra = true
			continue
		}
		publication, err := m.routePublication(metadata)
		if err != nil {
			return fmt.Errorf("recover route for Sandbox %s: %w", metadata.SandboxID, err)
		}
		if m.routePublisher != nil {
			publications = append(publications, publication)
		}
	}
	if m.routePublisher != nil && !pendingInfra {
		if err := m.routePublisher.ReconcileRoutes(ctx, publications); err != nil {
			return fmt.Errorf("reconcile Fastlet Proxy routes: %w", err)
		}
	}
	activeImages := make([]string, 0, len(recovered))
	for _, metadata := range recovered {
		activeImages = append(activeImages, metadata.Image)
	}
	m.cacheProtection.Replace(fastletcache.ProtectActive, activeImages)
	m.mu.Lock()
	m.sandboxes = recovered
	m.recovering = false
	m.runtimeReady = true
	m.routeReady = m.routePublisher == nil || !pendingInfra
	m.mu.Unlock()
	return nil
}

func (m *SandboxManager) runtimeResourceAvailable() bool {
	admission, ok := m.runtime.(RuntimeResourceAdmission)
	return !ok || admission.RuntimeResourceAvailable()
}

func (m *SandboxManager) SetDraining(draining bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining = draining
	m.drainReason = reason
}

func (m *SandboxManager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.recovering && m.runtimeReady && m.routeReady && !m.draining
}

func (m *SandboxManager) RuntimeReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.recovering && m.runtimeReady
}

func (m *SandboxManager) State() (fastletapi.AdmissionStatus, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admissionStatusLocked(), m.recovering, m.draining
}

func (m *SandboxManager) createExistingLocked(existing *SandboxMetadata, requested *fastletapi.SandboxSpec) (*fastletapi.CreateSandboxResponse, error) {
	identity := fastletapi.SandboxIdentity{
		RequestID: requested.RequestID, SandboxUID: requested.SandboxID,
		InstanceGeneration: requested.InstanceGeneration, RuntimeInstanceID: requested.RuntimeInstanceID, AssignmentAttempt: requested.AssignmentAttempt,
		RouteGeneration: requested.RouteGeneration, FastletPodUID: requested.FastletPodUID,
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
		return createFailure(failure, m.admissionStatusLocked())
	}
	if !sameSandboxClaim(existing, requested) {
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
	}
	status := sandboxStatus(existing)
	if existing.Phase == "creating" {
		failure := fastletError(fastletapi.ErrorInProgress, "Sandbox creation is already in progress", true)
		return &fastletapi.CreateSandboxResponse{Accepted: true, InProgress: true, Sandbox: &status, Admission: m.admissionStatusLocked(), Error: failure}, failure
	}
	if existing.Phase == "terminating" || existing.Phase == "deleting" {
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox deletion is already in progress", true), m.admissionStatusLocked())
	}
	switch existing.Phase {
	case "infra-pending", "initializing-infra", "infra-unavailable", "route-pending", "publishing-route", "route-unavailable":
		return &fastletapi.CreateSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
	}
	if existing.Phase != "running" {
		return createFailure(fastletError(fastletapi.ErrorRuntimeUnavailable, fmt.Sprintf("managed Sandbox runtime is %s, not running", existing.Phase), true), m.admissionStatusLocked())
	}
	return &fastletapi.CreateSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) validateIdentityTarget(identity *fastletapi.SandboxIdentity) *fastletapi.FastletError {
	if identity == nil || identity.SandboxUID == "" || identity.InstanceGeneration <= 0 || identity.RuntimeInstanceID == "" || identity.AssignmentAttempt <= 0 {
		return fastletError(fastletapi.ErrorConflict, "sandboxUid, runtimeInstanceId, positive instanceGeneration, and positive assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && identity.FastletPodUID != m.fastletPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	return nil
}

func reqIdentity(req *fastletapi.InspectSandboxRequest) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func deleteIdentity(req *fastletapi.DeleteSandboxV2Request) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func validateIdentityFence(expectedPodUID string, existing *SandboxMetadata, requested fastletapi.SandboxIdentity) *fastletapi.FastletError {
	if expectedPodUID != "" && requested.FastletPodUID != expectedPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if requested.InstanceGeneration < existing.InstanceGeneration ||
		(requested.InstanceGeneration == existing.InstanceGeneration && requested.AssignmentAttempt < existing.AssignmentAttempt) {
		return fastletError(fastletapi.ErrorStaleGeneration, "request generation/assignment attempt is older than the managed Sandbox", false)
	}
	if requested.InstanceGeneration > existing.InstanceGeneration || requested.AssignmentAttempt > existing.AssignmentAttempt {
		return fastletError(fastletapi.ErrorConflict, "newer generation/assignment requires the old runtime to be deleted first", true)
	}
	if requested.RuntimeInstanceID != existing.RuntimeInstanceID {
		return fastletError(fastletapi.ErrorConflict, "runtimeInstanceId conflicts with the managed Sandbox", false)
	}
	requestedRouteGeneration := requested.RouteGeneration
	if requestedRouteGeneration <= 0 {
		requestedRouteGeneration = existing.RouteGeneration
	}
	if requestedRouteGeneration < existing.RouteGeneration {
		return fastletError(fastletapi.ErrorStaleGeneration, "request route generation is older than the managed Sandbox", false)
	}
	if requestedRouteGeneration > existing.RouteGeneration {
		return fastletError(fastletapi.ErrorConflict, "newer route generation requires the old runtime to be deleted first", true)
	}
	return nil
}

func sameSandboxClaim(existing *SandboxMetadata, requested *fastletapi.SandboxSpec) bool {
	return existing.ClaimUID == requested.ClaimUID && existing.ClaimNamespace == requested.ClaimNamespace && existing.ClaimName == requested.ClaimName &&
		existing.RuntimeProfileHash == requested.RuntimeProfileHash && existing.ResourceProfileHash == requested.ResourceProfileHash &&
		existing.InfraProfile == requested.InfraProfile && existing.InfraProfileHash == requested.InfraProfileHash
}

func identityAtOrBefore(generation, attempt int64, highWater fastletapi.SandboxIdentity) bool {
	return generation < highWater.InstanceGeneration ||
		(generation == highWater.InstanceGeneration && attempt <= highWater.AssignmentAttempt)
}

func (m *SandboxManager) recordTombstoneLocked(identity fastletapi.SandboxIdentity) {
	current, found := m.tombstones[identity.SandboxUID]
	if !found || current.InstanceGeneration < identity.InstanceGeneration ||
		(current.InstanceGeneration == identity.InstanceGeneration && current.AssignmentAttempt < identity.AssignmentAttempt) {
		m.tombstones[identity.SandboxUID] = identity
	}
}

func (m *SandboxManager) validateCreateRequest(req *fastletapi.CreateSandboxRequest) *fastletapi.FastletError {
	if req == nil || req.Identity.SandboxUID == "" || req.Identity.InstanceGeneration <= 0 || req.Identity.RuntimeInstanceID == "" || req.Identity.AssignmentAttempt <= 0 {
		return fastletError(fastletapi.ErrorConflict, "sandboxUid, runtimeInstanceId, positive instanceGeneration, and positive assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && req.Identity.FastletPodUID != m.fastletPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if req.Sandbox.SandboxID != "" && req.Sandbox.SandboxID != req.Identity.SandboxUID {
		return fastletError(fastletapi.ErrorConflict, "sandboxId must be empty or equal sandboxUid", false)
	}
	return nil
}

func (m *SandboxManager) admissionStatusLocked() fastletapi.AdmissionStatus {
	status := fastletapi.AdmissionStatus{Capacity: m.capacity}
	for _, metadata := range m.sandboxes {
		switch metadata.Phase {
		case "creating", "infra-pending", "initializing-infra", "infra-unavailable", "route-pending", "publishing-route", "route-unavailable":
			status.Creating++
		case "terminating", "deleting", "delete-failed", "create-cleanup", "create-cleanup-failed":
			status.Deleting++
		default:
			status.Running++
		}
	}
	status.Used = status.Creating + status.Running + status.Deleting
	recordAdmissionStatus(status)
	return status
}

func sandboxStatus(metadata *SandboxMetadata) fastletapi.SandboxStatus {
	return fastletapi.SandboxStatus{
		SandboxID: metadata.SandboxID, ClaimUID: metadata.ClaimUID,
		InstanceGeneration: metadata.InstanceGeneration, RuntimeInstanceID: metadata.RuntimeInstanceID, AssignmentAttempt: metadata.AssignmentAttempt,
		RouteGeneration: metadata.RouteGeneration,
		Phase:           metadata.Phase, CreatedAt: metadata.CreatedAt, InfraDiagnostics: apiInfraDiagnostics(metadata.InfraDiagnostics),
	}
}

func apiInfraDiagnostics(diagnostics []fastletinfra.ComponentDiagnostic) []fastletapi.InfraComponentDiagnostic {
	result := make([]fastletapi.InfraComponentDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		result = append(result, fastletapi.InfraComponentDiagnostic{
			Component: diagnostic.Component, Service: diagnostic.Service, Required: diagnostic.Required,
			State: diagnostic.State, Message: diagnostic.Message,
		})
	}
	return result
}

func fastletError(code fastletapi.FastletErrorCode, message string, retryable bool) *fastletapi.FastletError {
	return fastletErrorWithOutcome(code, message, retryable, defaultOutcome(code))
}

func fastletErrorWithCause(code fastletapi.FastletErrorCode, message string, retryable bool, cause error) *fastletapi.FastletError {
	return fastletErrorWithCauseAndOutcome(code, message, retryable, cause, defaultOutcome(code))
}

func fastletErrorWithOutcome(code fastletapi.FastletErrorCode, message string, retryable bool, outcome fastletapi.FastletOutcome) *fastletapi.FastletError {
	return &fastletapi.FastletError{Code: code, Message: message, Retryable: retryable, Outcome: outcome}
}

func fastletErrorWithCauseAndOutcome(code fastletapi.FastletErrorCode, message string, retryable bool, cause error, outcome fastletapi.FastletOutcome) *fastletapi.FastletError {
	return &fastletapi.FastletError{Code: code, Message: message, Retryable: retryable, Outcome: outcome, Cause: cause}
}

func defaultOutcome(code fastletapi.FastletErrorCode) fastletapi.FastletOutcome {
	switch code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorProfileMismatch:
		return fastletapi.OutcomeRejectedBeforeSideEffects
	case fastletapi.ErrorInProgress:
		return fastletapi.OutcomeInProgress
	case fastletapi.ErrorGenerationFenced, fastletapi.ErrorStaleGeneration:
		return fastletapi.OutcomeGenerationFenced
	default:
		return fastletapi.OutcomeUnknown
	}
}

func createFailure(failure *fastletapi.FastletError, admission fastletapi.AdmissionStatus) (*fastletapi.CreateSandboxResponse, error) {
	return &fastletapi.CreateSandboxResponse{Admission: admission, Error: failure}, failure
}
