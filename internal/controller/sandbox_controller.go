package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/runtimecatalog"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// Constants for controller configuration
const (
	// FinalizerName is the finalizer used to ensure cleanup before deletion
	FinalizerName = "sandbox.fast.io/cleanup"

	// HeartbeatTimeout is the duration after which an fastlet is considered unhealthy
	HeartbeatTimeout = 10 * time.Second

	// DefaultRequeueInterval is the default interval for periodic reconciliation
	DefaultRequeueInterval = 5 * time.Second

	// DeletionPollInterval is the interval for polling deletion status
	DeletionPollInterval = 2 * time.Second

	// ExpirationCheckThreshold is the threshold for scheduling expiration check
	ExpirationCheckThreshold = 30 * time.Second
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Registry      fastletpool.FastletRegistry
	FastletClient api.FastletAPIClient
	Catalog       *runtimecatalog.Catalog
}

// Reconcile is the main entry point for the Sandbox controller.
// It implements a state machine pattern for managing Sandbox lifecycle.
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the Sandbox instance
	var sandbox apiv1alpha1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Step 1: Ensure Finalizer is present
	if err := r.ensureFinalizer(ctx, &sandbox); err != nil {
		return ctrl.Result{}, err
	}

	// Step 2: Handle Deletion (if DeletionTimestamp is set)
	if sandbox.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &sandbox)
	}

	// Step 3: Handle Expiration (before other operations)
	if result, err, done := r.handleExpiration(ctx, &sandbox); done {
		return result, err
	}

	// Step 4: Handle Reset Request
	if result, err, done := r.handleReset(ctx, &sandbox); done {
		return result, err
	}

	// Step 5: Main State Machine - reconcile based on current phase
	return r.reconcilePhase(ctx, &sandbox)
}

// ============================================================================
// Finalizer Management
// ============================================================================

// ensureFinalizer ensures the cleanup finalizer is present on the Sandbox.
func (r *SandboxReconciler) ensureFinalizer(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	if controllerutil.ContainsFinalizer(sandbox, FinalizerName) {
		return nil
	}
	if sandbox.DeletionTimestamp != nil {
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		controllerutil.AddFinalizer(latest, FinalizerName)
		return r.Update(ctx, latest)
	})

	return err
}

// getSandboxID returns the sandboxID to use when calling Fastlet API.
// Logic:
// 1. If status.sandboxID is set, use it (Strong mode or already synced)
// 2. If label "fastpath-fast" exists, regenerate from annotation timestamp
// 3. Otherwise, fallback to UID (legacy/test sandboxes)
func (r *SandboxReconciler) getSandboxID(sandbox *apiv1alpha1.Sandbox) string {
	// 1. Status already has sandboxID (Strong mode or already synced)
	if sandbox.Status.SandboxID != "" {
		return sandbox.Status.SandboxID
	}

	// 2. Fast mode: regenerate from label + annotation
	if sandbox.Labels[common.LabelCreatedBy] == common.CreatedByFastPathFast {
		if tsStr, ok := sandbox.Annotations[common.AnnotationCreateTimestamp]; ok {
			if timestamp, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
				return idgen.GenerateHashID(sandbox.Name, sandbox.Namespace, timestamp)
			}
		}
	}

	// 3. Legacy fallback for CRDs created before this feature
	return string(sandbox.UID)
}

// ============================================================================
// Deletion State Machine
// ============================================================================

