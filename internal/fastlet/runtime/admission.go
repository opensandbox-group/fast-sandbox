package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fast-sandbox/internal/api"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/runtimecatalog"
	"fast-sandbox/pkg/util/idgen"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type reservation struct {
	requestID           string
	createSpecHash      string
	claimNamespace      string
	claimName           string
	runtimeProfileHash  string
	resourceProfileHash string
	infraProfileHash    string
	token               string
	expiresAt           time.Time
}

func generateReservationToken() (string, error) {
	return idgen.GenerateRequestID()
}

func (m *SandboxManager) ReserveSandbox(req *api.ReserveSandboxRequest) (*api.ReserveSandboxResponse, error) {
	if req == nil || req.RequestID == "" || req.CreateSpecHash == "" || req.ClaimNamespace == "" || req.ClaimName == "" {
		return reserveFailure(api.ErrorConflict, "requestId, createSpecHash, claimNamespace, and claimName are required", false)
	}
	if m.fastletPodUID != "" && req.FastletPodUID != m.fastletPodUID {
		return reserveFailure(api.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if err := m.validateReservationProfiles(req); err != nil {
		return reserveFailure(api.ErrorConflict, err.Error(), false)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupExpiredReservationsLocked()
	if m.recovering || !m.runtimeReady {
		return reserveFailureWithAdmission(api.ErrorRuntimeUnavailable, "Fastlet runtime recovery/capability probe is incomplete", true, m.admissionStatusLocked())
	}
	if !m.infraReady {
		message := m.infraMessage
		if message == "" {
			message = "required InfraProfile artifacts are still preparing"
		}
		return reserveFailureWithAdmission(api.ErrorInfraUnavailable, message, true, m.admissionStatusLocked())
	}
	if m.draining {
		return reserveFailureWithAdmission(api.ErrorDraining, m.drainReason, true, m.admissionStatusLocked())
	}
	reservationKey := reservationLookupKey(req.ClaimNamespace, req.RequestID)
	if token, ok := m.requestReservations[reservationKey]; ok {
		existing := m.reservations[token]
		if existing != nil && reservationMatches(existing, req) {
			return &api.ReserveSandboxResponse{ReservationToken: token, FastletPodUID: m.fastletPodUID, ExpiresAt: existing.expiresAt, Admission: m.admissionStatusLocked()}, nil
		}
		return reserveFailureWithAdmission(api.ErrorConflict, "requestId is already reserved with a different create spec/profile", false, m.admissionStatusLocked())
	}
	if m.usedLocked() >= m.capacity {
		return reserveFailureWithAdmission(api.ErrorCapacityRejected, "Fastlet admission capacity is exhausted", true, m.admissionStatusLocked())
	}
	if !m.runtimeResourceAvailable() {
		return reserveFailureWithAdmission(api.ErrorNetworkUnavailable, "Fastlet has no clean network slot available", true, m.admissionStatusLocked())
	}
	token, err := m.tokenGenerator()
	if err != nil {
		return reserveFailureWithAdmission(api.ErrorUnknownOutcome, fmt.Sprintf("generate reservation token: %v", err), true, m.admissionStatusLocked())
	}
	expiresAt := m.clock.Now().Add(m.reservationTTL)
	m.reservations[token] = &reservation{
		requestID: req.RequestID, createSpecHash: req.CreateSpecHash,
		claimNamespace: req.ClaimNamespace, claimName: req.ClaimName,
		runtimeProfileHash: req.RuntimeProfileHash, resourceProfileHash: req.ResourceProfileHash,
		infraProfileHash: req.InfraProfileHash,
		token:            token, expiresAt: expiresAt,
	}
	m.requestReservations[reservationKey] = token
	return &api.ReserveSandboxResponse{ReservationToken: token, FastletPodUID: m.fastletPodUID, ExpiresAt: expiresAt, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) CancelReservation(req *api.CancelReservationRequest) (*api.CancelReservationResponse, error) {
	if req == nil || req.ReservationToken == "" {
		failure := fastletError(api.ErrorConflict, "reservationToken is required", false)
		return &api.CancelReservationResponse{Error: failure}, failure
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupExpiredReservationsLocked()
	reservation := m.reservations[req.ReservationToken]
	if reservation == nil {
		return &api.CancelReservationResponse{Canceled: true}, nil
	}
	if req.RequestID != "" && reservation.requestID != req.RequestID {
		failure := fastletError(api.ErrorConflict, "reservation token belongs to another requestId", false)
		return &api.CancelReservationResponse{Error: failure}, failure
	}
	m.removeReservationLocked(reservation)
	return &api.CancelReservationResponse{Canceled: true}, nil
}

func (m *SandboxManager) EnsureSandboxV2(ctx context.Context, req *api.EnsureSandboxRequest) (*api.EnsureSandboxResponse, error) {
	if failure := m.validateEnsureRequest(req); failure != nil {
		return ensureFailure(failure, api.AdmissionStatus{})
	}
	spec := req.Sandbox
	spec.SandboxID = req.Identity.SandboxUID
	spec.RequestID = req.Identity.RequestID
	spec.InstanceGeneration = req.Identity.InstanceGeneration
	spec.AssignmentAttempt = req.Identity.AssignmentAttempt
	spec.RouteGeneration = req.Identity.RouteGeneration
	if spec.RouteGeneration <= 0 {
		spec.RouteGeneration = 1
	}
	spec.FastletPodUID = req.Identity.FastletPodUID
	if err := m.validateProfiles(&spec); err != nil {
		return ensureFailure(fastletError(api.ErrorConflict, err.Error(), false), api.AdmissionStatus{})
	}

	m.mu.Lock()
	m.cleanupExpiredReservationsLocked()
	if m.recovering || !m.runtimeReady {
		response, err := ensureFailure(fastletError(api.ErrorRuntimeUnavailable, "Fastlet runtime recovery/capability probe is incomplete", true), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !m.infraReady {
		message := m.infraMessage
		if message == "" {
			message = "required InfraProfile artifacts are still preparing"
		}
		response, err := ensureFailure(fastletError(api.ErrorInfraUnavailable, message, true), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if existing := m.sandboxes[spec.SandboxID]; existing != nil {
		if existing.Phase == "infra-pending" {
			identity := api.SandboxIdentity{
				RequestID: spec.RequestID, SandboxUID: spec.SandboxID,
				InstanceGeneration: spec.InstanceGeneration, AssignmentAttempt: spec.AssignmentAttempt,
				RouteGeneration: spec.RouteGeneration, FastletPodUID: spec.FastletPodUID,
			}
			if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
				response, err := ensureFailure(failure, m.admissionStatusLocked())
				m.mu.Unlock()
				return response, err
			}
			if !sameSandboxClaim(existing, &spec) {
				response, err := ensureFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
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
				return ensureFailure(fastletError(api.ErrorConflict, "Sandbox changed while Infra Components were initializing", true), admission)
			}
			if infraErr != nil {
				existing.Phase = "infra-pending"
				admission := m.admissionStatusLocked()
				m.mu.Unlock()
				return ensureFailure(fastletErrorWithCause(api.ErrorInProgress, infraErr.Error(), true, infraErr), admission)
			}
			existing.Phase = "route-pending"
			m.mu.Unlock()
			return m.retryRoutePublication(ctx, existing)
		}
		if existing.Phase == "route-pending" {
			identity := api.SandboxIdentity{
				RequestID: spec.RequestID, SandboxUID: spec.SandboxID,
				InstanceGeneration: spec.InstanceGeneration, AssignmentAttempt: spec.AssignmentAttempt,
				RouteGeneration: spec.RouteGeneration, FastletPodUID: spec.FastletPodUID,
			}
			if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
				response, err := ensureFailure(failure, m.admissionStatusLocked())
				m.mu.Unlock()
				return response, err
			}
			if !sameSandboxClaim(existing, &spec) {
				response, err := ensureFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
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
				return ensureFailure(fastletError(api.ErrorConflict, "Sandbox changed while its route was being published", true), admission)
			}
			if publishErr != nil {
				existing.Phase = "route-pending"
				admission := m.admissionStatusLocked()
				m.mu.Unlock()
				return ensureFailure(fastletErrorWithCause(api.ErrorInProgress, publishErr.Error(), true, publishErr), admission)
			}
			existing.Phase = "running"
			status := sandboxStatus(existing)
			admission := m.admissionStatusLocked()
			m.mu.Unlock()
			return &api.EnsureSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: admission}, nil
		}
		response, err := m.ensureExistingLocked(existing, &spec)
		m.mu.Unlock()
		return response, err
	}
	if m.draining {
		response, err := ensureFailure(fastletError(api.ErrorDraining, m.drainReason, true), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}

	if req.ReservationToken != "" {
		reserved := m.reservations[req.ReservationToken]
		if reserved == nil || reserved.requestID != req.Identity.RequestID ||
			reserved.createSpecHash != req.CreateSpecHash ||
			reserved.claimNamespace != spec.ClaimNamespace || reserved.claimName != spec.ClaimName ||
			reserved.runtimeProfileHash != spec.RuntimeProfileHash || reserved.resourceProfileHash != spec.ResourceProfileHash ||
			reserved.infraProfileHash != spec.InfraProfileHash {
			response, err := ensureFailure(fastletError(api.ErrorConflict, "reservation is missing, expired, or does not match Ensure identity/profile", false), m.admissionStatusLocked())
			m.mu.Unlock()
			return response, err
		}
		m.removeReservationLocked(reserved)
	} else if token := m.requestReservations[reservationLookupKey(spec.ClaimNamespace, req.Identity.RequestID)]; token != "" {
		reserved := m.reservations[token]
		if reserved == nil || reserved.requestID != req.Identity.RequestID ||
			reserved.createSpecHash != req.CreateSpecHash || reserved.claimNamespace != spec.ClaimNamespace || reserved.claimName != spec.ClaimName ||
			reserved.runtimeProfileHash != spec.RuntimeProfileHash || reserved.resourceProfileHash != spec.ResourceProfileHash ||
			reserved.infraProfileHash != spec.InfraProfileHash {
			response, err := ensureFailure(fastletError(api.ErrorConflict, "matching committed claim could not take over reservation", false), m.admissionStatusLocked())
			m.mu.Unlock()
			return response, err
		}
		// A Controller may win the reconcile race or take over after the
		// FastPath commits its CRD. The durable claim is allowed to convert the
		// matching reservation without possessing the ephemeral token.
		m.removeReservationLocked(reserved)
	} else if m.usedLocked() >= m.capacity {
		response, err := ensureFailure(fastletError(api.ErrorCapacityRejected, "Fastlet admission capacity is exhausted", true), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}

	placeholder := &SandboxMetadata{SandboxSpec: spec, Phase: "creating", CreatedAt: m.clock.Now().Unix()}
	m.sandboxes[spec.SandboxID] = placeholder
	admission := m.admissionStatusLocked()
	m.mu.Unlock()

	metadata, err := m.runtime.EnsureSandbox(ctx, &spec)
	if err != nil {
		m.cacheProtection.ProtectHotUntil(spec.Image, m.clock.Now().Add(time.Hour))
		m.mu.Lock()
		if current := m.sandboxes[spec.SandboxID]; current == placeholder {
			delete(m.sandboxes, spec.SandboxID)
		}
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		code := api.ErrorRuntimeUnavailable
		if errors.Is(err, ErrNetworkUnavailable) {
			code = api.ErrorNetworkUnavailable
		} else if errors.Is(err, ErrInfraUnavailable) {
			code = api.ErrorInfraUnavailable
		}
		return ensureFailure(fastletErrorWithCause(code, err.Error(), true, err), admission)
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
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox was deleted while creation was in progress", false), admission)
	}
	m.sandboxes[spec.SandboxID] = metadata
	m.mu.Unlock()
	if err := m.initializeInfraInstance(ctx, metadata); err != nil {
		m.mu.Lock()
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		return ensureFailure(fastletErrorWithCause(api.ErrorInProgress, err.Error(), true, err), admission)
	}

	m.mu.Lock()
	if m.sandboxes[spec.SandboxID] != metadata || metadata.Phase != "infra-pending" {
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox changed while Infra Components were initializing", true), admission)
	}
	metadata.Phase = "route-pending"
	m.mu.Unlock()

	if err := m.publishRoute(ctx, metadata); err != nil {
		m.mu.Lock()
		if m.sandboxes[spec.SandboxID] == metadata && metadata.Phase == "route-pending" {
			admission = m.admissionStatusLocked()
		}
		m.mu.Unlock()
		return ensureFailure(fastletErrorWithCause(api.ErrorInProgress, err.Error(), true, err), admission)
	}

	m.mu.Lock()
	if m.sandboxes[spec.SandboxID] != metadata || metadata.Phase != "route-pending" {
		admission = m.admissionStatusLocked()
		m.mu.Unlock()
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox changed while its route was being published", true), admission)
	}
	metadata.Phase = "running"
	status := sandboxStatus(metadata)
	admission = m.admissionStatusLocked()
	m.mu.Unlock()
	return &api.EnsureSandboxResponse{Accepted: true, Created: true, Sandbox: &status, Admission: admission}, nil
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

func (m *SandboxManager) retryRoutePublication(ctx context.Context, metadata *SandboxMetadata) (*api.EnsureSandboxResponse, error) {
	m.mu.Lock()
	if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "route-pending" {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox changed before its route could be published", true), admission)
	}
	metadata.Phase = "publishing-route"
	m.mu.Unlock()
	publishErr := m.publishRoute(ctx, metadata)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sandboxes[metadata.SandboxID] != metadata || metadata.Phase != "publishing-route" {
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox changed while its route was being published", true), m.admissionStatusLocked())
	}
	if publishErr != nil {
		metadata.Phase = "route-pending"
		return ensureFailure(fastletErrorWithCause(api.ErrorInProgress, publishErr.Error(), true, publishErr), m.admissionStatusLocked())
	}
	metadata.Phase = "running"
	status := sandboxStatus(metadata)
	return &api.EnsureSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) DeleteSandboxV2(req *api.DeleteSandboxV2Request) (*api.DeleteSandboxV2Response, error) {
	if failure := m.validateIdentityTarget(deleteIdentity(req)); failure != nil {
		return &api.DeleteSandboxV2Response{Error: failure}, failure
	}
	m.mu.RLock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
			m.mu.RUnlock()
			return &api.DeleteSandboxV2Response{Error: failure}, failure
		}
	}
	m.mu.RUnlock()
	if _, err := m.DeleteSandbox(req.Identity.SandboxUID); err != nil {
		failure := fastletError(api.ErrorRuntimeUnavailable, err.Error(), true)
		return &api.DeleteSandboxV2Response{Error: failure}, failure
	}
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
	m.cleanupExpiredReservationsLocked()
	return m.admissionStatusLocked(), m.recovering, m.draining
}

func (m *SandboxManager) ensureExistingLocked(existing *SandboxMetadata, requested *api.SandboxSpec) (*api.EnsureSandboxResponse, error) {
	identity := api.SandboxIdentity{
		RequestID: requested.RequestID, SandboxUID: requested.SandboxID,
		InstanceGeneration: requested.InstanceGeneration, AssignmentAttempt: requested.AssignmentAttempt,
		RouteGeneration: requested.RouteGeneration, FastletPodUID: requested.FastletPodUID,
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
		return ensureFailure(failure, m.admissionStatusLocked())
	}
	if !sameSandboxClaim(existing, requested) {
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
	}
	status := sandboxStatus(existing)
	if existing.Phase == "creating" || existing.Phase == "initializing-infra" || existing.Phase == "publishing-route" {
		failure := fastletError(api.ErrorInProgress, "Sandbox creation is already in progress", true)
		return &api.EnsureSandboxResponse{Accepted: true, InProgress: true, Sandbox: &status, Admission: m.admissionStatusLocked(), Error: failure}, failure
	}
	if existing.Phase == "terminating" || existing.Phase == "deleting" {
		return ensureFailure(fastletError(api.ErrorConflict, "Sandbox deletion is already in progress", true), m.admissionStatusLocked())
	}
	if existing.Phase != "running" {
		return ensureFailure(fastletError(api.ErrorRuntimeUnavailable, fmt.Sprintf("managed Sandbox runtime is %s, not running", existing.Phase), true), m.admissionStatusLocked())
	}
	return &api.EnsureSandboxResponse{Accepted: true, Created: false, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) validateIdentityTarget(identity *api.SandboxIdentity) *api.FastletError {
	if identity == nil || identity.SandboxUID == "" || identity.InstanceGeneration <= 0 || identity.AssignmentAttempt <= 0 {
		return fastletError(api.ErrorConflict, "sandboxUid, positive instanceGeneration, and positive assignmentAttempt are required", false)
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

func (m *SandboxManager) validateEnsureRequest(req *api.EnsureSandboxRequest) *api.FastletError {
	if req == nil || req.Identity.SandboxUID == "" || req.Identity.InstanceGeneration <= 0 || req.Identity.AssignmentAttempt <= 0 {
		return fastletError(api.ErrorConflict, "sandboxUid, positive instanceGeneration, and positive assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && req.Identity.FastletPodUID != m.fastletPodUID {
		return fastletError(api.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if req.Sandbox.SandboxID != "" && req.Sandbox.SandboxID != req.Identity.SandboxUID {
		return fastletError(api.ErrorConflict, "sandboxId must be empty or equal sandboxUid", false)
	}
	return nil
}

func (m *SandboxManager) validateReservationProfiles(req *api.ReserveSandboxRequest) error {
	if m.runtimeProfileHash != "" && req.RuntimeProfileHash != m.runtimeProfileHash {
		return fmt.Errorf("runtime profile hash does not match Fastlet")
	}
	if m.resourceProfileHash != "" && req.ResourceProfileHash != m.resourceProfileHash {
		return fmt.Errorf("resource profile hash does not match Fastlet")
	}
	if m.infraProfileHash != "" && req.InfraProfileHash != m.infraProfileHash {
		return fmt.Errorf("InfraProfile hash does not match Fastlet")
	}
	return nil
}

func (m *SandboxManager) cleanupExpiredReservationsLocked() {
	now := m.clock.Now()
	for _, item := range m.reservations {
		if !now.Before(item.expiresAt) {
			m.removeReservationLocked(item)
		}
	}
}

func (m *SandboxManager) removeReservationLocked(item *reservation) {
	delete(m.reservations, item.token)
	key := reservationLookupKey(item.claimNamespace, item.requestID)
	if m.requestReservations[key] == item.token {
		delete(m.requestReservations, key)
	}
}

func (m *SandboxManager) usedLocked() int {
	return len(m.reservations) + len(m.sandboxes)
}

func (m *SandboxManager) admissionStatusLocked() api.AdmissionStatus {
	status := api.AdmissionStatus{Capacity: m.capacity, Reservations: len(m.reservations)}
	for _, metadata := range m.sandboxes {
		switch metadata.Phase {
		case "creating", "infra-pending", "initializing-infra", "route-pending", "publishing-route":
			status.Creating++
		case "terminating", "deleting", "delete-failed":
			status.Deleting++
		default:
			status.Running++
		}
	}
	status.Used = status.Reservations + status.Creating + status.Running + status.Deleting
	return status
}

func sandboxStatus(metadata *SandboxMetadata) api.SandboxStatus {
	return api.SandboxStatus{
		SandboxID: metadata.SandboxID, ClaimUID: metadata.ClaimUID,
		InstanceGeneration: metadata.InstanceGeneration, AssignmentAttempt: metadata.AssignmentAttempt,
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

func reservationMatches(existing *reservation, req *api.ReserveSandboxRequest) bool {
	return existing.requestID == req.RequestID && existing.createSpecHash == req.CreateSpecHash &&
		existing.claimNamespace == req.ClaimNamespace && existing.claimName == req.ClaimName &&
		existing.runtimeProfileHash == req.RuntimeProfileHash && existing.resourceProfileHash == req.ResourceProfileHash &&
		existing.infraProfileHash == req.InfraProfileHash
}

func reservationLookupKey(namespace, requestID string) string {
	return namespace + "\x00" + requestID
}

func fastletError(code api.FastletErrorCode, message string, retryable bool) *api.FastletError {
	return &api.FastletError{Code: code, Message: message, Retryable: retryable}
}

func fastletErrorWithCause(code api.FastletErrorCode, message string, retryable bool, cause error) *api.FastletError {
	return &api.FastletError{Code: code, Message: message, Retryable: retryable, Cause: cause}
}

func reserveFailure(code api.FastletErrorCode, message string, retryable bool) (*api.ReserveSandboxResponse, error) {
	return reserveFailureWithAdmission(code, message, retryable, api.AdmissionStatus{})
}

func reserveFailureWithAdmission(code api.FastletErrorCode, message string, retryable bool, admission api.AdmissionStatus) (*api.ReserveSandboxResponse, error) {
	failure := fastletError(code, message, retryable)
	return &api.ReserveSandboxResponse{Admission: admission, Error: failure}, failure
}

func ensureFailure(failure *api.FastletError, admission api.AdmissionStatus) (*api.EnsureSandboxResponse, error) {
	return &api.EnsureSandboxResponse{Admission: admission, Error: failure}, failure
}
