package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/agentpool"
	"fast-sandbox/internal/controller/common"
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

	// HeartbeatTimeout is the duration after which an agent is considered unhealthy
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
	Scheme      *runtime.Scheme
	Registry    agentpool.AgentRegistry
	AgentClient api.AgentAPIClient
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

// getSandboxID returns the sandboxID to use when calling Agent API.
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
// State transitions: Bound/Running → Terminating → (Agent confirms) → Removed
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
		// Active sandbox - need to cleanup Agent resources
		return r.handleActiveDeletion(ctx, sandbox)

	case apiv1alpha1.PhaseTerminating:
		// Already terminating - wait for Agent confirmation
		return r.handleTerminatingDeletion(ctx, sandbox)

	default:
		// Pending, Failed, or unknown phase - no Agent resources to cleanup
		logger.V(1).Info("Removing finalizer for sandbox without Agent resources", "phase", phase)
		return r.removeFinalizer(ctx, sandbox)
	}
}

// handleActiveDeletion handles deletion of an active (Bound/Running) sandbox.
func (r *SandboxReconciler) handleActiveDeletion(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	logger.Info("[DEBUG-ACTIVE-DEL] handleActiveDeletion ENTER",
		"sandbox", sandbox.Name,
		"assignedPod", sandbox.Status.AssignedPod,
		"phase", sandbox.Status.Phase)

	// Check if Agent exists
	if sandbox.Status.AssignedPod == "" {
		logger.Info("[DEBUG-ACTIVE-DEL] No assigned pod, removing finalizer directly")
		return r.removeFinalizer(ctx, sandbox)
	}

	_, agentExists := r.Registry.GetAgentByID(agentpool.AgentID(sandbox.Status.AssignedPod))
	if !agentExists {
		// Agent doesn't exist - still try to release the allocated slot
		// This fixes the bug where Allocated was never decreased when Agent disappeared
		logger.Info("[BUG-FIX] Agent not found in registry during active deletion - attempting Release to free Allocated slot",
			"agent", sandbox.Status.AssignedPod)
		r.Registry.Release(agentpool.AgentID(sandbox.Status.AssignedPod), sandbox)
		return r.removeFinalizer(ctx, sandbox)
	}

	logger.Info("[DEBUG-ACTIVE-DEL] Agent exists, calling deleteFromAgent",
		"agentID", agentpool.AgentID(sandbox.Status.AssignedPod))

	// Call Agent to delete the sandbox
	if err := r.deleteFromAgent(ctx, sandbox); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete from agent: %w", err)
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
		"assignedPod", sandbox.Status.AssignedPod,
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
				latest.Status.AssignedPod = ""
				latest.Status.SandboxID = ""
				latest.Status.Phase = string(apiv1alpha1.PhasePending)
				latest.Status.AcceptedResetRevision = sandbox.Spec.ResetRevision
				return r.Status().Update(ctx, latest)
			})
			if err != nil {
				return ctrl.Result{}, err
			}
			// Release the old agent resource
			r.Registry.Release(agentpool.AgentID(sandbox.Status.AssignedPod), sandbox)
			logger.Info("[DEBUG-TERM] Reset processed successfully, sandbox transitioning to Pending")
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Check if Agent still exists
	agent, agentExists := r.Registry.GetAgentByID(agentpool.AgentID(sandbox.Status.AssignedPod))
	logger.Info("[DEBUG-TERM] Agent existence check",
		"agentID", agentpool.AgentID(sandbox.Status.AssignedPod),
		"agentExists", agentExists)

	if !agentExists {
		// Agent gone - still try to release in case the slot still exists
		// The Release function handles the case where the slot doesn't exist (no-op)
		// This fixes the bug where Allocated was never decreased when Agent disappeared
		logger.Info("[BUG-FIX] Agent disappeared during termination - attempting Release to free Allocated slot",
			"agentID", agentpool.AgentID(sandbox.Status.AssignedPod))
		r.Registry.Release(agentpool.AgentID(sandbox.Status.AssignedPod), sandbox)
		return r.removeFinalizer(ctx, sandbox)
	}

	// Check Agent-reported status
	agentStatus, hasStatus := agent.SandboxStatuses[r.getSandboxID(sandbox)]
	logger.Info("[DEBUG-TERM] Agent status check",
		"hasStatus", hasStatus,
		"phase", func() string {
			if hasStatus {
				return agentStatus.Phase
			} else {
				return "<none>"
			}
		}(),
		"agentAllocated", agent.Allocated)

	if !hasStatus {
		// Agent no longer reports this sandbox = deletion confirmed
		// Release resources and remove finalizer
		logger.Info("[DEBUG-TERM] Agent no longer reports sandbox - deletion confirmed, calling Registry.Release",
			"agentAllocatedBefore", agent.Allocated)
		r.Registry.Release(agentpool.AgentID(sandbox.Status.AssignedPod), sandbox)
		return r.removeFinalizer(ctx, sandbox)
	}

	// Still terminating - continue waiting
	logger.Info("[DEBUG-TERM] Still waiting for Agent termination",
		"currentPhase", agentStatus.Phase,
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

	// Delete runtime from Agent if assigned
	if sandbox.Status.AssignedPod != "" {
		if err := r.deleteFromAgent(ctx, sandbox); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete expired sandbox from agent: %w", err), true
		}
		r.Registry.Release(agentpool.AgentID(sandbox.Status.AssignedPod), sandbox)
	}

	// Update status to Expired
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		latest.Status.Phase = string(apiv1alpha1.PhaseExpired)
		latest.Status.AssignedPod = ""
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

	// Clean up existing Agent resources
	if sandbox.Status.AssignedPod != "" {
		// Delete from Agent first (fix for BUG-03)
		if err := r.deleteFromAgent(ctx, sandbox); err != nil {
			// Log but don't block - reset takes priority
			logger.Error(err, "Failed to delete old sandbox from agent during reset")
		}
		r.Registry.Release(agentpool.AgentID(sandbox.Status.AssignedPod), sandbox)
	}

	// Reset status to Pending for rescheduling
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		latest.Status.AssignedPod = ""
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
		// Lost phase - Agent was lost under Manual policy, waiting for user intervention
		// Check if a new Agent is available for rescheduling
		return r.reconcileLost(ctx, sandbox)

	default:
		// Unknown phase - requeue for later
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}
}

