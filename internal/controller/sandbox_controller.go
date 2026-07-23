package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/sandboxorchestrator"
	"fast-sandbox/internal/observability"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	FinalizerName          = "sandbox.fast.io/cleanup"
	DefaultRequeueInterval = 5 * time.Second
	DeletionPollInterval   = time.Second
)

type SandboxReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Orchestrator *sandboxorchestrator.Orchestrator
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, request ctrl.Request) (_ ctrl.Result, resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: request.Namespace, SandboxName: request.Name})
	ctx, span := observability.Start(ctx, "controller.reconcile Sandbox")
	defer func() { observability.End(span, resultErr) }()
	var sandbox apiv1alpha1.Sandbox
	if err := r.Get(ctx, request.NamespacedName, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = observability.WithIdentity(ctx, sandboxObservabilityIdentity(&sandbox))
	if r.Orchestrator == nil {
		return ctrl.Result{}, errors.New("Sandbox orchestrator is not configured")
	}
	orchestrator := r.Orchestrator

	if sandbox.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, orchestrator, &sandbox)
	}
	if !controllerutil.ContainsFinalizer(&sandbox, FinalizerName) {
		controllerutil.AddFinalizer(&sandbox, FinalizerName)
		if err := r.Update(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if sandbox.Status.HasCondition(sandboxorchestrator.ConditionRuntimeReady, metav1.ConditionFalse, sandboxorchestrator.ReasonExpired) {
		return ctrl.Result{}, nil
	}
	if sandbox.Spec.ExpireTime != nil && !time.Now().Before(sandbox.Spec.ExpireTime.Time) {
		return r.reconcileExpiration(ctx, orchestrator, &sandbox)
	}
	if resetPending(&sandbox) {
		return r.reconcileReset(ctx, orchestrator, &sandbox)
	}
	if sandbox.Status.HasCondition(sandboxorchestrator.ConditionRuntimeReady, metav1.ConditionFalse, sandboxorchestrator.ReasonFastletPodLost) && sandbox.Spec.FailurePolicy != apiv1alpha1.FailurePolicyAutoRecreate {
		return ctrl.Result{}, nil
	}
	return r.reconcileEnsure(ctx, orchestrator, &sandbox)
}

func sandboxObservabilityIdentity(sandbox *apiv1alpha1.Sandbox) observability.Identity {
	identity := observability.Identity{
		RequestID: sandbox.Annotations[common.AnnotationRequestID], Namespace: sandbox.Namespace, SandboxName: sandbox.Name,
		SandboxUID: string(sandbox.UID), InstanceGeneration: sandbox.Status.InstanceGeneration, RouteGeneration: sandbox.Status.RouteGeneration,
	}
	if sandbox.Status.Assignment != nil {
		identity.FastletPodUID = sandbox.Status.Assignment.FastletPodUID
		identity.AssignmentAttempt = sandbox.Status.Assignment.Attempt
	}
	return identity
}

func (r *SandboxReconciler) reconcileEnsure(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	if sandbox.Status.Assignment != nil {
		lost, err := r.assignedPodLost(ctx, sandbox)
		if err != nil {
			return ctrl.Result{}, err
		}
		if lost {
			return r.reconcilePodLost(ctx, orchestrator, sandbox)
		}
	}
	assigned, newlyAssigned, err := orchestrator.AssignDeclarative(ctx, sandbox, string(sandbox.UID))
	if err != nil {
		if errors.Is(err, sandboxorchestrator.ErrNoCandidate) {
			_ = orchestrator.MarkPending(ctx, sandbox, "NoCandidate", "No Ready Fastlet currently accepts this Pool/profile")
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		if errors.Is(err, sandboxorchestrator.ErrAssignedFastletUnavailable) {
			if statusErr := r.markAssignedFastletUnavailable(ctx, sandbox); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if err := orchestrator.ReconcileRuntime(ctx, assigned); err != nil {
		if errors.Is(err, sandboxorchestrator.ErrRuntimeInProgress) || errors.Is(err, sandboxorchestrator.ErrUnknownFastletOutcome) {
			return ctrl.Result{RequeueAfter: DeletionPollInterval}, nil
		}
		if errors.Is(err, sandboxorchestrator.ErrAssignedFastletUnavailable) {
			if statusErr := r.markAssignedFastletUnavailable(ctx, assigned); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		if explicitReschedule(err) {
			// A CRD-first assignment is a durable creation fact. Move directly
			// from the rejected identity to another eligible candidate with one
			// CAS; if none exists, preserve the current identity until capacity
			// or a new Fastlet becomes visible.
			_, moved, reassignErr := orchestrator.ReassignDeclarativeAfterRejection(ctx, assigned, string(assigned.UID))
			if reassignErr != nil {
				return ctrl.Result{}, reassignErr
			}
			if moved {
				// Status still projects the previous annotation here. Requeue
				// immediately so the normal annotation-to-status projection runs
				// before the new identity is sent to Fastlet.
				return ctrl.Result{Requeue: true}, nil
			}
			_ = orchestrator.MarkPending(ctx, assigned, "FastletRejected", err.Error())
			return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, err
	}
	if newlyAssigned {
		klog.FromContext(ctx).Info("Sandbox assigned and runtime ensured", "sandbox", sandbox.Name, "fastlet", assigned.Status.Assignment.FastletName)
	}
	return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

func (r *SandboxReconciler) assignedPodLost(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (bool, error) {
	if sandbox == nil || sandbox.Status.Assignment == nil {
		return false, nil
	}
	assignment := sandbox.Status.Assignment
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: assignment.FastletName}, &pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return string(pod.UID) != assignment.FastletPodUID || pod.DeletionTimestamp != nil, nil
}

func (r *SandboxReconciler) markAssignedFastletUnavailable(ctx context.Context, sandbox *apiv1alpha1.Sandbox) error {
	return r.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateUnavailable
		status.DataPlaneState = apiv1alpha1.ObservedStateUnavailable
		setSandboxCondition(status, sandboxorchestrator.ConditionRuntimeReady, metav1.ConditionFalse, "FastletRegistryPending", "The assigned Fastlet Pod still exists, but its local registry endpoint is temporarily unavailable")
	})
}

func (r *SandboxReconciler) reconcilePodLost(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	if sandbox.Spec.FailurePolicy == apiv1alpha1.FailurePolicyAutoRecreate {
		if sandbox.Status.Assignment != nil {
			if _, err := orchestrator.ClearAssignment(ctx, sandbox, true); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true}, nil
	}
	err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateUnavailable
		status.DataPlaneState = apiv1alpha1.ObservedStateUnavailable
		setSandboxCondition(status, sandboxorchestrator.ConditionRuntimeReady, metav1.ConditionFalse, sandboxorchestrator.ReasonFastletPodLost, "The assigned Fastlet Pod no longer exists")
		setSandboxCondition(status, sandboxorchestrator.ConditionDataPlaneReady, metav1.ConditionFalse, sandboxorchestrator.ReasonFastletPodLost, "The assigned Fastlet Pod no longer exists")
	})
	return ctrl.Result{}, err
}

func (r *SandboxReconciler) reconcileDeletion(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, FinalizerName) {
		return ctrl.Result{}, nil
	}
	done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
	if err != nil {
		return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
	}
	if !done {
		_ = r.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
			status.RuntimeState = apiv1alpha1.ObservedStateDraining
			status.DataPlaneState = apiv1alpha1.ObservedStateDraining
		})
		return ctrl.Result{RequeueAfter: DeletionPollInterval}, nil
	}
	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha1.Sandbox
		if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		controllerutil.RemoveFinalizer(&current, FinalizerName)
		return r.Update(ctx, &current)
	})
}