// handleDeletion processes the Sandbox deletion workflow.
// State transitions: Bound/Running → Terminating → (Fastlet confirms) → Removed
func (r *SandboxReconciler) handleDeletion(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	klog.Info("[SIMPLE-LOG] handleDeletion called for", "sandbox:", sandbox.Name)
	logger := klog.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(sandbox, FinalizerName) {
		return ctrl.Result{}, nil
	}

	phase := apiv1alpha1.SandboxPhase(sandbox.Status.Phase)

	switch phase {
	case apiv1alpha1.PhaseExpired:
		// Already expired - runtime resources are already cleaned, just remove finalizer
		logger.V(1).Info("Removing finalizer for expired sandbox")
		return r.removeFinalizer(ctx, sandbox)

	case apiv1alpha1.PhaseBound, apiv1alpha1.PhaseRunning:
		// Active sandbox - need to cleanup Fastlet resources
		return r.handleActiveDeletion(ctx, sandbox)

	case apiv1alpha1.PhaseTerminating:
		// Already terminating - wait for Fastlet confirmation
		return r.handleTerminatingDeletion(ctx, sandbox)

	default:
		// Pending, Failed, or unknown phase - no Fastlet resources to cleanup
		logger.V(1).Info("Removing finalizer for sandbox without Fastlet resources", "phase", phase)
		return r.removeFinalizer(ctx, sandbox)
	}
}

// handleActiveDeletion handles deletion of an active (Bound/Running) sandbox.
func (r *SandboxReconciler) handleActiveDeletion(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	logger.Info("[DEBUG-ACTIVE-DEL] handleActiveDeletion ENTER",
		"sandbox", sandbox.Name,
		"assignedFastlet", sandbox.Status.AssignedFastlet,
		"phase", sandbox.Status.Phase)

	// Check if Fastlet exists
	if sandbox.Status.AssignedFastlet == "" {
		logger.Info("[DEBUG-ACTIVE-DEL] No assigned pod, removing finalizer directly")
		return r.removeFinalizer(ctx, sandbox)
	}

	_, fastletExists := r.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.AssignedFastlet))
	if !fastletExists {
		// Fastlet doesn't exist - still try to release the allocated slot
		// This fixes the bug where Allocated was never decreased when Fastlet disappeared
		logger.Info("[BUG-FIX] Fastlet not found in registry during active deletion - attempting Release to free Allocated slot",
			"fastlet", sandbox.Status.AssignedFastlet)
		r.Registry.Release(fastletpool.FastletID(sandbox.Status.AssignedFastlet), sandbox)
		return r.removeFinalizer(ctx, sandbox)
	}

	logger.Info("[DEBUG-ACTIVE-DEL] Fastlet exists, calling deleteFromFastlet",
		"fastletID", fastletpool.FastletID(sandbox.Status.AssignedFastlet))

	// Call Fastlet to delete the sandbox
	if err := r.deleteFromFastlet(ctx, sandbox); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete from fastlet: %w", err)
	}

	// Transition to Terminating phase
	if err := r.updatePhase(ctx, sandbox, apiv1alpha1.PhaseTerminating); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("[DEBUG-ACTIVE-DEL] Transitioning to Terminating, will requeue after", "seconds", DeletionPollInterval)
	return ctrl.Result{RequeueAfter: DeletionPollInterval}, nil
}