// reconcilePending handles sandboxes in Pending phase.
// Workflow: Schedule → Create on Agent → Transition to Bound
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
			"assignedPod", allocInfo.AssignedPod, "assignedNode", allocInfo.AssignedNode)

		if err := r.moveAllocationToStatus(ctx, sandbox, allocInfo); err != nil {
			logger.Error(err, "Failed to move allocation to status")
			return ctrl.Result{}, err
		}

		logger.Info("Allocation moved to status, annotation cleared, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	// === 原有逻辑保持不变 ===
	// Step 1: Scheduling (if not yet assigned)
	if sandbox.Status.AssignedPod == "" {
		return r.handleScheduling(ctx, sandbox)
	}

	// Step 2: Validate Agent availability
	agent, agentExists := r.Registry.GetAgentByID(agentpool.AgentID(sandbox.Status.AssignedPod))
	if !agentExists {
		return r.handleAgentLost(ctx, sandbox)
	}

	// Step 3: Check Agent heartbeat
	heartbeatAge := time.Since(agent.LastHeartbeat)
	if heartbeatAge >= HeartbeatTimeout {
		logger.V(1).Info("Agent heartbeat timeout, waiting for cleanup", "age", heartbeatAge)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Step 4: Create sandbox on Agent
	if err := r.handleCreateOnAgent(ctx, sandbox); err != nil {
		logger.Error(err, "Failed to create sandbox on agent")
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Step 5: Transition to Bound
	if err := r.updatePhase(ctx, sandbox, apiv1alpha1.PhaseBound); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Sandbox created on agent, transitioning to Bound", "sandbox", sandbox.Name)
	return ctrl.Result{RequeueAfter: 0}, nil
}

// reconcileRunning handles sandboxes in Bound/Running phase.
// Workflow: Sync status from Agent, handle Agent loss
func (r *SandboxReconciler) reconcileRunning(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	// Validate Agent exists
	agent, agentExists := r.Registry.GetAgentByID(agentpool.AgentID(sandbox.Status.AssignedPod))
	if !agentExists {
		return r.handleAgentLost(ctx, sandbox)
	}

	// Check heartbeat
	heartbeatAge := time.Since(agent.LastHeartbeat)
	if heartbeatAge >= HeartbeatTimeout {
		logger.V(1).Info("Agent heartbeat timeout, waiting for cleanup", "age", heartbeatAge)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Sync status from Agent
	if err := r.syncStatusFromAgent(ctx, sandbox, &agent); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

// reconcileLost handles sandboxes in Lost phase.
// Workflow: Wait for new Agent to become available, then transition to Pending for rescheduling.
func (r *SandboxReconciler) reconcileLost(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	// Check if any Agent is available for the sandbox's pool
	agent, err := r.Registry.Allocate(sandbox)
	if err != nil {
		// No agent available yet, continue waiting
		logger.V(1).Info("Waiting for available agent for rescheduling", "error", err)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Agent available - transition to Pending for rescheduling
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		// Guard against concurrent updates
		if latest.Status.Phase != string(apiv1alpha1.PhaseLost) {
			return fmt.Errorf("sandbox phase changed from Lost, aborting reschedule")
		}
		latest.Status.AssignedPod = agent.PodName
		latest.Status.NodeName = agent.NodeName
		latest.Status.Phase = string(apiv1alpha1.PhasePending)
		return r.Status().Update(ctx, latest)
	})

	if err != nil {
		// Scheduling failed - release the allocation
		r.Registry.Release(agent.ID, sandbox)
		return ctrl.Result{}, err
	}

	logger.Info("Agent available for rescheduling, transitioning from Lost to Pending", "agent", agent.PodName)
	return ctrl.Result{Requeue: true}, nil
}

// ============================================================================
// Scheduling
// ============================================================================

// handleScheduling allocates an Agent for the Sandbox.
func (r *SandboxReconciler) handleScheduling(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	agent, err := r.Registry.Allocate(sandbox)
	if err != nil {
		logger.V(1).Info("No available agent for scheduling", "error", err)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Update status with assignment
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apiv1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return err
		}
		// Guard against concurrent scheduling
		if latest.Status.AssignedPod != "" {
			return fmt.Errorf("sandbox already scheduled to %s", latest.Status.AssignedPod)
		}
		latest.Status.AssignedPod = agent.PodName
		latest.Status.NodeName = agent.NodeName
		latest.Status.Phase = string(apiv1alpha1.PhasePending)
		return r.Status().Update(ctx, latest)
	})

	if err != nil {
		// Scheduling failed - release the allocation
		r.Registry.Release(agent.ID, sandbox)
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Sandbox scheduled to agent", "agent", agent.PodName, "node", agent.NodeName)
	return ctrl.Result{Requeue: true}, nil
}

// ============================================================================
// Agent Interaction
// ============================================================================

// handleAgentLost handles the case when the assigned Agent is no longer available.
func (r *SandboxReconciler) handleAgentLost(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)
	logger.Info("Assigned agent lost", "agent", sandbox.Status.AssignedPod)

	if sandbox.Spec.FailurePolicy == apiv1alpha1.FailurePolicyAutoRecreate {
		// AutoRecreate: clear assignment and reschedule
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &apiv1alpha1.Sandbox{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
				return err
			}
			// Guard against concurrent updates
			if latest.Status.AssignedPod != sandbox.Status.AssignedPod {
				return nil // Another reconcile already handled this
			}
			latest.Status.AssignedPod = ""
			latest.Status.SandboxID = ""
			latest.Status.Phase = string(apiv1alpha1.PhasePending)
			return r.Status().Update(ctx, latest)
		})

		logger.Info("Agent lost - triggering AutoRecreate")
		return ctrl.Result{Requeue: true}, err
	}

	// Manual policy: transition to Lost phase to explicitly indicate agent loss
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
		latest.Status.AssignedPod = ""
		latest.Status.SandboxID = ""
		return r.Status().Update(ctx, latest)
	})

	logger.Info("Agent lost - Manual policy, transitioning to Lost phase")
	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, err
}

