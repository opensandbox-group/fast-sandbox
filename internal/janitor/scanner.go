package janitor

import (
	"context"
	"errors"
	"fmt"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

func (j *Janitor) Scan(ctx context.Context) {
	j.scanMu.Lock()
	defer j.scanMu.Unlock()
	for _, backend := range j.backends {
		resources, err := backend.Scan(ctx)
		if err != nil {
			klog.ErrorS(err, "Janitor backend scan failed", "backend", backend.Name())
			continue
		}
		for _, resource := range resources {
			decision, err := j.cleanupDecision(ctx, resource)
			if err != nil {
				klog.ErrorS(err, "Janitor authority check failed closed", "backend", resource.Backend, "resource", resource.ResourceID)
				continue
			}
			if !decision.Eligible {
				klog.V(2).InfoS("janitor scan skip; resource not eligible for cleanup", "backend", resource.Backend, "resource", resource.ResourceID, "reason", decision.Reason)
				continue
			}
			klog.InfoS("enqueuing orphan node resource", "backend", resource.Backend, "resource", resource.ResourceID, "reason", decision.Reason)
			j.queue.Add(CleanupTask{Resource: resource})
		}
	}
}

// cleanupDecision is shared by discovery and pre-delete revalidation. Any API
// or identity ambiguity fails closed.
func (j *Janitor) cleanupDecision(ctx context.Context, resource ResourceIdentity) (CleanupDecision, error) {
	if resource.Backend == "" || resource.ResourceID == "" || resource.FastletPodUID == "" {
		return CleanupDecision{}, errors.New("resource is missing backend, ID, or Fastlet Pod UID")
	}
	if resource.CreatedAt.IsZero() || j.now().Sub(resource.CreatedAt) < j.orphanTimeout() {
		return CleanupDecision{Reason: "OrphanGracePeriod"}, nil
	}
	podExists, err := j.exactFastletPodExists(ctx, resource)
	if err != nil {
		return CleanupDecision{}, err
	}
	if podExists {
		return CleanupDecision{Reason: "FastletPodStillExists"}, nil
	}
	if resource.SandboxUID == "" {
		return CleanupDecision{Eligible: true, Reason: "UnboundResourceFromLostPod"}, nil
	}

	sandbox, found, err := j.findSandbox(ctx, resource)
	if err != nil {
		return CleanupDecision{}, err
	}
	if !found || string(sandbox.UID) != resource.SandboxUID {
		return CleanupDecision{Eligible: true, Reason: "SandboxIdentityMissing"}, nil
	}
	placement := sandbox.Status.Placement
	if placement.FastletName == "" {
		return CleanupDecision{Eligible: true, Reason: "SandboxUnassigned"}, nil
	}
	if string(placement.FastletPodUID) != resource.FastletPodUID {
		return CleanupDecision{Eligible: true, Reason: "AssignmentMoved"}, nil
	}
	if resource.InstanceGeneration <= 0 || resource.AssignmentAttempt <= 0 {
		return CleanupDecision{Reason: "InvalidResourceFence"}, nil
	}
	currentGeneration := sandbox.Status.Runtime.Generation
	if currentGeneration < apiv1alpha2.InitialInstanceGeneration {
		currentGeneration = apiv1alpha2.InitialInstanceGeneration
	}
	if currentGeneration > resource.InstanceGeneration || placement.Attempt > resource.AssignmentAttempt {
		return CleanupDecision{Eligible: true, Reason: "ResourceFenceSuperseded"}, nil
	}
	if currentGeneration < resource.InstanceGeneration || placement.Attempt < resource.AssignmentAttempt {
		return CleanupDecision{Reason: "ResourceFenceAheadOfControlPlane"}, nil
	}
	if sandbox.Status.Runtime.State == apiv1alpha2.RuntimeUnavailable && sandbox.Status.Placement.Recovery != nil {
		return CleanupDecision{Eligible: true, Reason: "SandboxMarkedLost"}, nil
	}
	return CleanupDecision{Reason: "AssignmentStillAuthoritative"}, nil
}

func (j *Janitor) exactFastletPodExists(ctx context.Context, resource ResourceIdentity) (bool, error) {
	if j.kubeClient == nil {
		return false, errors.New("Kubernetes Pod client is not configured")
	}
	if resource.FastletPodName != "" && resource.FastletPodNamespace != "" {
		pod, err := j.kubeClient.CoreV1().Pods(resource.FastletPodNamespace).Get(ctx, resource.FastletPodName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return string(pod.UID) == resource.FastletPodUID && pod.DeletionTimestamp == nil, nil
	}
	pods, err := j.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if string(pod.UID) == resource.FastletPodUID && pod.DeletionTimestamp == nil {
			return true, nil
		}
	}
	return false, nil
}

func (j *Janitor) findSandbox(ctx context.Context, resource ResourceIdentity) (*apiv1alpha2.Sandbox, bool, error) {
	if j.K8sClient == nil {
		return nil, false, errors.New("Sandbox client is not configured")
	}
	if resource.SandboxName != "" && resource.SandboxNamespace != "" {
		var sandbox apiv1alpha2.Sandbox
		err := j.K8sClient.Get(ctx, types.NamespacedName{Namespace: resource.SandboxNamespace, Name: resource.SandboxName}, &sandbox)
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return &sandbox, true, nil
	}
	var list apiv1alpha2.SandboxList
	if err := j.K8sClient.List(ctx, &list); err != nil {
		return nil, false, err
	}
	for index := range list.Items {
		if string(list.Items[index].UID) == resource.SandboxUID {
			return list.Items[index].DeepCopy(), true, nil
		}
	}
	return nil, false, nil
}

func (resource ResourceIdentity) String() string {
	return fmt.Sprintf("%s/%s", resource.Backend, resource.ResourceID)
}
