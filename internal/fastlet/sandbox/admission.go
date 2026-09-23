package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/observability"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type createReservation struct {
	placeholder     *SandboxMetadata
	cleanupExisting *SandboxMetadata
	existing        *fastletapi.CreateSandboxResponse
	admission       fastletapi.AdmissionStatus
	disposition     fastletapi.CreateDisposition
}

func (m *SandboxManager) CreateSandbox(ctx context.Context, req *fastletapi.CreateSandboxRequest) (_ *fastletapi.CreateSandboxResponse, resultErr error) {
	ctx = withCreateIdentity(ctx, req)
	ctx, span := observability.Start(ctx, "fastlet.create Sandbox")
	started := time.Now()
	defer func() {
		observability.End(span, resultErr)
		recordAdmission("create", resultErr)
	}()
	_, finishValidation := startFastletCreateStage(ctx, m.runtimeName, "validation")
	input, validationFailure := m.prepareCreateInput(req)
	finishValidation(validationFailure)
	if validationFailure != nil {
		return createFailure(validationFailure, fastletapi.AdmissionStatus{})
	}

	_, finishAdmission := startFastletCreateStage(ctx, m.runtimeName, "admission")
	reservation, admissionErr := m.reserveSandboxForCreate(req, input)
	finishAdmission(admissionErr)
	if admissionErr != nil {
		if reservation.existing != nil {
			return reservation.existing, admissionErr
		}
		failure := &fastletapi.FastletError{}
		ok := errors.As(admissionErr, &failure)
		if !ok {
			failure = fastletErrorWithCause(fastletapi.ErrorUnknownOutcome, admissionErr.Error(), true, admissionErr)
		}
		return createFailureWithDisposition(failure, reservation.admission, reservation.disposition)
	}
	if reservation.cleanupExisting != nil {
		return m.retryFailedCreateCleanup(ctx, req, &input, reservation.cleanupExisting)
	}
	if reservation.existing != nil {
		return m.finishCreate(ctx, req, reservation.existing)
	}
	placeholder := reservation.placeholder
	sandboxUID := input.Sandbox.Identity.SandboxUID
	m.recordDiagnostic(sandboxUID, "info", "admission", sandboxStateCreating, "Fastlet admission accepted; atomic runtime creation started")
	if bindingFailure, currentAdmission := m.registerDesiredBindings(placeholder, req); bindingFailure != nil {
		return createFailureWithDisposition(bindingFailure, currentAdmission, fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	// Async artifact delivery: when the runtime supports it (Firecracker),
	// a missing image — or, for a resume, a missing checkpoint set — is
	// delivered in the background instead of blocking this create on the
	// network. The Sandbox parks in image-pending and a boot worker boots it
	// once the artifacts are committed.
	if deliver, ok := m.deliveryForCreate(&input); ok {
		deliverStatus, deliverErr := deliver(ctx)
		if deliverErr != nil {
			return m.handleRuntimeCreateFailure(ctx, &input, placeholder, deliverErr)
		}
		if deliverStatus == ImageDelivering {
			return m.parkForImageDelivery(req, &input, placeholder, started)
		}
	}
	metadata, err := m.ensureRuntimeForCreate(ctx, started, &input)
	if err != nil {
		return m.handleRuntimeCreateFailure(ctx, &input, placeholder, err)
	}
	status, admission, dataPlaneReady, commitFailure := m.commitRuntimeCreate(req, input, placeholder, metadata)
	if commitFailure != nil {
		return createFailure(commitFailure, admission)
	}
	m.recordRuntimeReadyAndDispatchHooks(metadata, req, dataPlaneReady)
	m.continueDataPlaneCreation(metadata, started, dataPlaneReady)
	return m.finishCreate(ctx, req, &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionCreated, Sandbox: &status, Admission: admission})
}

func withCreateIdentity(ctx context.Context, req *fastletapi.CreateSandboxRequest) context.Context {
	if req == nil {
		return ctx
	}
	return observability.WithIdentity(ctx, observability.Identity{
		RequestID: req.RequestID, Namespace: req.Identity.Namespace, SandboxName: req.Identity.Name,
		SandboxUID: req.Identity.SandboxUID, FastletPodUID: req.Identity.FastletPodUID,
		InstanceGeneration: req.Identity.InstanceGeneration, AssignmentAttempt: req.Identity.AssignmentAttempt,
		RouteGeneration: req.Identity.RouteGeneration,
	})
}

func (m *SandboxManager) prepareCreateInput(req *fastletapi.CreateSandboxRequest) (fastletapi.EnsureSandboxInput, *fastletapi.FastletError) {
	if failure := m.validateCreateRequest(req); failure != nil {
		return fastletapi.EnsureSandboxInput{}, failure
	}
	identity := req.Identity
	if identity.RouteGeneration <= 0 {
		identity.RouteGeneration = 1
	}
	input := fastletapi.EnsureSandboxInput{
		RequestID: req.RequestID,
		Sandbox:   fastletapi.RuntimeSandboxConfig{Identity: identity, Spec: req.Sandbox},
	}
	if err := m.validateProfiles(&input.Sandbox.Spec); err != nil {
		return fastletapi.EnsureSandboxInput{}, fastletError(fastletapi.ErrorProfileMismatch, err.Error(), false)
	}
	return input, nil
}

func (m *SandboxManager) reserveSandboxForCreate(req *fastletapi.CreateSandboxRequest, input fastletapi.EnsureSandboxInput) (createReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := createReservation{admission: m.admissionStatusLocked()}
	reject := func(failure *fastletapi.FastletError, disposition fastletapi.CreateDisposition) (createReservation, error) {
		result.admission = m.admissionStatusLocked()
		result.disposition = disposition
		return result, failure
	}
	if m.recovering || !m.runtimeReady {
		return reject(fastletError(fastletapi.ErrorRuntimeUnavailable, "Fastlet runtime recovery/capability probe is incomplete", true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if !m.infraReady {
		message := m.infraMessage
		if message == "" {
			message = "required Infra Component artifacts are still preparing"
		}
		return reject(fastletError(fastletapi.ErrorInfraUnavailable, message, true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if len(req.ActionBindings) > 0 && m.actionManager == nil {
		return reject(fastletError(fastletapi.ErrorActionUnavailable, "Fastlet has no Action Handler configuration", false), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	identity := input.Sandbox.Identity
	if existing := m.sandboxes[identity.SandboxUID]; existing != nil {
		if existing.Phase == sandboxStateCreateCleanupFailed {
			result.cleanupExisting = existing
			return result, nil
		}
		response, err := m.createExistingLocked(existing, &input.Sandbox)
		result.existing = response
		return result, err
	}
	if tombstone, found := m.tombstones[identity.SandboxUID]; found && identityAtOrBefore(identity.InstanceGeneration, identity.AssignmentAttempt, tombstone) {
		return reject(fastletError(fastletapi.ErrorGenerationFenced, "Sandbox generation was already deleted", false), fastletapi.CreateDispositionGenerationFenced)
	}
	if m.draining {
		return reject(fastletError(fastletapi.ErrorDraining, m.drainReason, true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if len(m.sandboxes) >= m.capacity {
		// A terminal create-failed Sandbox is projected but occupies no
		// capacity: it must not block new creates.
		active := 0
		for _, managed := range m.sandboxes {
			if managed.Phase != sandboxStateCreateFailed {
				active++
			}
		}
		if active >= m.capacity {
			return reject(fastletError(fastletapi.ErrorCapacityRejected, "Fastlet admission capacity is exhausted", true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
		}
	}
	if !m.runtimeResourceAvailable() {
		return reject(fastletError(fastletapi.ErrorNetworkUnavailable, "Fastlet has no clean runtime/network resource available", true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	result.placeholder = &SandboxMetadata{
		Config: input.Sandbox, Phase: sandboxStateCreating, CreatedAt: m.clock.Now().Unix(), AcceptedGeneration: req.SpecGeneration,
		ActionBindingStatuses: pendingActionBindingStatuses(req.ActionBindings),
	}
	m.sandboxes[identity.SandboxUID] = result.placeholder
	result.admission = m.admissionStatusLocked()
	return result, nil
}

func (m *SandboxManager) registerDesiredBindings(placeholder *SandboxMetadata, req *fastletapi.CreateSandboxRequest) (*fastletapi.FastletError, fastletapi.AdmissionStatus) {
	if err := m.registerDesiredActions(placeholder, req.SpecGeneration, req.ActionBindings); err != nil {
		m.mu.Lock()
		sandboxUID := placeholder.Config.Identity.SandboxUID
		if m.sandboxes[sandboxUID] == placeholder {
			delete(m.sandboxes, sandboxUID)
		}
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return fastletErrorWithCause(fastletapi.ErrorActionUnavailable, err.Error(), false, err), admission
	}
	return nil, fastletapi.AdmissionStatus{}
}

func (m *SandboxManager) ensureRuntimeForCreate(ctx context.Context, createStarted time.Time, input *fastletapi.EnsureSandboxInput) (*SandboxMetadata, error) {
	runtimeStarted := time.Now()
	runtimeContext, finishRuntime := startFastletCreateStage(ctx, m.runtimeName, "runtime_ensure")
	metadata, err := m.runtime.EnsureSandbox(runtimeContext, input)
	finishRuntime(err)
	observeRuntimeCreate(m.runtimeName, runtimeStarted, err)
	observeUserProcessStart(m.runtimeName, m.infraRevision, createStarted, metadata)
	return metadata, err
}

func (m *SandboxManager) handleRuntimeCreateFailure(ctx context.Context, input *fastletapi.EnsureSandboxInput, placeholder *SandboxMetadata, runtimeErr error) (*fastletapi.CreateSandboxResponse, error) {
	sandboxUID := input.Sandbox.Identity.SandboxUID
	m.cacheProtection.ProtectHotUntil(input.Sandbox.Spec.Image, m.clock.Now().Add(time.Hour))
	cleanupErr := m.runtime.DeleteSandbox(ctx, sandboxUID)
	if m.actionManager != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr = errors.Join(cleanupErr, m.actionManager.Delete(cleanupCtx, sandboxUID))
		cancel()
	}
	m.mu.Lock()
	disposition := fastletapi.CreateDispositionRejectedBeforeSideEffects
	if cleanupErr == nil && m.sandboxes[sandboxUID] == placeholder {
		delete(m.sandboxes, sandboxUID)
		delete(m.runtimeMessages, sandboxUID)
	} else if m.sandboxes[sandboxUID] == placeholder {
		placeholder.Phase = sandboxStateCreateCleanupFailed
		disposition = fastletapi.CreateDispositionFailedNeedsCleanup
	}
	admission := m.admissionStatusLocked()
	m.mu.Unlock()
	// Deterministic node-vs-artifact mismatch, not a runtime fault: the
	// orchestrator moves to the next candidate and never re-queues this
	// node for the same image.
	code := fastletapi.ErrorRuntimeUnavailable
	retryable := true
	if errors.Is(runtimeErr, ErrNetworkUnavailable) {
		code = fastletapi.ErrorNetworkUnavailable
	} else if errors.Is(runtimeErr, ErrInfraUnavailable) {
		code = fastletapi.ErrorInfraUnavailable
	} else if errors.Is(runtimeErr, ErrIncompatibleArtifact) {
		code = fastletapi.ErrorProfileMismatch
		retryable = false
	}
	message := runtimeErr.Error()
	if cleanupErr != nil {
		message = fmt.Sprintf("%s; cleanup failed: %v", message, cleanupErr)
	}
	m.recordDiagnostic(sandboxUID, "error", "runtime", string(disposition), message)
	return createFailureWithDisposition(fastletErrorWithCause(code, message, retryable, errors.Join(runtimeErr, cleanupErr)), admission, disposition)
}

func (m *SandboxManager) commitRuntimeCreate(req *fastletapi.CreateSandboxRequest, input fastletapi.EnsureSandboxInput, placeholder, metadata *SandboxMetadata) (fastletapi.SandboxStatus, fastletapi.AdmissionStatus, bool, *fastletapi.FastletError) {
	metadata.Phase, metadata.Config = sandboxStateInfraPending, input.Sandbox
	metadata.ActionBindingStatuses = pendingActionBindingStatuses(req.ActionBindings)
	metadata.AcceptedGeneration = req.SpecGeneration
	if len(req.ActionBindings) == 0 {
		metadata.AppliedGeneration = req.SpecGeneration
	}
	m.cacheProtection.Protect(input.Sandbox.Spec.Image, fastletcache.ProtectActive)
	m.mu.Lock()
	defer m.mu.Unlock()
	sandboxUID := input.Sandbox.Identity.SandboxUID
	if placeholder.Phase == sandboxStateTerminating {
		metadata.Phase = sandboxStateTerminating
		m.sandboxes[sandboxUID] = metadata
		admission := m.admissionStatusLocked()
		go m.asyncDelete(sandboxUID, metadata)
		return fastletapi.SandboxStatus{}, admission, false, fastletError(fastletapi.ErrorConflict, "Sandbox was deleted while creation was in progress", false)
	}
	m.sandboxes[sandboxUID] = metadata
	if m.infraManager == nil && m.routePublisher == nil {
		if len(req.ActionBindings) > 0 {
			metadata.Phase = sandboxStateActionPending
		} else {
			metadata.Phase = sandboxStateRunning
		}
		m.recordDiagnosticLocked(sandboxUID, "info", "fastlet", sandboxStateRunning, "runtime is ready; no asynchronous data-plane initialization is required")
	}
	status := m.sandboxStatusLocked(metadata)
	return status, m.admissionStatusLocked(), metadata.Phase == sandboxStateRunning, nil
}

func (m *SandboxManager) recordRuntimeReadyAndDispatchHooks(metadata *SandboxMetadata, req *fastletapi.CreateSandboxRequest, dataPlaneReady bool) {
	if err := m.registerDesiredActions(metadata, req.SpecGeneration, req.ActionBindings); err != nil {
		m.recordDiagnostic(metadata.Config.Identity.SandboxUID, "error", "action", "binding-pending", err.Error())
	}
	m.recordActionHook(metadata, actionapi.LifecycleHookRuntimeReady, 1)
	if dataPlaneReady {
		m.recordActionHook(metadata, actionapi.LifecycleHookDataPlaneReady, 2)
	}
}

func (m *SandboxManager) continueDataPlaneCreation(metadata *SandboxMetadata, started time.Time, dataPlaneReady bool) {
	m.mu.RLock()
	phase := metadata.Phase
	sandboxUID := metadata.Config.Identity.SandboxUID
	m.mu.RUnlock()
	if dataPlaneReady {
		observeDataPlaneReady(m.runtimeName, m.infraRevision, started, nil)
	} else if dataPlaneWorkPending(phase) {
		m.recordDiagnostic(sandboxUID, "info", "runtime", sandboxStateInfraPending, "runtime and private network are ready; Infra Component initialization continues asynchronously")
		m.startDataPlaneReconcile(metadata, started)
	} else {
		m.recordDiagnostic(sandboxUID, "info", "action", sandboxStateActionPending, "runtime and private network are ready; Sandbox Actions are pending")
	}
}

func pendingActionBindingStatuses(bindings []fastletapi.ActionBindingInput) []fastletapi.ActionBindingStatus {
	statuses := make([]fastletapi.ActionBindingStatus, 0, len(bindings))
	for _, binding := range bindings {
		statuses = append(statuses, fastletapi.ActionBindingStatus{Handler: binding.Handler, State: "Pending"})
	}
	return statuses
}

// finishCreate implements the caller-selected completion boundary entirely on
// Fastlet-local state. The public FastPath normalizes its unspecified enum to
// READY. The internal zero value keeps Controller recovery calls non-blocking.
func (m *SandboxManager) finishCreate(ctx context.Context, req *fastletapi.CreateSandboxRequest, response *fastletapi.CreateSandboxResponse) (*fastletapi.CreateSandboxResponse, error) {
	if len(req.ActionBindings) == 0 {
		m.mu.Lock()
		if metadata := m.sandboxes[req.Identity.SandboxUID]; metadata != nil {
			metadata.AppliedGeneration = req.SpecGeneration
			status := m.sandboxStatusLocked(metadata)
			response.Sandbox = &status
		}
		m.mu.Unlock()
	}
	if req.Completion != fastletapi.CreateCompletionReady {
		return response, nil
	}
	ready, err := m.waitUntilSandboxReady(ctx, req.Identity, req.SpecGeneration)
	if ready != nil {
		response.Sandbox = ready
	}
	if err != nil {
		failure := &fastletapi.FastletError{}
		if errors.As(err, &failure) {
			response.Error = failure
		}
		return response, err
	}
	return response, nil
}

// retryFailedCreateCleanup resumes only cleanup that belongs to a failed
// Create attempt. A user-requested delete uses the distinct delete-failed
// phase and can never be resurrected by a delayed Create retry.
func (m *SandboxManager) retryFailedCreateCleanup(ctx context.Context, req *fastletapi.CreateSandboxRequest, input *fastletapi.EnsureSandboxInput, existing *SandboxMetadata) (*fastletapi.CreateSandboxResponse, error) {
	requested := &input.Sandbox
	sandboxUID := requested.Identity.SandboxUID
	m.mu.Lock()
	if m.sandboxes[sandboxUID] != existing {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox changed before failed Create cleanup retry", true), admission)
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, requested.Identity); failure != nil {
		response, err := createFailure(failure, m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !sameSandboxClaim(existing, requested) {
		response, err := createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	existing.Phase = sandboxStateCreateCleanup
	m.mu.Unlock()

	cleanupErr := m.runtime.DeleteSandbox(ctx, sandboxUID)
	m.mu.Lock()
	if m.sandboxes[sandboxUID] != existing {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox changed while failed Create cleanup was retried", true), admission)
	}
	if cleanupErr != nil {
		existing.Phase = sandboxStateCreateCleanupFailed
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		message := fmt.Sprintf("retry failed Create cleanup: %v", cleanupErr)
		m.recordDiagnostic(sandboxUID, "error", "runtime", string(fastletapi.CreateDispositionFailedNeedsCleanup), message)
		return createFailureWithDisposition(fastletErrorWithCause(fastletapi.ErrorRuntimeUnavailable, message, true, cleanupErr), admission, fastletapi.CreateDispositionFailedNeedsCleanup)
	}
	delete(m.sandboxes, sandboxUID)
	delete(m.runtimeMessages, sandboxUID)
	m.mu.Unlock()
	m.recordDiagnostic(sandboxUID, "info", "runtime", "cleanup-recovered", "failed Create cleanup converged; retrying the same runtime identity")
	return m.CreateSandbox(ctx, req)
}

func (m *SandboxManager) InspectSandbox(req *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
	if failure := m.validateIdentityTarget(reqIdentity(req)); failure != nil {
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	m.mu.RLock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata == nil {
		m.mu.RUnlock()
		failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
		m.mu.RUnlock()
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	status := m.sandboxStatusLocked(metadata)
	actionManager := m.actionManager
	m.mu.RUnlock()

	// The Action Manager owns the freshest Handler observation. In particular,
	// its local probe can invalidate a Binding immediately when a Handler is
	// unavailable or its instance ID changes, before the Controller has had time
	// to project that transition back into SandboxMetadata and CRD status.
	if actionManager != nil {
		if bindings, generation := actionManager.Statuses(req.Identity.SandboxUID); bindings != nil {
			status.ActionBindings = bindings
			status.AppliedGeneration = generation
		}
	}
	return &fastletapi.InspectSandboxResponse{Sandbox: &status}, nil
}

func (m *SandboxManager) DeleteSandbox(req *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	return m.DeleteSandboxContext(context.Background(), req)
}

// DeleteSandboxContext keeps the bounded, best-effort Action Handler cleanup
// attempt ahead of runtime and network teardown. Handler failure is diagnostic
// only and cannot block deletion or finalizer completion.
func (m *SandboxManager) DeleteSandboxContext(ctx context.Context, req *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	if failure := m.validateIdentityTarget(deleteIdentity(req)); failure != nil {
		return &fastletapi.DeleteSandboxResponse{Error: failure}, failure
	}
	m.mu.Lock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	wasCreating := false
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
			m.mu.Unlock()
			return &fastletapi.DeleteSandboxResponse{Error: failure}, failure
		}
		if metadata.Phase == sandboxStateTerminating || metadata.Phase == sandboxStateDeleting {
			m.mu.Unlock()
			return &fastletapi.DeleteSandboxResponse{}, nil
		}
		wasCreating = metadata.Phase == sandboxStateCreating || metadata.Phase == sandboxStateImagePending
		m.cancelImageBootWorkerLocked(metadata)
		m.cancelDataPlaneReconcileLocked(metadata)
		metadata.Phase = sandboxStateTerminating
	}
	m.recordTombstoneLocked(req.Identity)
	m.recordDiagnosticLocked(req.Identity.SandboxUID, "info", "admission", sandboxStateTerminating, "declarative deletion accepted; new Binding and Hook work is fenced")
	m.signalReadinessChangedLocked()
	m.mu.Unlock()
	if metadata != nil && m.actionManager != nil {
		deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := m.actionManager.Delete(deleteCtx, req.Identity.SandboxUID)
		cancel()
		if err != nil {
			m.recordDiagnostic(req.Identity.SandboxUID, "error", "action", "delete-best-effort-failed", fmt.Sprintf("best-effort delete of Sandbox Action Bindings failed: %v", err))
		}
	}
	if metadata != nil && !wasCreating {
		go m.asyncDelete(req.Identity.SandboxUID, metadata)
	}
	return &fastletapi.DeleteSandboxResponse{}, nil
}

func (m *SandboxManager) Recover(ctx context.Context) error { //nolint:gocognit // pre-existing recovery sequence; refactor tracked separately
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
		if metadata == nil || metadata.Config.Identity.SandboxUID == "" {
			continue
		}
		identity := &metadata.Config.Identity
		if m.fastletPodUID != "" && identity.FastletPodUID != "" && identity.FastletPodUID != m.fastletPodUID {
			continue
		}
		if identity.InstanceGeneration <= 0 {
			identity.InstanceGeneration = 1
		}
		if identity.RuntimeInstanceID == "" {
			identity.RuntimeInstanceID = "legacy-" + identity.SandboxUID
		}
		if identity.AssignmentAttempt <= 0 {
			identity.AssignmentAttempt = 1
		}
		if identity.RouteGeneration <= 0 {
			identity.RouteGeneration = 1
		}
		if metadata.Phase == "" {
			metadata.Phase = sandboxStateUnknown
		}
		if m.infraManager != nil {
			if metadata.Config.Spec.InfraRevision != m.infraRevision {
				return fmt.Errorf("recovered Sandbox %s Infra revision does not match Fastlet", identity.SandboxUID)
			}
			metadata.Phase = sandboxStateInfraPending
		} else if m.actionManager != nil && m.actionManager.Required() {
			metadata.Phase = sandboxStateActionPending
		}
		recovered[identity.SandboxUID] = metadata
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
			return fmt.Errorf("recover route for Sandbox %s: %w", metadata.Config.Identity.SandboxUID, err)
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
		activeImages = append(activeImages, metadata.Config.Spec.Image)
	}
	m.cacheProtection.Replace(fastletcache.ProtectActive, activeImages)
	m.mu.Lock()
	m.sandboxes = recovered
	m.recovering = false
	m.runtimeReady = true
	m.routeReady = m.routePublisher == nil || !pendingInfra
	m.mu.Unlock()
	for _, metadata := range recovered {
		m.recordActionHook(metadata, actionapi.LifecycleHookRuntimeReady, 1)
		if m.infraManager == nil {
			m.recordActionHook(metadata, actionapi.LifecycleHookDataPlaneReady, 2)
		}
	}
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
	return !m.recovering && m.runtimeReady && m.routeReady && !m.draining && (m.actionManager == nil || m.actionManager.Ready())
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

func (m *SandboxManager) createExistingLocked(existing *SandboxMetadata, requested *fastletapi.RuntimeSandboxConfig) (*fastletapi.CreateSandboxResponse, error) {
	if failure := validateIdentityFence(m.fastletPodUID, existing, requested.Identity); failure != nil {
		return createFailure(failure, m.admissionStatusLocked())
	}
	if !sameSandboxClaim(existing, requested) {
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
	}
	status := m.sandboxStatusLocked(existing)
	if existing.Phase == sandboxStateCreating || existing.Phase == sandboxStateImagePending {
		failure := fastletError(fastletapi.ErrorInProgress, "Sandbox creation is already in progress", true)
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionInProgress, Sandbox: &status, Admission: m.admissionStatusLocked(), Error: failure}, failure
	}
	if existing.Phase == sandboxStateTerminating || existing.Phase == sandboxStateDeleting {
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox deletion is already in progress", true), m.admissionStatusLocked())
	}
	switch existing.Phase {
	case sandboxStateInfraPending, sandboxStateInitializingInfra, sandboxStateInfraUnavailable, sandboxStateRoutePending, sandboxStatePublishingRoute, sandboxStateRouteUnavailable, sandboxStateActionPending, sandboxStateActionUnavailable:
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionExisting, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
	}
	if existing.Phase != sandboxStateRunning {
		return createFailure(fastletError(fastletapi.ErrorRuntimeUnavailable, fmt.Sprintf("managed Sandbox runtime is %s, not running", existing.Phase), true), m.admissionStatusLocked())
	}
	return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionExisting, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
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

func deleteIdentity(req *fastletapi.DeleteSandboxRequest) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func validateIdentityFence(expectedPodUID string, existing *SandboxMetadata, requested fastletapi.SandboxIdentity) *fastletapi.FastletError {
	if expectedPodUID != "" && requested.FastletPodUID != expectedPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	existingIdentity := existing.Config.Identity
	if requested.InstanceGeneration < existingIdentity.InstanceGeneration ||
		(requested.InstanceGeneration == existingIdentity.InstanceGeneration && requested.AssignmentAttempt < existingIdentity.AssignmentAttempt) {
		return fastletError(fastletapi.ErrorStaleGeneration, "request generation/assignment attempt is older than the managed Sandbox", false)
	}
	if requested.InstanceGeneration > existingIdentity.InstanceGeneration || requested.AssignmentAttempt > existingIdentity.AssignmentAttempt {
		return fastletError(fastletapi.ErrorConflict, "newer generation/assignment requires the old runtime to be deleted first", true)
	}
	if requested.RuntimeInstanceID != existingIdentity.RuntimeInstanceID {
		return fastletError(fastletapi.ErrorConflict, "runtimeInstanceId conflicts with the managed Sandbox", false)
	}
	requestedRouteGeneration := requested.RouteGeneration
	if requestedRouteGeneration <= 0 {
		requestedRouteGeneration = existingIdentity.RouteGeneration
	}
	if requestedRouteGeneration < existingIdentity.RouteGeneration {
		return fastletError(fastletapi.ErrorStaleGeneration, "request route generation is older than the managed Sandbox", false)
	}
	if requestedRouteGeneration > existingIdentity.RouteGeneration {
		return fastletError(fastletapi.ErrorConflict, "newer route generation requires the old runtime to be deleted first", true)
	}
	return nil
}

func sameSandboxClaim(existing *SandboxMetadata, requested *fastletapi.RuntimeSandboxConfig) bool {
	return existing.Config.Identity.Namespace == requested.Identity.Namespace && existing.Config.Identity.Name == requested.Identity.Name &&
		existing.Config.Spec.RuntimeProfileHash == requested.Spec.RuntimeProfileHash && existing.Config.Spec.ResourceProfileHash == requested.Spec.ResourceProfileHash &&
		existing.Config.Spec.InfraRevision == requested.Spec.InfraRevision
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
	if req == nil || req.SpecGeneration <= 0 || req.Identity.SandboxUID == "" || req.Identity.Namespace == "" || req.Identity.Name == "" || req.Identity.InstanceGeneration <= 0 || req.Identity.RuntimeInstanceID == "" || req.Identity.AssignmentAttempt <= 0 {
		return fastletError(fastletapi.ErrorConflict, "sandboxUid, namespace, name, runtimeInstanceId, positive specGeneration, instanceGeneration, and assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && req.Identity.FastletPodUID != m.fastletPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	return nil
}

func (m *SandboxManager) admissionStatusLocked() fastletapi.AdmissionStatus {
	status := fastletapi.AdmissionStatus{Capacity: m.capacity}
	for _, metadata := range m.sandboxes {
		switch metadata.Phase {
		case sandboxStateCreating, sandboxStateImagePending, sandboxStateInfraPending, sandboxStateInitializingInfra, sandboxStateInfraUnavailable, sandboxStateRoutePending, sandboxStatePublishingRoute, sandboxStateRouteUnavailable, sandboxStateActionPending, sandboxStateActionUnavailable:
			status.Creating++
		case sandboxStateTerminating, sandboxStateDeleting, sandboxStateDeleteFailed, sandboxStateCreateCleanup, sandboxStateCreateCleanupFailed:
			status.Deleting++
		case sandboxStateCreateFailed:
			// Terminal create failure: projected for the Controller but it
			// occupies no admission capacity.
		default:
			status.Running++
		}
	}
	status.Used = status.Creating + status.Running + status.Deleting
	recordAdmissionStatus(status)
	return status
}

// sandboxStatusLocked returns the point-in-time Fastlet observation. Callers
// hold m.mu so the global proxy connection state and Sandbox phase form one
// coherent data-plane result.
func (m *SandboxManager) sandboxStatusLocked(metadata *SandboxMetadata) fastletapi.SandboxStatus {
	dataPlaneReady := (m.routePublisher == nil || m.routeReady) && routeReadyForPhase(metadata.Phase)
	runtime, dataPlane := observationsForPhase(metadata.Phase, dataPlaneReady)
	if message := m.runtimeMessages[metadata.Config.Identity.SandboxUID]; message != "" && metadata.Phase != sandboxStateRunning {
		runtime.Message = message
	}
	return fastletapi.SandboxStatus{
		SandboxID:          metadata.Config.Identity.SandboxUID,
		InstanceGeneration: metadata.Config.Identity.InstanceGeneration, RuntimeInstanceID: metadata.Config.Identity.RuntimeInstanceID, AssignmentAttempt: metadata.Config.Identity.AssignmentAttempt,
		RouteGeneration: metadata.Config.Identity.RouteGeneration, AcceptedGeneration: metadata.AcceptedGeneration, AppliedGeneration: metadata.AppliedGeneration,
		Runtime: runtime, DataPlane: dataPlane, CreatedAt: metadata.CreatedAt,
		InfraComponents: apiInfraDiagnostics(metadata.InfraDiagnostics, metadata.InfraServices, metadata.Config.Identity.RouteGeneration, dataPlaneReady),
		ActionBindings:  append([]fastletapi.ActionBindingStatus(nil), metadata.ActionBindingStatuses...),
	}
}

func observationsForPhase(phase string, routeReady bool) (fastletapi.RuntimeObservation, fastletapi.DataPlaneObservation) {
	runtime := fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateUnknown}
	dataPlane := fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateUnknown}
	switch phase {
	case sandboxStateCreating, sandboxStateImagePending:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateCreating, fastletapi.DataPlaneStatePending
	case sandboxStateCreateFailed:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateFailed, fastletapi.DataPlaneStateFailed
	case sandboxStateInfraPending, sandboxStateInitializingInfra, sandboxStateRoutePending, sandboxStatePublishingRoute:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateReady, fastletapi.DataPlaneStatePublishing
	case sandboxStateInfraUnavailable, sandboxStateRouteUnavailable:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateReady, fastletapi.DataPlaneStateUnavailable
	case sandboxStateActionPending, sandboxStateActionUnavailable, sandboxStateRunning:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateReady, fastletapi.DataPlaneStateReady
	case sandboxStateTerminating, sandboxStateDeleting, sandboxStateCreateCleanup:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateStopping, fastletapi.DataPlaneStateDraining
	case sandboxStateDeleteFailed, sandboxStateCreateCleanupFailed:
		runtime.State, dataPlane.State = fastletapi.RuntimeStateFailed, fastletapi.DataPlaneStateFailed
	default:
		// An unrecognized internal phase is a Fastlet state-machine defect, not
		// a normal observation gap. Surface it explicitly instead of silently
		// projecting Unknown and retrying forever without a useful diagnosis.
		runtime.State, dataPlane.State = fastletapi.RuntimeStateFailed, fastletapi.DataPlaneStateFailed
		runtime.Message = "unrecognized internal Fastlet phase: " + phase
		dataPlane.Message = runtime.Message
	}
	if dataPlane.State == fastletapi.DataPlaneStateReady && !routeReady {
		dataPlane.State = fastletapi.DataPlaneStateUnavailable
		dataPlane.Message = "Fastlet proxy route is unavailable"
	}
	return runtime, dataPlane
}

func routeReadyForPhase(phase string) bool {
	return phase == sandboxStateRunning || phase == sandboxStateActionPending || phase == sandboxStateActionUnavailable
}

func apiInfraDiagnostics(
	diagnostics []fastletinfra.ComponentDiagnostic,
	services []fastletinfra.ServiceEndpoint,
	routeGeneration int64,
	routeReady bool,
) []fastletapi.InfraComponentDiagnostic {
	serviceByComponent := make(map[string]fastletinfra.ServiceEndpoint, len(services))
	for _, service := range services {
		serviceByComponent[service.Component] = service
	}
	result := make([]fastletapi.InfraComponentDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		service := serviceByComponent[diagnostic.Component]
		observedRouteGeneration := int64(0)
		if routeReady && diagnostic.State == string(apiv1alpha2.InfraComponentReady) {
			observedRouteGeneration = routeGeneration
		}
		result = append(result, fastletapi.InfraComponentDiagnostic{
			Component: diagnostic.Component, Protocol: service.Protocol, Port: service.Port,
			State: diagnostic.State, ObservedRouteGeneration: observedRouteGeneration, Message: diagnostic.Message,
		})
	}
	return result
}

func fastletError(code fastletapi.FastletErrorCode, message string, retryable bool) *fastletapi.FastletError {
	return &fastletapi.FastletError{Code: code, Message: message, Retryable: retryable}
}

func fastletErrorWithCause(code fastletapi.FastletErrorCode, message string, retryable bool, cause error) *fastletapi.FastletError {
	return &fastletapi.FastletError{Code: code, Message: message, Retryable: retryable, Cause: cause}
}

func defaultCreateDisposition(code fastletapi.FastletErrorCode) fastletapi.CreateDisposition {
	switch code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorProfileMismatch:
		return fastletapi.CreateDispositionRejectedBeforeSideEffects
	case fastletapi.ErrorInProgress:
		return fastletapi.CreateDispositionInProgress
	case fastletapi.ErrorGenerationFenced, fastletapi.ErrorStaleGeneration:
		return fastletapi.CreateDispositionGenerationFenced
	default:
		return fastletapi.CreateDispositionUnknown
	}
}

func createFailure(failure *fastletapi.FastletError, admission fastletapi.AdmissionStatus) (*fastletapi.CreateSandboxResponse, error) {
	return createFailureWithDisposition(failure, admission, defaultCreateDisposition(failure.Code))
}

func createFailureWithDisposition(failure *fastletapi.FastletError, admission fastletapi.AdmissionStatus, disposition fastletapi.CreateDisposition) (*fastletapi.CreateSandboxResponse, error) {
	if disposition == "" {
		disposition = fastletapi.CreateDispositionUnknown
	}
	return &fastletapi.CreateSandboxResponse{Disposition: disposition, Admission: admission, Error: failure}, failure
}