// handleCreateOnAgent sends a create request to the Agent.
func (r *SandboxReconciler) handleCreateOnAgent(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	agent, ok := r.Registry.GetAgentByID(agentpool.AgentID(sandbox.Status.AssignedPod))
	if !ok {
		return fmt.Errorf("agent %s not found in registry", sandbox.Status.AssignedPod)
	}

	_, err := r.AgentClient.CreateSandbox(agent.PodIP, &api.CreateSandboxRequest{
		Sandbox: api.SandboxSpec{
			SandboxID:  r.getSandboxID(sandbox),
			ClaimName:  sandbox.Name,
			Image:      sandbox.Spec.Image,
			Command:    sandbox.Spec.Command,
			Args:       sandbox.Spec.Args,
			Env:        envVarToMap(sandbox.Spec.Envs),
			WorkingDir: sandbox.Spec.WorkingDir,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create sandbox on agent %s: %w", agent.PodIP, err)
	}
	return nil
}

// deleteFromAgent sends a delete request to the Agent.
func (r *SandboxReconciler) deleteFromAgent(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	klog.Info("[DEBUG-DELETE-FROM-AGENT] ENTER",
		"sandbox", sandbox.Name,
		"assignedPod", sandbox.Status.AssignedPod)

	agent, ok := r.Registry.GetAgentByID(agentpool.AgentID(sandbox.Status.AssignedPod))
	if !ok {
		// Agent not found - nothing to delete
		klog.Warning("[DEBUG-DELETE-FROM-AGENT] Agent not found in registry",
			"agentID", agentpool.AgentID(sandbox.Status.AssignedPod))
		return nil
	}

	klog.Info("[DEBUG-DELETE-FROM-AGENT] Calling Agent DeleteSandbox API",
		"agentPodIP", agent.PodIP,
		"name", sandbox.Name,
		"sandboxID", r.getSandboxID(sandbox))

	_, err := r.AgentClient.DeleteSandbox(agent.PodIP, &api.DeleteSandboxRequest{
		SandboxID: r.getSandboxID(sandbox),
	})
	if err != nil {
		klog.Error("[DEBUG-DELETE-FROM-AGENT] DeleteSandbox API failed", "err", err)
		return fmt.Errorf("failed to delete sandbox from agent %s: %w", agent.PodIP, err)
	}

	klog.Info("[DEBUG-DELETE-FROM-AGENT] DeleteSandbox API called successfully")
	return nil
}

// syncStatusFromAgent synchronizes sandbox status from Agent's reported status.
func (r *SandboxReconciler) syncStatusFromAgent(ctx context.Context, sandbox *apiv1alpha1.Sandbox, agent *agentpool.AgentInfo) error {
	// Agent statuses are keyed by SandboxID (hash or UID), not by name
	status, hasStatus := agent.SandboxStatuses[r.getSandboxID(sandbox)]
	if !hasStatus {
		return nil
	}

	// Map Agent phase to Controller phase
	controllerPhase := mapAgentPhaseToController(status.Phase)

	// Skip status sync if sandbox has been reset (AcceptedResetRevision is set)
	// This prevents Agent-reported Terminated state from overwriting the reset state
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
		if len(latest.Spec.ExposedPorts) > 0 && agent.PodIP != "" {
			endpoints := make([]string, 0, len(latest.Spec.ExposedPorts))
			for _, port := range latest.Spec.ExposedPorts {
				endpoints = append(endpoints, fmt.Sprintf("%s:%d", agent.PodIP, port))
			}
			latest.Status.Endpoints = endpoints
		}

		return r.Status().Update(ctx, latest)
	})
}

