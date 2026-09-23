package hostready

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeNodeClient records every patch and applies it to the node store
// (the strategic merge semantics the reconciler relies on).
type fakeNodeClient struct {
	node          *corev1.Node
	labelPatches  [][]byte
	statusPatches [][]byte
}

func (f *fakeNodeClient) GetNode(context.Context, string) (*corev1.Node, error) {
	return f.node, nil
}

func (f *fakeNodeClient) PatchNode(_ context.Context, _ string, patch []byte) error {
	f.labelPatches = append(f.labelPatches, patch)
	var payload struct {
		Metadata struct {
			Labels map[string]*string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(patch, &payload); err != nil {
		return err
	}
	if f.node.Labels == nil {
		f.node.Labels = map[string]string{}
	}
	for key, value := range payload.Metadata.Labels {
		if value == nil {
			delete(f.node.Labels, key)
			continue
		}
		f.node.Labels[key] = *value
	}
	return nil
}

func (f *fakeNodeClient) PatchNodeStatus(_ context.Context, _ string, patch []byte) error {
	f.statusPatches = append(f.statusPatches, patch)
	var payload struct {
		Status struct {
			Conditions []corev1.NodeCondition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(patch, &payload); err != nil {
		return err
	}
	for _, incoming := range payload.Status.Conditions {
		replaced := false
		for i := range f.node.Status.Conditions {
			if f.node.Status.Conditions[i].Type == incoming.Type {
				f.node.Status.Conditions[i] = incoming
				replaced = true
				break
			}
		}
		if !replaced {
			f.node.Status.Conditions = append(f.node.Status.Conditions, incoming)
		}
	}
	return nil
}

func readyReport() Report {
	return Report{Ready: true, Summary: "9 checks: 8 pass, 1 warn, 0 fail", Checks: []Check{{Name: "kvm-device", Status: StatusPass}}}
}

func notReadyReport() Report {
	return Report{Ready: false, Summary: "9 checks: 6 pass, 1 warn, 2 fail", Checks: []Check{{Name: "kvm-device", Status: StatusFail}}}
}

func TestReconcilerLabelsAndConditionOnReady(t *testing.T) {
	client := &fakeNodeClient{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	reconciler := NewNodeReconciler(client, "node-1")
	if err := reconciler.Apply(context.Background(), readyReport(), "T2", ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(client.labelPatches) != 1 || len(client.statusPatches) != 1 {
		t.Fatalf("expected one label + one status patch, got %d/%d", len(client.labelPatches), len(client.statusPatches))
	}
	condition := nodeCondition(client.node, ConditionFirecrackerReady)
	if condition == nil || condition.Status != corev1.ConditionTrue || condition.Reason != "HostReady" {
		t.Fatalf("expected a True HostReady condition, got %+v", condition)
	}
}

func TestReconcilerIdempotentOnUnchangedState(t *testing.T) {
	client := &fakeNodeClient{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	reconciler := NewNodeReconciler(client, "node-1")
	report := readyReport()
	if err := reconciler.Apply(context.Background(), report, "T2", ""); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Apply(context.Background(), report, "T2", ""); err != nil {
		t.Fatal(err)
	}
	if len(client.labelPatches) != 1 || len(client.statusPatches) != 1 {
		t.Fatalf("the second apply must be a no-op, got %d/%d patches", len(client.labelPatches), len(client.statusPatches))
	}
}

func TestReconcilerRemovesLabelsOnDegradation(t *testing.T) {
	client := &fakeNodeClient{node: &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
			LabelKVM: "true", LabelFirecrackerNode: "true", "other.example.com/keep": "yes",
		}},
	}}
	client.node.Status.Conditions = []corev1.NodeCondition{{
		Type: ConditionFirecrackerReady, Status: corev1.ConditionTrue, Reason: "HostReady",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	reconciler := NewNodeReconciler(client, "node-1")
	if err := reconciler.Apply(context.Background(), notReadyReport(), "T2", ""); err != nil {
		t.Fatal(err)
	}
	if len(client.labelPatches) != 1 {
		t.Fatalf("expected a label removal patch, got %d", len(client.labelPatches))
	}
	if patch := string(client.labelPatches[0]); !containsAll(patch, `"sandbox.fast.io/kvm":null`, `"fast-sandbox.io/firecracker-node":null`) {
		t.Fatalf("the removal patch must null the managed labels only: %s", patch)
	}
	if len(client.statusPatches) != 1 {
		t.Fatalf("expected a status patch, got %d", len(client.statusPatches))
	}
	if patch := string(client.statusPatches[0]); !containsAll(patch, `"status":"False"`, `"reason":"HostNotReady"`) {
		t.Fatalf("the condition must flip to False/HostNotReady: %s", patch)
	}
	// True -> False is a status change, so the transition time legitimately
	// advances; only a same-status refresh preserves it (asserted below in
	// TestConditionPatchPreservesTransitionTime). Assert the condition
	// state instead.
	condition := nodeCondition(client.node, ConditionFirecrackerReady)
	if condition == nil || condition.Status != corev1.ConditionFalse || condition.Reason != "HostNotReady" {
		t.Fatalf("expected a False HostNotReady condition, got %+v", condition)
	}
	if _, still := client.node.Labels[LabelKVM]; still {
		t.Fatalf("the kvm label must be removed: %v", client.node.Labels)
	}
	if _, still := client.node.Labels[LabelFirecrackerNode]; still {
		t.Fatalf("the firecracker-node label must be removed: %v", client.node.Labels)
	}
	if _, keep := client.node.Labels["other.example.com/keep"]; !keep {
		t.Fatalf("foreign labels must survive: %v", client.node.Labels)
	}
}

func TestConditionPatchPreservesTransitionTime(t *testing.T) {
	transition := metav1.NewTime(time.Now().Add(-time.Hour))
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
		Type: ConditionFirecrackerReady, Status: corev1.ConditionTrue, Reason: "HostReady",
		Message: "old summary", LastTransitionTime: transition,
	}}}}
	patch, ok := conditionPatch(node, readyReport(), time.Now())
	if !ok {
		t.Fatal("a message drift must produce a patch")
	}
	if string(patch) == "" || !containsAll(string(patch), transition.Format(time.RFC3339)) {
		t.Fatalf("the transition time must be preserved: %s", patch)
	}
}

// TestReconcilerCPUCompatibilityLabel: the CPU template tier rides the
// scheduling labels — set when ready, nulled on degradation and when the
// tier is unknown (empty).
func TestReconcilerCPUCompatibilityLabel(t *testing.T) {
	client := &fakeNodeClient{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	reconciler := NewNodeReconciler(client, "node-1")

	if err := reconciler.Apply(context.Background(), readyReport(), "none", "AuthenticAMD-26-17"); err != nil {
		t.Fatal(err)
	}
	if got := client.node.Labels[LabelCPUTemplate]; got != "none" {
		t.Fatalf("cpu-template label = %q, want none", got)
	}
	if got := client.node.Labels[LabelCPUIdentity]; got != "AuthenticAMD-26-17" {
		t.Fatalf("cpu-identity label = %q, want AuthenticAMD-26-17", got)
	}

	if err := reconciler.Apply(context.Background(), notReadyReport(), "none", "AuthenticAMD-26-17"); err != nil {
		t.Fatal(err)
	}
	if patch := string(client.labelPatches[len(client.labelPatches)-1]); !containsAll(patch, `"sandbox.fast.io/cpu-template":null`, `"sandbox.fast.io/cpu-identity":null`) {
		t.Fatalf("the degraded patch must null the cpu labels: %s", patch)
	}

	// A stale tier on an already-ready node (e.g. the binary version became
	// unreadable after an agent restart) is nulled on the next pass.
	stale := &fakeNodeClient{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
		LabelKVM: "true", LabelFirecrackerNode: "true", LabelCPUTemplate: "T2A",
	}}}}
	staleReconciler := NewNodeReconciler(stale, "node-1")
	if err := staleReconciler.Apply(context.Background(), readyReport(), "", ""); err != nil {
		t.Fatal(err)
	}
	if patch := string(stale.labelPatches[len(stale.labelPatches)-1]); !containsAll(patch, `"sandbox.fast.io/cpu-template":null`) {
		t.Fatalf("an unknown tier must null a stale cpu-template label: %s", patch)
	}

	// A template-tier node loses a stale identity label: the allowlist, not
	// the identity, decides what it restores.
	template := &fakeNodeClient{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
		LabelKVM: "true", LabelFirecrackerNode: "true", LabelCPUTemplate: "T2A", LabelCPUIdentity: "AuthenticAMD-25-1",
	}}}}
	templateReconciler := NewNodeReconciler(template, "node-1")
	if err := templateReconciler.Apply(context.Background(), readyReport(), "T2A", ""); err != nil {
		t.Fatal(err)
	}
	if patch := string(template.labelPatches[len(template.labelPatches)-1]); !containsAll(patch, `"sandbox.fast.io/cpu-identity":null`) {
		t.Fatalf("a template-tier node must null a stale cpu-identity label: %s", patch)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}

// nodeCondition finds one condition by type.
func nodeCondition(node *corev1.Node, conditionType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == conditionType {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}