// handleTerminatingDeletion handles a sandbox in Terminating phase.
func (r *SandboxReconciler) handleTerminatingDeletion(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	logger.Info("[DEBUG-TERM] handleTerminatingDeletion ENTER",
		"sandbox", sandbox.Name,
		"assignedFastlet", sandbox.Status.AssignedFastlet,
		"deletionTimestamp", sandbox.DeletionTimestamp)

	// Check for reset request - reset takes priority over graceful shutdown
	// This allows TestControlledRecovery to proceed when ResetRevision is set during termination
	if sandbox.Spec.ResetRevision != nil && !sandbox.Spec.ResetRevision.IsZero() {
		if sandbox.Status.AcceptedResetRevision == nil ||
			sandbox.Spec.ResetRevision.After(sandbox.Status.AcceptedResetRevision.Time) {
			logger.Info("[DEBUG-TERM] Reset request detected during termination - processing reset first")
			// Process reset: clear status and set AcceptedResetRevision
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &apiv1alpha1.Sandbox{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
					return err
				}
				latest.Status.AssignedFastlet = ""
				latest.Status.SandboxID = ""
				latest.Status.Phase = string(apiv1alpha1.PhasePending)
				latest.Status.AcceptedResetRevision = sandbox.Spec.ResetRevision
				return r.Status().Update(ctx, latest)
			})
			if err != nil {
				return ctrl.Result{}, err
			}
			// Release the old fastlet resource
			r.Registry.Release(fastletpool.FastletID(sandbox.Status.AssignedFastlet), sandbox)
			logger.Info("[DEBUG-TERM] Reset processed successfully, sandbox transitioning to Pending")
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Check if Fastlet still exists
	fastlet, fastletExists := r.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.AssignedFastlet))
	logger.Info("[DEBUG-TERM] Fastlet existence check",
		"fastletID", fastletpool.FastletID(sandbox.Status.AssignedFastlet),
		"fastletExists", fastletExists)

	if !fastletExists {
		// Fastlet gone - still try to release in case the slot still exists
		// The Release function handles the case where the slot doesn't exist (no-op)
		// This fixes the bug where Allocated was never decreased when Fastlet disappeared
		logger.Info("[BUG-FIX] Fastlet disappeared during termination - attempting Release to free Allocated slot",
			"fastletID", fastletpool.FastletID(sandbox.Status.AssignedFastlet))
		r.Registry.Release(fastletpool.FastletID(sandbox.Status.AssignedFastlet), sandbox)
		return r.removeFinalizer(ctx, sandbox)
	}

	// Check Fastlet-reported status
	fastletStatus, hasStatus := fastlet.SandboxStatuses[r.getSandboxID(sandbox)]
	logger.Info("[DEBUG-TERM] Fastlet status check",
		"hasStatus", hasStatus,
		"phase", func() string {
			if hasStatus {
				return fastletStatus.Phase
			} else {
				return "<none>"
			}
		}(),
		"fastletAllocated", fastlet.Allocated)

	if !hasStatus {
		// Fastlet no longer reports this sandbox = deletion confirmed
		// Release resources and remove finalizer
		logger.Info("[DEBUG-TERM] Fastlet no longer reports sandbox - deletion confirmed, calling Registry.Release",
			"fastletAllocatedBefore", fastlet.Allocated)
		r.Registry.Release(fastletpool.FastletID(sandbox.Status.AssignedFastlet), sandbox)
		return r.removeFinalizer(ctx, sandbox)
	}

	// Still terminating - continue waiting
	logger.Info("[DEBUG-TERM] Still waiting for Fastlet termination",
		"currentPhase", fastletStatus.Phase,
		"willRequeueAfter", DeletionPollInterval)
	return ctrl.Result{RequeueAfter: DeletionPollInterval}, nil
}

// removeFinalizer removes the cleanup finalizer from the Sandbox.
func (r *SandboxReconciler) removeFinalizer(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(latest, FinalizerName)
		return r.Update(ctx, latest)
	})
	return ctrl.Result{}, err
}

// ============================================================================
// Expiration Handling
// ============================================================================

// handleExpiration checks and processes sandbox expiration.
// Returns (result, error, done) where done=true means the sandbox was expired/processed.
func (r *SandboxReconciler) handleExpiration(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error, bool) {
	// Skip if no expiration time set
	if sandbox.Spec.ExpireTime == nil || sandbox.Spec.ExpireTime.IsZero() {
		return ctrl.Result{}, nil, false
	}

	// Already expired - nothing to do
	if apiv1alpha1.SandboxPhase(sandbox.Status.Phase) == apiv1alpha1.PhaseExpired {
		return ctrl.Result{}, nil, true
	}

	now := time.Now()
	expireTime := sandbox.Spec.ExpireTime.Time

	if now.After(expireTime) {
		// Sandbox has expired - clean up runtime but keep CRD
		return r.processExpiration(ctx, sandbox)
	}

	// Schedule requeue before expiration if within threshold
	remaining := time.Until(expireTime)
	if remaining > 0 && remaining < ExpirationCheckThreshold {
		return ctrl.Result{RequeueAfter: remaining}, nil, true
	}

	return ctrl.Result{}, nil, false
}

