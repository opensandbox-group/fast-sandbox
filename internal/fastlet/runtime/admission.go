package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fast-sandbox/internal/api"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/runtimecatalog"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (m *SandboxManager) CreateSandbox(ctx context.Context, req *api.CreateSandboxRequest) (_ *api.CreateSandboxResponse, resultErr error) {
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
		observeDataPlaneReady(m.runtimeName, m.infraProfile, started, resultErr)
	}()
	if failure := m.validateCreateRequest(req); failure != nil {
		return createFailure(failure, api.AdmissionStatus{})
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
		return createFailure(fastletErrorWithOutcome(api.ErrorProfileMismatch, err.Error(), false, api.OutcomeRejectedBeforeSideEffects), api.AdmissionStatus{})
	}

	m.mu.Lock()
	if m.recovering || !m.runtimeReady {
		response, err := createFailure(fastletErrorWithOutcome(api.ErrorRuntimeUnavailable, "Fastlet runtime recovery/capability probe is incomplete", true, api.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !m.infraReady {
		message := m.infraMessage
		if message == "" {
			message = "required InfraProfile artifacts are still preparing"
		}
		response, err := createFailure(fastletErrorWithOutcome(api.ErrorInfraUnavailable, message, true, api.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if existing := m.sandboxes[spec.SandboxID]; existing != nil {
		if existing.Phase == "create-cleanup-failed" {
			return m.retryFailedCreateCleanup(ctx, req, &spec, existing)
		}
		if existing.Phase == "infra-pending" {
			identity := api.SandboxIdentity{
				RequestID: spec.RequestID, SandboxUID: spec.SandboxID,
				InstanceGeneration: spec.InstanceGeneration, RuntimeInstanceID: spec.RuntimeInstanceID, AssignmentAttempt: spec.AssignmentAttempt,
				RouteGeneration: spec.RouteGeneration, FastletPodUID: spec.FastletPodUID,
			}
			if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
				response, err := createFailure(failure, m.admissionStatusLocked())
				m.mu.Unlock()
				return response, err
			}
			if !sameSandboxClaim(existing, &spec) {
				response, err := createFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
				m.mu.Unlock()
				return response, err
			}
			existing.Phase = "initializing-infra"
			m.mu.Unlock()
			infraErr := m.initializeInfraInstance(ctx, existing)
			m.mu.Lock()
			if m.sandboxes[spec.SandboxID] != existing || existing.Phase != "initializing-infra" {
				admission := m.admissionStatusLocked()
				m.mu.Unlock()
				return createFailure(fastletError(api.ErrorConflict, "Sandbox changed while Infra Components were initializing", true), admission)
			}
			if infraErr != nil {
				existing.Phase = "infra-pending"
				admission := m.admissionStatusLocked()
				m.mu.Unlock()
				return createFailure(fastletErrorWithCause(api.ErrorInProgress, infraErr.Error(), true, infraErr), admission)
			}
			existing.Phase = "route-pending"
			m.mu.Unlock()
			return m.retryRoutePublication(ctx, existing)
		}
		if existing.Phase == "route-pending" {
			identity := api.SandboxIdentity{
				RequestID: spec.RequestID, SandboxUID: spec.SandboxID,
				InstanceGeneration: spec.InstanceGeneration, RuntimeInstanceID: spec.RuntimeInstanceID, AssignmentAttempt: spec.AssignmentAttempt,
				RouteGeneration: spec.RouteGeneration, FastletPodUID: spec.FastletPodUID,
			}
			if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
				response, err := createFailure(failure, m.admissionStatusLocked())
				m.mu.Unlock()
				return response, err
			}
			if !sameSandboxClaim(existing, &spec) {
				response, err := createFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
				m.mu.Unlock()
				return response, err
			}
			existing.Phase = "publishing-route"
			m.mu.Unlock()
			publishErr := m.publishRoute(ctx, existing)
			m.mu.Lock()
			if m.sandboxes[spec.SandboxID] != existing || existing.Phase != "publishing-route" {
				admission := m.admissionStatusLocked()
				m.mu.Unlock()
				return createFailure(fastletError(api.ErrorConflict, "Sandbox changed while its route was being published", true), admission)
			}
			if publishErr != nil {
				existing.Phase = "route-pending"
				admission := m.admissionStatusLocked()
				m.mu.Unlock()
				return createFailure(fastletErrorWithCause(api.ErrorInProgress, publishErr.Error(), true, publishErr), admission)
			}
			existing.Phase = "running"
			status := sandboxStatus(existing)
			admission := m.admissionStatusLocked()
			m.mu.Unlock()
			return &api.CreateSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: admission}, nil
		}
		response, err := m.createExistingLocked(existing, &spec)
		m.mu.Unlock()
		return response, err
	}
	if tombstone, found := m.tombstones[spec.SandboxID]; found && identityAtOrBefore(spec.InstanceGeneration, spec.AssignmentAttempt, tombstone) {
		response, err := createFailure(fastletErrorWithOutcome(api.ErrorGenerationFenced, "Sandbox generation was already deleted", false, api.OutcomeGenerationFenced), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if m.draining {
		response, err := createFailure(fastletErrorWithOutcome(api.ErrorDraining, m.drainReason, true, api.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if len(m.sandboxes) >= m.capacity {
		response, err := createFailure(fastletErrorWithOutcome(api.ErrorCapacityRejected, "Fastlet admission capacity is exhausted", true, api.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !m.runtimeResourceAvailable() {
		response, err := createFailure(fastletErrorWithOutcome(api.ErrorNetworkUnavailable, "Fastlet has no clean runtime/network resource available", true, api.OutcomeRejectedBeforeSideEffects), m.admissionStatusLocked())
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
		outcome := api.OutcomeRejectedBeforeSideEffects
		if cleanupErr == nil && m.sandboxes[spec.SandboxID] == placeholder {
			delete(m.sandboxes, spec.SandboxID)
		} else if m.sandboxes[spec.SandboxID] == placeholder {
			placeholder.Phase = "create-cleanup-failed"
			outcome = api.OutcomeFailedNeedsCleanup
		}
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		code := api.ErrorRuntimeUnavailable
		if errors.Is(err, ErrNetworkUnavailable) {
			code = api.ErrorNetworkUnavailable
		} else if errors.Is(err, ErrInfraUnavailable) {
			code = api.ErrorInfraUnavailable
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
		return createFailure(fastletError(api.ErrorConflict, "Sandbox was deleted while creation was in progress", false), admission)
	}
	m.sandboxes[spec.SandboxID] = metadata
	m.mu.Unlock()
	m.recordDiagnostic(spec.SandboxID, "info", "runtime", "infra-pending", "runtime and private network are ready; Infra Component initialization started")
	if err := m.initializeInfraInstance(ctx, metadata); err != nil {
		m.mu.Lock()
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		m.recordDiagnostic(spec.SandboxID, "error", "infra", "infra-pending", err.Error())
		return createFailure(fastletErrorWithCause(api.ErrorInProgress, err.Error(), true, err), admission)
	}
	m.recordDiagnostic(spec.SandboxID, "info", "infra", "route-pending", "required Infra Components are ready; proxy route publication started")

	m.mu.Lock()
	if m.sandboxes[spec.SandboxID] != metadata || metadata.Phase != "infra-pending" {
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(api.ErrorConflict, "Sandbox changed while Infra Components were initializing", true), admission)
	}
	metadata.Phase = "route-pending"
	m.mu.Unlock()

	if err := m.publishRoute(ctx, metadata); err != nil {
		m.mu.Lock()
		if m.sandboxes[spec.SandboxID] == metadata && metadata.Phase == "route-pending" {
			admission = m.admissionStatusLocked()
		}
		m.mu.Unlock()
		m.recordDiagnostic(spec.SandboxID, "error", "route", "route-pending", err.Error())
		return createFailure(fastletErrorWithCause(api.ErrorInProgress, err.Error(), true, err), admission)
	}

	m.mu.Lock()
	if m.sandboxes[spec.SandboxID] != metadata || metadata.Phase != "route-pending" {
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(api.ErrorConflict, "Sandbox changed while its route was being published", true), admission)
	}
	metadata.Phase = "running"
	status := sandboxStatus(metadata)
	admission = m.admissionStatusLocked()
	m.recordDiagnosticLocked(spec.SandboxID, "info", "fastlet", "running", "runtime, private network, Infra Components, and proxy route are ready")
	m.mu.Unlock()
	return &api.CreateSandboxResponse{Accepted: true, Created: true, Sandbox: &status, Admission: admission}, nil
}

// retryFailedCreateCleanup resumes only cleanup that belongs to a failed
// Create attempt. A user-requested delete uses the distinct delete-failed
// phase and can never be resurrected by a delayed Create retry.
//
// m.mu must be held on entry. This method releases it before returning.
func (m *SandboxManager) retryFailedCreateCleanup(ctx context.Context, req *api.CreateSandboxRequest, requested *api.SandboxSpec, existing *SandboxMetadata) (*api.CreateSandboxResponse, error) {
	identity := api.SandboxIdentity{
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
		response, err := createFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
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
		return createFailure(fastletError(api.ErrorConflict, "Sandbox changed while failed Create cleanup was retried", true), admission)
	}
	if cleanupErr != nil {
		existing.Phase = "create-cleanup-failed"
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		message := fmt.Sprintf("retry failed Create cleanup: %v", cleanupErr)
		m.recordDiagnostic(requested.SandboxID, "error", "runtime", string(api.OutcomeFailedNeedsCleanup), message)
		return createFailure(fastletErrorWithCauseAndOutcome(api.ErrorRuntimeUnavailable, message, true, cleanupErr, api.OutcomeFailedNeedsCleanup), admission)
	}
	delete(m.sandboxes, requested.SandboxID)
	m.mu.Unlock()
	m.recordDiagnostic(requested.SandboxID, "info", "runtime", "cleanup-recovered", "failed Create cleanup converged; retrying the same runtime identity")
	return m.CreateSandbox(ctx, req)
}

func (m *SandboxManager) InspectSandboxV2(req *api.InspectSandboxRequest) (*api.InspectSandboxResponse, error) {
	if failure := m.validateIdentityTarget(reqIdentity(req)); failure != nil {
		return &api.InspectSandboxResponse{Error: failure}, failure
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata == nil {
		failure := fastletError(api.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
		return &api.InspectSandboxResponse{Error: failure}, failure
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
		return &api.InspectSandboxResponse{Error: failure}, failure
	}
	status := sandboxStatus(metadata)
	return &api.InspectSandboxResponse{Sandbox: &status}, nil
}

func (m *SandboxManager) retryRoutePublication(ctx context.Context, metadata *SandboxMetadata) (*api.CreateSandboxResponse, error) {
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "route-pending" {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(api.ErrorConflict, "Sandbox changed before its route could be published", true), admission)
	}
	metadata.Phase = "publishing-route"
	m.mu.Unlock()
	publishErr := m.publishRoute(ctx, metadata)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "publishing-route" {
		return createFailure(fastletError(api.ErrorConflict, "Sandbox changed while its route was being published", true), m.admissionStatusLocked())
	}
	if publishErr != nil {
		metadata.Phase = "route-pending"
		return createFailure(fastletErrorWithCause(api.ErrorInProgress, publishErr.Error(), true, publishErr), m.admissionStatusLocked())
	}
	metadata.Phase = "running"
	status := sandboxStatus(metadata)
	return &api.CreateSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) DeleteSandboxV2(req *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error) {
	if failure := m.validateIdentityTarget(deleteIdentity(req)); failure != nil {
		return &api.DeleteSandboxV2Response{Error: failure}, failure
	}
	m.mu.Lock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
			m.mu.Unlock()
			return &api.DeleteSandboxV2Response{Error: failure}, failure
		}
	}
	m.recordTombstoneLocked(req.Identity)
	m.recordDiagnosticLocked(req.Identity.SandboxUID, "info", "admission", "terminating", "declarative deletion accepted")
	m.mu.Unlock()
	m.beginDelete(req.Identity.SandboxUID)
	return &api.DeleteSandboxV2Response{Accepted: true}, nil
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

func (m *SandboxManager) State() (api.AdmissionStatus, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admissionStatusLocked(), m.recovering, m.draining
}

func (m *SandboxManager) createExistingLocked(existing *SandboxMetadata, requested *api.SandboxSpec) (*api.CreateSandboxResponse, error) {
	identity := api.SandboxIdentity{
		RequestID: requested.RequestID, SandboxUID: requested.SandboxID,
		InstanceGeneration: requested.InstanceGeneration, RuntimeInstanceID: requested.RuntimeInstanceID, AssignmentAttempt: requested.AssignmentAttempt,
		RouteGeneration: requested.RouteGeneration, FastletPodUID: requested.FastletPodUID,
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
		return createFailure(failure, m.admissionStatusLocked())
	}
	if !sameSandboxClaim(existing, requested) {
		return createFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
	}
	status := sandboxStatus(existing)
	if existing.Phase == "creating" || existing.Phase == "initializing-infra" || existing.Phase == "publishing-route" {
		failure := fastletError(api.ErrorInProgress, "Sandbox creation is already in progress", true)
		return &api.CreateSandboxResponse{Accepted: true, InProgress: true, Sandbox: &status, Admission: m.admissionStatusLocked(), Error: failure}, failure
	}
	if existing.Phase == "terminating" || existing.Phase == "deleting" {
		return createFailure(fastletError(api.ErrorConflict, "Sandbox deletion is already in progress", true), m.admissionStatusLocked())
	}
	if existing.Phase != "running" {
		return createFailure(fastletError(api.ErrorRuntimeUnavailable, fmt.Sprintf("managed Sandbox runtime is %s, not running", existing.Phase), true), m.admissionStatusLocked())
	}
	return &api.CreateSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) validateIdentityTarget(identity *api.SandboxIdentity) *api.FastletError {
	if identity == nil || identity.SandboxUID == "" || identity.InstanceGeneration <= 0 || identity.RuntimeInstanceID == "" || identity.AssignmentAttempt <= 0 {
		return fastletError(api.ErrorConflict, "sandboxUid, runtimeInstanceId, positive instanceGeneration, and positive assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && identity.FastletPodUID != m.fastletPodUID {
		return fastletError(api.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	return nil
}

func reqIdentity(req *api.InspectSandboxRequest) *api.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func deleteIdentity(req *api.DeleteSandboxV2Request) *api.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func validateIdentityFence(expectedPodUID string, existing *SandboxMetadata, requested api.SandboxIdentity) *api.FastletError {
	if expectedPodUID != "" && requested.FastletPodUID != expectedPodUID {
		return fastletError(api.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if requested.InstanceGeneration < existing.InstanceGeneration ||
		(requested.InstanceGeneration == existing.InstanceGeneration && requested.AssignmentAttempt < existing.AssignmentAttempt) {
		return fastletError(api.ErrorStaleGeneration, "request generation/assignment attempt is older than the managed Sandbox", false)
	}
	if requested.InstanceGeneration > existing.InstanceGeneration || requested.AssignmentAttempt > existing.AssignmentAttempt {
		return fastletError(api.ErrorConflict, "newer generation/assignment requires the old runtime to be deleted first", true)
	}
	if requested.RuntimeInstanceID != existing.RuntimeInstanceID {
		return fastletError(api.ErrorConflict, "runtimeInstanceId conflicts with the managed Sandbox", false)
	}
	requestedRouteGeneration := requested.RouteGeneration
	if requestedRouteGeneration <= 0 {
		requestedRouteGeneration = existing.RouteGeneration
	}
	if requestedRouteGeneration < existing.RouteGeneration {
		return fastletError(api.ErrorStaleGeneration, "request route generation is older than the managed Sandbox", false)
	}
	if requestedRouteGeneration > existing.RouteGeneration {
		return fastletError(api.ErrorConflict, "newer route generation requires the old runtime to be deleted first", true)
	}
	return nil
}

func sameSandboxClaim(existing *SandboxMetadata, requested *api.SandboxSpec) bool {
	return existing.ClaimUID == requested.ClaimUID && existing.ClaimNamespace == requested.ClaimNamespace && existing.ClaimName == requested.ClaimName &&
		existing.RuntimeProfileHash == requested.RuntimeProfileHash && existing.ResourceProfileHash == requested.ResourceProfileHash &&
		existing.InfraProfile == requested.InfraProfile && existing.InfraProfileHash == requested.InfraProfileHash
}

func identityAtOrBefore(generation, attempt int64, highWater api.SandboxIdentity) bool {
	return generation < highWater.InstanceGeneration ||
		(generation == highWater.InstanceGeneration && attempt <= highWater.AssignmentAttempt)
}

func (m *SandboxManager) recordTombstoneLocked(identity api.SandboxIdentity) {
	current, found := m.tombstones[identity.SandboxUID]
	if !found || current.InstanceGeneration < identity.InstanceGeneration ||
		(current.InstanceGeneration == identity.InstanceGeneration && current.AssignmentAttempt < identity.AssignmentAttempt) {
		m.tombstones[identity.SandboxUID] = identity
	}
}

func (m *SandboxManager) validateCreateRequest(req *api.CreateSandboxRequest) *api.FastletError {
	if req == nil || req.Identity.SandboxUID == "" || req.Identity.InstanceGeneration <= 0 || req.Identity.RuntimeInstanceID == "" || req.Identity.AssignmentAttempt <= 0 {
		return fastletError(api.ErrorConflict, "sandboxUid, runtimeInstanceId, positive instanceGeneration, and positive assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && req.Identity.FastletPodUID != m.fastletPodUID {
		return fastletError(api.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if req.Sandbox.SandboxID != "" && req.Sandbox.SandboxID != req.Identity.SandboxUID {
		return fastletError(api.ErrorConflict, "sandboxId must be empty or equal sandboxUid", false)
	}
	return nil
}

func (m *SandboxManager) admissionStatusLocked() api.AdmissionStatus {
	status := api.AdmissionStatus{Capacity: m.capacity}
	for _, metadata := range m.sandboxes {
		switch metadata.Phase {
		case "creating", "infra-pending", "initializing-infra", "route-pending", "publishing-route":
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

func sandboxStatus(metadata *SandboxMetadata) api.SandboxStatus {
	return api.SandboxStatus{
		SandboxID: metadata.SandboxID, ClaimUID: metadata.ClaimUID,
		InstanceGeneration: metadata.InstanceGeneration, RuntimeInstanceID: metadata.RuntimeInstanceID, AssignmentAttempt: metadata.AssignmentAttempt,
		RouteGeneration: metadata.RouteGeneration,
		Phase:           metadata.Phase, CreatedAt: metadata.CreatedAt, InfraDiagnostics: apiInfraDiagnostics(metadata.InfraDiagnostics),
	}
}

func apiInfraDiagnostics(diagnostics []fastletinfra.ComponentDiagnostic) []api.InfraComponentDiagnostic {
	result := make([]api.InfraComponentDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		result = append(result, api.InfraComponentDiagnostic{
			Component: diagnostic.Component, Service: diagnostic.Service, Required: diagnostic.Required,
			State: diagnostic.State, Message: diagnostic.Message,
		})
	}
	return result
}

func fastletError(code api.FastletErrorCode, message string, retryable bool) *api.FastletError {
	return fastletErrorWithOutcome(code, message, retryable, defaultOutcome(code))
}

func fastletErrorWithCause(code api.FastletErrorCode, message string, retryable bool, cause error) *api.FastletError {
	return fastletErrorWithCauseAndOutcome(code, message, retryable, cause, defaultOutcome(code))
}

func fastletErrorWithOutcome(code api.FastletErrorCode, message string, retryable bool, outcome api.FastletOutcome) *api.FastletError {
	return &api.FastletError{Code: code, Message: message, Retryable: retryable, Outcome: outcome}
}

func fastletErrorWithCauseAndOutcome(code api.FastletErrorCode, message string, retryable bool, cause error, outcome api.FastletOutcome) *api.FastletError {
	return &api.FastletError{Code: code, Message: message, Retryable: retryable, Outcome: outcome, Cause: cause}
}

func defaultOutcome(code api.FastletErrorCode) api.FastletOutcome {
	switch code {
	case api.ErrorCapacityRejected, api.ErrorDraining, api.ErrorProfileMismatch:
		return api.OutcomeRejectedBeforeSideEffects
	case api.ErrorInProgress:
		return api.OutcomeInProgress
	case api.ErrorGenerationFenced, api.ErrorStaleGeneration:
		return api.OutcomeGenerationFenced
	default:
		return api.OutcomeUnknown
	}
}

func createFailure(failure *api.FastletError, admission api.AdmissionStatus) (*api.CreateSandboxResponse, error) {
	return &api.CreateSandboxResponse{Admission: admission, Error: failure}, failure
}