func (r *SandboxReconciler) reconcileExpiration(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
	}
	if sandbox.Status.Assignment != nil {
		cleared, err := orchestrator.ClearAssignment(ctx, sandbox, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		sandbox = cleared
	}
	err = r.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		status.RuntimeState = apiv1alpha1.ObservedStateStopped
		status.DataPlaneState = apiv1alpha1.ObservedStateStopped
		setSandboxCondition(status, sandboxorchestrator.ConditionRuntimeReady, metav1.ConditionFalse, sandboxorchestrator.ReasonExpired, "Sandbox desired lifetime expired")
		setSandboxCondition(status, sandboxorchestrator.ConditionDataPlaneReady, metav1.ConditionFalse, sandboxorchestrator.ReasonExpired, "Sandbox desired lifetime expired")
	})
	return ctrl.Result{}, err
}

func (r *SandboxReconciler) reconcileReset(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
	if sandbox.Status.Assignment != nil {
		done, err := r.ensureRuntimeDeleted(ctx, orchestrator, sandbox)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: DeletionPollInterval}, err
		}
		cleared, err := orchestrator.ClearAssignment(ctx, sandbox, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		sandbox = cleared
	}
	if err := r.patchStatus(ctx, sandbox, func(status *apiv1alpha1.SandboxStatus) {
		if status.InstanceGeneration < apiv1alpha1.InitialInstanceGeneration {
			status.InstanceGeneration = apiv1alpha1.InitialInstanceGeneration
		}
		status.AcceptedResetRevision = sandbox.Spec.ResetRevision.DeepCopy()
		status.RuntimeState = apiv1alpha1.ObservedStatePending
		status.DataPlaneState = apiv1alpha1.ObservedStatePending
		setSandboxCondition(status, sandboxorchestrator.ConditionRuntimeReady, metav1.ConditionFalse, "ResetRequested", "Sandbox reset is pending")
		setSandboxCondition(status, sandboxorchestrator.ConditionDataPlaneReady, metav1.ConditionFalse, "ResetRequested", "Sandbox reset is pending")
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SandboxReconciler) ensureRuntimeDeleted(ctx context.Context, orchestrator *sandboxorchestrator.Orchestrator, sandbox *apiv1alpha1.Sandbox) (bool, error) {
	if sandbox.Status.Assignment == nil {
		return true, nil
	}
	gone, inspectErr := orchestrator.RuntimeGone(ctx, sandbox)
	if gone {
		return true, nil
	}
	if inspectErr != nil {
		if errors.Is(inspectErr, sandboxorchestrator.ErrAssignedFastletUnavailable) {
			lost, podErr := r.assignedPodLost(ctx, sandbox)
			if podErr != nil {
				return false, podErr
			}
			if lost {
				return true, nil
			}
			return false, inspectErr
		}
		if sandboxorchestrator.IsNotFound(inspectErr) {
			// Pod-bound model: once the Fastlet Pod identity is gone, all of its
			// Sandbox runtimes are considered gone and cannot be taken over.
			return true, nil
		}
		return false, inspectErr
	}
	if err := orchestrator.DeleteRuntime(ctx, sandbox); err != nil && !sandboxorchestrator.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

func resetPending(sandbox *apiv1alpha1.Sandbox) bool {
	if sandbox.Spec.ResetRevision == nil {
		return false
	}
	return sandbox.Status.AcceptedResetRevision == nil || sandbox.Spec.ResetRevision.After(sandbox.Status.AcceptedResetRevision.Time)
}

func explicitReschedule(err error) bool {
	var failure *api.FastletError
	if !errors.As(err, &failure) || failure.Outcome != api.OutcomeRejectedBeforeSideEffects {
		return false
	}
	switch failure.Code {
	case api.ErrorCapacityRejected, api.ErrorDraining, api.ErrorRuntimeUnavailable, api.ErrorNetworkUnavailable, api.ErrorInfraUnavailable:
		return true
	default:
		return false
	}
}

func (r *SandboxReconciler) patchStatus(ctx context.Context, sandbox *apiv1alpha1.Sandbox, mutate func(*apiv1alpha1.SandboxStatus)) error {
	key := client.ObjectKeyFromObject(sandbox)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current apiv1alpha1.Sandbox
		if err := r.Get(ctx, key, &current); err != nil {
			return err
		}
		before := current.DeepCopy().Status
		mutate(&current.Status)
		if reflect.DeepEqual(before, current.Status) {
			return nil
		}
		return r.Status().Update(ctx, &current)
	})
}