// processExpiration cleans up an expired sandbox.
func (r *SandboxReconciler) processExpiration(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error, bool) {
	logger := klog.FromContext(ctx)
	logger.Info("Processing sandbox expiration")

	// Delete runtime from Fastlet if assigned
	if sandbox.Status.AssignedFastlet != "" {
		if err := r.deleteFromFastlet(ctx, sandbox); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete expired sandbox from fastlet: %w", err), true
		}
		r.Registry.Release(fastletpool.FastletID(sandbox.Status.AssignedFastlet), sandbox)
	}

	// Update status to Expired
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		latest.Status.Phase = string(apiv1alpha1.PhaseExpired)
		latest.Status.AssignedFastlet = ""
		latest.Status.SandboxID = ""
		return r.Status().Update(ctx, latest)
	})

	logger.Info("Sandbox expired and cleaned up")
	return ctrl.Result{}, err, true
}

// ============================================================================
// Reset Handling
// ============================================================================

// handleReset processes reset requests triggered by ResetRevision changes.
// Returns (result, error, done) where done=true means reset was processed.
func (r *SandboxReconciler) handleReset(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error, bool) {
	// Skip if no reset revision set
	if sandbox.Spec.ResetRevision == nil || sandbox.Spec.ResetRevision.IsZero() {
		return ctrl.Result{}, nil, false
	}

	// Skip if already processed
	if sandbox.Status.AcceptedResetRevision != nil &&
		!sandbox.Spec.ResetRevision.After(sandbox.Status.AcceptedResetRevision.Time) {
		return ctrl.Result{}, nil, false
	}

	logger := klog.FromContext(ctx)
	logger.Info("Processing reset request")

	// Clean up existing Fastlet resources
	if sandbox.Status.AssignedFastlet != "" {
		// Delete from Fastlet first (fix for BUG-03)
		if err := r.deleteFromFastlet(ctx, sandbox); err != nil {
			// Log but don't block - reset takes priority
			logger.Error(err, "Failed to delete old sandbox from fastlet during reset")
		}
		r.Registry.Release(fastletpool.FastletID(sandbox.Status.AssignedFastlet), sandbox)
	}

	// Reset status to Pending for rescheduling
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		latest.Status.AssignedFastlet = ""
		latest.Status.SandboxID = ""
		latest.Status.Phase = string(apiv1alpha1.PhasePending)
		latest.Status.AcceptedResetRevision = sandbox.Spec.ResetRevision
		return r.Status().Update(ctx, latest)
	})

	logger.Info("Sandbox reset complete, pending rescheduling")
	return ctrl.Result{Requeue: true}, err, true
}

// ============================================================================
// Main State Machine
// ============================================================================

// reconcilePhase routes to the appropriate handler based on current phase.
func (r *SandboxReconciler) reconcilePhase(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	phase := apiv1alpha1.SandboxPhase(sandbox.Status.Phase)

	switch phase {
	case "", apiv1alpha1.PhasePending:
		return r.reconcilePending(ctx, sandbox)

	case apiv1alpha1.PhaseBound, apiv1alpha1.PhaseRunning:
		return r.reconcileRunning(ctx, sandbox)

	case apiv1alpha1.PhaseExpired:
		// Expired sandboxes are kept for history, no action needed
		return ctrl.Result{}, nil

	case apiv1alpha1.PhaseFailed:
		// Failed sandboxes need manual intervention
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil

	case apiv1alpha1.PhaseLost:
		// Lost phase - Fastlet was lost under Manual policy, waiting for user intervention
		// Check if a new Fastlet is available for rescheduling
		return r.reconcileLost(ctx, sandbox)

	default:
		// Unknown phase - requeue for later
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}
}