// mapAgentPhaseToController maps Agent-reported phase to Controller standard phase.
// Agent uses lowercase (running, terminated), Controller uses TitleCase (Running, Terminated).
func mapAgentPhaseToController(agentPhase string) apiv1alpha1.SandboxPhase {
	switch apiv1alpha1.AgentSandboxPhase(agentPhase) {
	case apiv1alpha1.AgentPhaseRunning:
		return apiv1alpha1.PhaseRunning
	case apiv1alpha1.AgentPhaseCreating:
		return apiv1alpha1.PhaseBound // Still creating, keep as Bound
	case apiv1alpha1.AgentPhaseFailed:
		return apiv1alpha1.PhaseFailed
	case apiv1alpha1.AgentPhaseStopped:
		return apiv1alpha1.PhaseFailed // Stopped unexpectedly
	case apiv1alpha1.AgentPhaseTerminated:
		return apiv1alpha1.PhaseTerminating // Being deleted
	default:
		// Unknown phase - return as-is converted to SandboxPhase
		// This handles any future phases gracefully
		return apiv1alpha1.SandboxPhase(agentPhase)
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

		latest.Status.AssignedPod = allocInfo.AssignedPod
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
		"status.assignedPod",
		func(o client.Object) []string {
			return []string{o.(*apiv1alpha1.Sandbox).Status.AssignedPod}
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

// mapPodToSandboxes returns reconcile requests for unassigned sandboxes when an agent pod becomes ready.
func (r *SandboxReconciler) mapPodToSandboxes(ctx context.Context, obj client.Object) []ctrl.Request {
	pod := obj.(*corev1.Pod)

	// Only trigger for running agent pods
	if pod.Labels["app"] != "sandbox-agent" || pod.Status.Phase != corev1.PodRunning {
		return nil
	}

	// Request reconciliation for all unassigned sandboxes
	var sandboxList apiv1alpha1.SandboxList
	if err := r.List(ctx, &sandboxList, client.MatchingFields{"status.assignedPod": ""}); err != nil {
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
