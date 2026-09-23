package hostready

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// The scheduling labels the reconciler manages. They already existed in
// the cluster semantics (the SandboxTemplate builder selects
// sandbox.fast.io/kvm, the fastlet pods and pools select
// fast-sandbox.io/firecracker-node); the agent turns them from
// manually-applied static labels into readiness-driven ones.
const (
	// LabelKVM marks the node as able to run Firecracker workloads (the
	// SandboxTemplate builder schedules on it).
	LabelKVM = "sandbox.fast.io/kvm"
	// LabelFirecrackerNode marks the node as ready for firecracker
	// fastlets (pools nodeSelector onto it).
	LabelFirecrackerNode = "fast-sandbox.io/firecracker-node"
	// LabelCPUTemplate marks which snapshot CPU template tier the node
	// restores: "T2", "T2A", or "none" (identity-matched unmasked
	// snapshots only). See docs/guides/snapshot-cpu-compatibility.md.
	LabelCPUTemplate = "sandbox.fast.io/cpu-template"
	// LabelCPUIdentity carries the vendor-family-model identity of a
	// "none"-tier node — the scheduling key for unmasked snapshots. Set
	// only alongside LabelCPUTemplate: "none".
	LabelCPUIdentity = "sandbox.fast.io/cpu-identity"
	// ConditionFirecrackerReady is the Node condition reporting the last
	// check pass (True when ready, False with the failing summary).
	ConditionFirecrackerReady = "FirecrackerReady"
)

// NodeClient abstracts the node API surface the reconciler needs (tests
// fake it; production wraps client-go).
type NodeClient interface {
	GetNode(ctx context.Context, name string) (*corev1.Node, error)
	PatchNode(ctx context.Context, name string, patch []byte) error
	PatchNodeStatus(ctx context.Context, name string, patch []byte) error
}

// ClientsetNodeClient adapts a client-go clientset.
type ClientsetNodeClient struct {
	Clientset kubernetes.Interface
}

// GetNode fetches one node.
func (c ClientsetNodeClient) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	return c.Clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

// PatchNode applies a strategic merge patch to the node metadata.
func (c ClientsetNodeClient) PatchNode(ctx context.Context, name string, patch []byte) error {
	_, err := c.Clientset.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// PatchNodeStatus applies a strategic merge patch to the node status
// subresource (conditions live there).
func (c ClientsetNodeClient) PatchNodeStatus(ctx context.Context, name string, patch []byte) error {
	_, err := c.Clientset.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

// NodeReconciler converges one node's labels and condition onto the
// readiness report. Patches are computed against the observed node, so an
// unchanged pass is a no-op (the 5-minute recheck does not churn the API).
type NodeReconciler struct {
	client   NodeClient
	nodeName string
}

// NewNodeReconciler builds the reconciler for one node.
func NewNodeReconciler(client NodeClient, nodeName string) *NodeReconciler {
	return &NodeReconciler{client: client, nodeName: nodeName}
}

// Apply converges labels + condition onto the report outcome. cpuTemplate
// is the node's compatibility tier ("T2"/"T2A"/"none") for the
// LabelCPUTemplate scheduling label; empty removes the label. cpuIdentity
// (vendor-family-model) accompanies it on "none" nodes.
func (r *NodeReconciler) Apply(ctx context.Context, report Report, cpuTemplate, cpuIdentity string) error {
	node, err := r.client.GetNode(ctx, r.nodeName)
	if err != nil {
		return fmt.Errorf("get node %s: %w", r.nodeName, err)
	}
	if patch := labelPatch(node, report.Ready, cpuTemplate, cpuIdentity); patch != nil {
		if err := r.client.PatchNode(ctx, r.nodeName, patch); err != nil {
			return fmt.Errorf("patch node %s labels: %w", r.nodeName, err)
		}
	}
	if patch, ok := conditionPatch(node, report, time.Now()); ok {
		if err := r.client.PatchNodeStatus(ctx, r.nodeName, patch); err != nil {
			return fmt.Errorf("patch node %s status: %w", r.nodeName, err)
		}
	}
	return nil
}

// labelPatch returns the metadata patch moving the labels to the desired
// state, or nil when they already match. Removal uses a null value (the
// strategic merge delete form).
func labelPatch(node *corev1.Node, ready bool, cpuTemplate, cpuIdentity string) []byte {
	desired := map[string]string{
		LabelKVM:             "",
		LabelFirecrackerNode: "",
		LabelCPUTemplate:     "",
		LabelCPUIdentity:     "",
	}
	if ready {
		desired[LabelKVM] = "true"
		desired[LabelFirecrackerNode] = "true"
		if cpuTemplate != "" {
			desired[LabelCPUTemplate] = cpuTemplate
		}
		if cpuTemplate == "none" && cpuIdentity != "" {
			desired[LabelCPUIdentity] = cpuIdentity
		}
	}
	labels := map[string]interface{}{}
	changed := false
	for key, value := range desired {
		current, exists := node.Labels[key]
		if value == "" {
			// Removal: only when the label is present.
			if exists {
				labels[key] = nil
				changed = true
			}
			continue
		}
		if !exists || current != value {
			labels[key] = value
			changed = true
		}
	}
	if !changed {
		return nil
	}
	payload, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": labels}})
	if err != nil {
		return nil
	}
	return payload
}

// conditionPatch returns the status patch for the FirecrackerReady
// condition, or nil when the node already reflects the report. The
// transition time only advances when the status flips; the heartbeat and
// message refresh whenever the summary drifts.
func conditionPatch(node *corev1.Node, report Report, now time.Time) ([]byte, bool) {
	desiredStatus := corev1.ConditionFalse
	reason := "HostNotReady"
	if report.Ready {
		desiredStatus = corev1.ConditionTrue
		reason = "HostReady"
	}
	var existing *corev1.NodeCondition
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == ConditionFirecrackerReady {
			existing = &node.Status.Conditions[i]
			break
		}
	}
	if existing != nil &&
		existing.Status == desiredStatus &&
		existing.Reason == reason &&
		existing.Message == report.Summary {
		return nil, false
	}
	transition := metav1.NewTime(now)
	if existing != nil && existing.Status == desiredStatus && !existing.LastTransitionTime.IsZero() {
		transition = existing.LastTransitionTime
	}
	condition := map[string]interface{}{
		"type":               ConditionFirecrackerReady,
		"status":             string(desiredStatus),
		"reason":             reason,
		"message":            report.Summary,
		"lastHeartbeatTime":  metav1.NewTime(now).Format(time.RFC3339),
		"lastTransitionTime": transition.Format(time.RFC3339),
	}
	payload, err := json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{"conditions": []interface{}{condition}},
	})
	if err != nil {
		return nil, false
	}
	return payload, true
}