// reconcilePending handles sandboxes in Pending phase.
// Workflow: Schedule → Create on Fastlet → Transition to Bound
func (r *SandboxReconciler) reconcilePending(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	// === Step 0: 搬运 allocation annotation 到 status ===
	allocInfo, err := common.ParseAllocationInfo(sandbox.Annotations)
	if err != nil {
		logger.Error(err, "Failed to parse allocation annotation, clearing it")
		r.clearAllocationAnnotation(ctx, sandbox)
		return ctrl.Result{Requeue: true}, nil
	}
	if allocInfo != nil {
		logger.Info("Found allocation annotation from FastPath, moving to status",
			"assignedFastlet", allocInfo.AssignedFastlet, "assignedNode", allocInfo.AssignedNode)

		if err := r.moveAllocationToStatus(ctx, sandbox, allocInfo); err != nil {
			logger.Error(err, "Failed to move allocation to status")
			return ctrl.Result{}, err
		}

		logger.Info("Allocation moved to status, annotation cleared, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	// === 原有逻辑保持不变 ===
	// Step 1: Scheduling (if not yet assigned)
	if sandbox.Status.AssignedFastlet == "" {
		return r.handleScheduling(ctx, sandbox)
	}

	// Step 2: Validate Fastlet availability
	fastlet, fastletExists := r.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.AssignedFastlet))
	if !fastletExists {
		return r.handleFastletLost(ctx, sandbox)
	}

	// Step 3: Check Fastlet heartbeat
	heartbeatAge := time.Since(fastlet.LastHeartbeat)
	if heartbeatAge >= HeartbeatTimeout {
		logger.V(1).Info("Fastlet heartbeat timeout, waiting for cleanup", "age", heartbeatAge)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Step 4: Create sandbox on Fastlet
	if err := r.handleCreateOnFastlet(ctx, sandbox); err != nil {
		logger.Error(err, "Failed to create sandbox on fastlet")
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Step 5: Transition to Bound
	if err := r.updatePhase(ctx, sandbox, apiv1alpha1.PhaseBound); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Sandbox created on fastlet, transitioning to Bound", "sandbox", sandbox.Name)
	return ctrl.Result{RequeueAfter: 0}, nil
}

// reconcileRunning handles sandboxes in Bound/Running phase.
// Workflow: Sync status from Fastlet, handle Fastlet loss
func (r *SandboxReconciler) reconcileRunning(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	// Validate Fastlet exists
	fastlet, fastletExists := r.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.AssignedFastlet))
	if !fastletExists {
		return r.handleFastletLost(ctx, sandbox)
	}

	// Check heartbeat
	heartbeatAge := time.Since(fastlet.LastHeartbeat)
	if heartbeatAge >= HeartbeatTimeout {
		logger.V(1).Info("Fastlet heartbeat timeout, waiting for cleanup", "age", heartbeatAge)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Sync status from Fastlet
	if err := r.syncStatusFromFastlet(ctx, sandbox, &fastlet); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

// reconcileLost handles sandboxes in Lost phase.
// Workflow: Wait for new Fastlet to become available, then transition to Pending for rescheduling.
func (r *SandboxReconciler) reconcileLost(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	// Check if any Fastlet is available for the sandbox's pool
	fastlet, err := r.Registry.Allocate(sandbox)
	if err != nil {
		// No fastlet available yet, continue waiting
		logger.V(1).Info("Waiting for available fastlet for rescheduling", "error", err)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Fastlet available - transition to Pending for rescheduling
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		// Guard against concurrent updates
		if latest.Status.Phase != string(apiv1alpha1.PhaseLost) {
			return fmt.Errorf("sandbox phase changed from Lost, aborting reschedule")
		}
		latest.Status.AssignedFastlet = fastlet.PodName
		latest.Status.NodeName = fastlet.NodeName
		latest.Status.Phase = string(apiv1alpha1.PhasePending)
		return r.Status().Update(ctx, latest)
	})

	if err != nil {
		// Scheduling failed - release the allocation
		r.Registry.Release(fastlet.ID, sandbox)
		return ctrl.Result{}, err
	}

	logger.Info("Fastlet available for rescheduling, transitioning from Lost to Pending", "fastlet", fastlet.PodName)
	return ctrl.Result{Requeue: true}, nil
}

// ============================================================================
// Scheduling
// ============================================================================

// handleScheduling allocates an Fastlet for the Sandbox.
func (r *SandboxReconciler) handleScheduling(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	fastlet, err := r.Registry.Allocate(sandbox)
	if err != nil {
		logger.V(1).Info("No available fastlet for scheduling", "error", err)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Update status with assignment
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		// Guard against concurrent scheduling
		if latest.Status.AssignedFastlet != "" {
			return fmt.Errorf("sandbox already scheduled to %s", latest.Status.AssignedFastlet)
		}
		latest.Status.AssignedFastlet = fastlet.PodName
		latest.Status.NodeName = fastlet.NodeName
		latest.Status.Phase = string(apiv1alpha1.PhasePending)
		return r.Status().Update(ctx, latest)
	})

	if err != nil {
		// Scheduling failed - release the allocation
		r.Registry.Release(fastlet.ID, sandbox)
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Sandbox scheduled to fastlet", "fastlet", fastlet.PodName, "node", fastlet.NodeName)
	return ctrl.Result{Requeue: true}, nil
}

// ============================================================================
// Fastlet Interaction
// ============================================================================

// handleFastletLost handles the case when the assigned Fastlet is no longer available.
func (r *SandboxReconciler) handleFastletLost(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)
	logger.Info("Assigned fastlet lost", "fastlet", sandbox.Status.AssignedFastlet)

	if sandbox.Spec.FailurePolicy == apiv1alpha1.FailurePolicyAutoRecreate {
		// AutoRecreate: clear assignment and reschedule
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &apiv1alpha1.Sandbox{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
				return err
			}
			// Guard against concurrent updates
			if latest.Status.AssignedFastlet != sandbox.Status.AssignedFastlet {
				return nil // Another reconcile already handled this
			}
			latest.Status.AssignedFastlet = ""
			latest.Status.SandboxID = ""
			latest.Status.Phase = string(apiv1alpha1.PhasePending)
			return r.Status().Update(ctx, latest)
		})

		logger.Info("Fastlet lost - triggering AutoRecreate")
		return ctrl.Result{Requeue: true}, err
	}

	// Manual policy: transition to Lost phase to explicitly indicate fastlet loss
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		// Guard against concurrent updates
		if latest.Status.Phase == string(apiv1alpha1.PhaseLost) {
			return nil // Already in Lost phase
		}
		latest.Status.Phase = string(apiv1alpha1.PhaseLost)
		latest.Status.AssignedFastlet = ""
		latest.Status.SandboxID = ""
		return r.Status().Update(ctx, latest)
	})

	logger.Info("Fastlet lost - Manual policy, transitioning to Lost phase")
	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, err
}