func setSandboxCondition(status *apiv1alpha1.SandboxStatus, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
	})
}

func (r *SandboxReconciler) SetupWithManager(manager ctrl.Manager) error {
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &apiv1alpha1.Sandbox{}, "status.assignment.fastletName", func(object client.Object) []string {
		sandbox := object.(*apiv1alpha1.Sandbox)
		if sandbox.Status.Assignment == nil {
			return nil
		}
		return []string{sandbox.Status.Assignment.FastletName}
	}); err != nil {
		return fmt.Errorf("index Sandbox assignment: %w", err)
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&apiv1alpha1.Sandbox{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPodToSandboxes)).
		Complete(r)
}

func (r *SandboxReconciler) mapPodToSandboxes(ctx context.Context, object client.Object) []ctrl.Request {
	if object.GetLabels()["app"] != "sandbox-fastlet" {
		return nil
	}
	var list apiv1alpha1.SandboxList
	if err := r.List(ctx, &list, client.InNamespace(object.GetNamespace()), client.MatchingFields{"status.assignment.fastletName": object.GetName()}); err != nil {
		return nil
	}
	result := make([]ctrl.Request, 0, len(list.Items))
	for index := range list.Items {
		result = append(result, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[index].Namespace, Name: list.Items[index].Name}})
	}
	return result
}