// handleCreateOnFastlet sends a create request to the Fastlet.
func (r *SandboxReconciler) handleCreateOnFastlet(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	fastlet, ok := r.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.AssignedFastlet))
	if !ok {
		return fmt.Errorf("fastlet %s not found in registry", sandbox.Status.AssignedFastlet)
	}

	var pool apiv1alpha1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{Name: sandbox.Spec.PoolRef, Namespace: sandbox.Namespace}, &pool); err != nil {
		return fmt.Errorf("get SandboxPool %s: %w", sandbox.Spec.PoolRef, err)
	}
	runtimeName, err := pool.Spec.EffectiveRuntime()
	if err != nil {
		return fmt.Errorf("resolve Pool runtime: %w", err)
	}
	catalog := r.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	profile, err := catalog.Resolve(runtimeName)
	if err != nil {
		return fmt.Errorf("resolve runtime profile: %w", err)
	}
	sandboxResources, err := pool.Spec.EffectiveSandboxResources()
	if err != nil {
		return err
	}

	_, err = r.FastletClient.CreateSandbox(fastlet.PodIP, &api.CreateSandboxRequest{
		Sandbox: api.SandboxSpec{
			SandboxID:           r.getSandboxID(sandbox),
			ClaimName:           sandbox.Name,
			Image:               sandbox.Spec.Image,
			Command:             sandbox.Spec.Command,
			Args:                sandbox.Spec.Args,
			Env:                 envVarToMap(sandbox.Spec.Envs),
			WorkingDir:          sandbox.Spec.WorkingDir,
			CPU:                 sandboxResources.CPU.String(),
			Memory:              sandboxResources.Memory.String(),
			PIDs:                sandboxResources.PIDs,
			RuntimeProfileHash:  profile.ProfileHash,
			ResourceProfileHash: sandboxResources.Hash(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create sandbox on fastlet %s: %w", fastlet.PodIP, err)
	}
	return nil
}

// deleteFromFastlet sends a delete request to the Fastlet.
func (r *SandboxReconciler) deleteFromFastlet(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	klog.Info("[DEBUG-DELETE-FROM-FASTLET] ENTER",
		"sandbox", sandbox.Name,
		"assignedFastlet", sandbox.Status.AssignedFastlet)

	fastlet, ok := r.Registry.GetFastletByID(fastletpool.FastletID(sandbox.Status.AssignedFastlet))
	if !ok {
		// Fastlet not found - nothing to delete
		klog.Warning("[DEBUG-DELETE-FROM-FASTLET] Fastlet not found in registry",
			"fastletID", fastletpool.FastletID(sandbox.Status.AssignedFastlet))
		return nil
	}

	klog.Info("[DEBUG-DELETE-FROM-FASTLET] Calling Fastlet DeleteSandbox API",
		"fastletPodIP", fastlet.PodIP,
		"name", sandbox.Name,
		"sandboxID", r.getSandboxID(sandbox))

	_, err := r.FastletClient.DeleteSandbox(fastlet.PodIP, &api.DeleteSandboxRequest{
		SandboxID: r.getSandboxID(sandbox),
	})
	if err != nil {
		klog.Error("[DEBUG-DELETE-FROM-FASTLET] DeleteSandbox API failed", "err", err)
		return fmt.Errorf("failed to delete sandbox from fastlet %s: %w", fastlet.PodIP, err)
	}

	klog.Info("[DEBUG-DELETE-FROM-FASTLET] DeleteSandbox API called successfully")
	return nil
}

// syncStatusFromFastlet synchronizes sandbox status from Fastlet's reported status.
func (r *SandboxReconciler) syncStatusFromFastlet(ctx context.Context, sandbox *apiv1alpha1.Sandbox, fastlet *fastletpool.FastletInfo) error {
	// Fastlet statuses are keyed by SandboxID (hash or UID), not by name
	status, hasStatus := fastlet.SandboxStatuses[r.getSandboxID(sandbox)]
	if !hasStatus {
		return nil
	}

	// Map Fastlet phase to Controller phase
	controllerPhase := mapFastletPhaseToController(status.Phase)

	// Skip status sync if sandbox has been reset (AcceptedResetRevision is set)
	// This prevents Fastlet-reported Terminated state from overwriting the reset state
	if sandbox.Status.AcceptedResetRevision != nil {
		return nil
	}

	// Check if update is needed
	if sandbox.Status.Phase == string(controllerPhase) && sandbox.Status.SandboxID == status.SandboxID {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}

		latest.Status.Phase = string(controllerPhase)
		latest.Status.SandboxID = status.SandboxID

		// Update endpoints if ports are exposed
		if len(latest.Spec.ExposedPorts) > 0 && fastlet.PodIP != "" {
			endpoints := make([]string, 0, len(latest.Spec.ExposedPorts))
			for _, port := range latest.Spec.ExposedPorts {
				endpoints = append(endpoints, fmt.Sprintf("%s:%d", fastlet.PodIP, port))
			}
			latest.Status.Endpoints = endpoints
		}

		return r.Status().Update(ctx, latest)
	})
}

// mapFastletPhaseToController maps Fastlet-reported phase to Controller standard phase.
// Fastlet uses lowercase (running, terminated), Controller uses TitleCase (Running, Terminated).
func mapFastletPhaseToController(fastletPhase string) apiv1alpha1.SandboxPhase {
	switch apiv1alpha1.FastletSandboxPhase(fastletPhase) {
	case apiv1alpha1.FastletPhaseRunning:
		return apiv1alpha1.PhaseRunning
	case apiv1alpha1.FastletPhaseCreating:
		return apiv1alpha1.PhaseBound // Still creating, keep as Bound
	case apiv1alpha1.FastletPhaseFailed:
		return apiv1alpha1.PhaseFailed
	case apiv1alpha1.FastletPhaseStopped:
		return apiv1alpha1.PhaseFailed // Stopped unexpectedly
	case apiv1alpha1.FastletPhaseTerminated:
		return apiv1alpha1.PhaseTerminating // Being deleted
	default:
		// Unknown phase - return as-is converted to SandboxPhase
		// This handles any future phases gracefully
		return apiv1alpha1.SandboxPhase(fastletPhase)
	}
}

// ============================================================================
// Helpers
// ============================================================================

// updatePhase updates the sandbox phase.
func (r *SandboxReconciler) updatePhase(ctx context.Context, sandbox *apiv1alpha1.Sandbox, phase apiv1alpha1.SandboxPhase) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		latest.Status.Phase = string(phase)
		return r.Status().Update(ctx, latest)
	})
}

// envVarToMap converts K8s EnvVar slice to map[string]string
func envVarToMap(envs []corev1.EnvVar) map[string]string {
	result := make(map[string]string, len(envs))
	for _, e := range envs {
		result[e.Name] = e.Value
	}
	return result
}

// moveAllocationToStatus 搬运 annotation 到 status，然后删除 annotation
func (r *SandboxReconciler) moveAllocationToStatus(ctx context.Context, sandbox *apiv1alpha1.Sandbox, allocInfo *common.AllocationInfo) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}

		latest.Status.AssignedFastlet = allocInfo.AssignedFastlet
		latest.Status.NodeName = allocInfo.AssignedNode
		latest.Status.Phase = string(apiv1alpha1.PhaseBound)
		return r.Status().Update(ctx, latest)
	})
}

// clearAllocationAnnotation 清除损坏的 annotation
func (r *SandboxReconciler) clearAllocationAnnotation(ctx context.Context, sandbox *apiv1alpha1.Sandbox) {
	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		if latest.Annotations != nil {
			delete(latest.Annotations, common.AnnotationAllocation)
		}
		return r.Update(ctx, latest)
	})
}

// ============================================================================
// Controller Setup
// ============================================================================

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create index for efficient lookup of unassigned sandboxes
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&apiv1alpha1.Sandbox{},
		"status.assignedFastlet",
		func(o client.Object) []string {
			return []string{o.(*apiv1alpha1.Sandbox).Status.AssignedFastlet}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.Sandbox{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToSandboxes),
		).
		Complete(r)
}

// mapPodToSandboxes returns reconcile requests for unassigned sandboxes when an fastlet pod becomes ready.
func (r *SandboxReconciler) mapPodToSandboxes(ctx context.Context, obj client.Object) []ctrl.Request {
	pod := obj.(*corev1.Pod)

	// Only trigger for running fastlet pods
	if pod.Labels["app"] != "sandbox-fastlet" || pod.Status.Phase != corev1.PodRunning {
		return nil
	}

	// Request reconciliation for all unassigned sandboxes
	var sandboxList apiv1alpha1.SandboxList
	if err := r.List(ctx, &sandboxList, client.MatchingFields{"status.assignedFastlet": ""}); err != nil {
		return nil
	}

	requests := make([]ctrl.Request, 0, len(sandboxList.Items))
	for _, sb := range sandboxList.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(&sb),
		})
	}

	return requests
}
