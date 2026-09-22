package reconciler

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestApplyBuilderPodTemplateOverlayEmpty(t *testing.T) {
	spec := corev1.PodSpec{NodeSelector: map[string]string{"kvm": "true"}}
	if err := applyBuilderPodTemplateOverlay(&spec, nil); err != nil {
		t.Fatalf("nil overlay: %v", err)
	}
	if err := applyBuilderPodTemplateOverlay(&spec, []byte("")); err != nil {
		t.Fatalf("empty overlay: %v", err)
	}
	if len(spec.Tolerations) != 0 || spec.Affinity != nil {
		t.Fatalf("expected untouched spec, got %+v", spec)
	}
}

func TestApplyBuilderPodTemplateOverlayAppendsTolerations(t *testing.T) {
	spec := corev1.PodSpec{Tolerations: []corev1.Toleration{{
		Key:      "node-role.kubernetes.io/control-plane",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}}
	raw := []byte(`tolerations:
- effect: NoSchedule
  key: sigma.ali/resource-pool
  value: sigma_public
- effect: NoSchedule
  key: sigma.ali/is-ecs
  operator: Exists
`)
	if err := applyBuilderPodTemplateOverlay(&spec, raw); err != nil {
		t.Fatalf("apply overlay: %v", err)
	}
	if len(spec.Tolerations) != 3 {
		t.Fatalf("expected existing toleration preserved plus two appended, got %+v", spec.Tolerations)
	}
	if spec.Tolerations[0].Key != "node-role.kubernetes.io/control-plane" {
		t.Fatalf("expected the pre-existing toleration first, got %+v", spec.Tolerations[0])
	}
	if spec.Tolerations[1].Key != "sigma.ali/resource-pool" || spec.Tolerations[1].Value != "sigma_public" {
		t.Fatalf("unexpected appended toleration: %+v", spec.Tolerations[1])
	}
}

func TestApplyBuilderPodTemplateOverlayReplacesSchedulingFields(t *testing.T) {
	spec := corev1.PodSpec{}
	raw := []byte(`affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
      - matchExpressions:
        - key: fast-sandbox.io/firecracker-node
          operator: Exists
topologySpreadConstraints:
- maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
  labelSelector:
    matchLabels:
      app: build
`)
	if err := applyBuilderPodTemplateOverlay(&spec, raw); err != nil {
		t.Fatalf("apply overlay: %v", err)
	}
	if spec.Affinity == nil || len(spec.TopologySpreadConstraints) != 1 {
		t.Fatalf("expected affinity and spread constraints applied, got %+v", spec)
	}
}

func TestApplyBuilderPodTemplateOverlayIgnoresPlatformOwnedFields(t *testing.T) {
	spec := corev1.PodSpec{
		ServiceAccountName: "fast-sandbox-sandboxtemplate-builder",
		NodeSelector:       map[string]string{"fast-sandbox.io/kvm": "true"},
	}
	raw := []byte(`serviceAccountName: attacker-sa
nodeSelector:
  fast-sandbox.io/kvm: "false"
restartPolicy: Always
activeDeadlineSeconds: 999999
automountServiceAccountToken: true
`)
	if err := applyBuilderPodTemplateOverlay(&spec, raw); err != nil {
		t.Fatalf("apply overlay: %v", err)
	}
	if spec.ServiceAccountName != "fast-sandbox-sandboxtemplate-builder" {
		t.Fatalf("serviceAccountName must stay platform-owned, got %q", spec.ServiceAccountName)
	}
	if spec.NodeSelector["fast-sandbox.io/kvm"] != "true" {
		t.Fatalf("nodeSelector must stay platform-owned, got %+v", spec.NodeSelector)
	}
	if spec.RestartPolicy != "" || spec.ActiveDeadlineSeconds != nil || spec.AutomountServiceAccountToken != nil {
		t.Fatalf("platform-owned fields must be ignored, got %+v", spec)
	}
}

func TestApplyBuilderPodTemplateOverlayRejectsUnknownFields(t *testing.T) {
	spec := corev1.PodSpec{}
	err := applyBuilderPodTemplateOverlay(&spec, []byte("tolerationss:\n- key: typo\n"))
	if err == nil {
		t.Fatal("expected strict parsing to reject an unknown field")
	}
}

func TestLoadBuilderPodTemplateOverlayMissing(t *testing.T) {
	raw, err := loadBuilderPodTemplateOverlay(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("missing dir must mean no overlay, got %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil overlay, got %q", raw)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, builderPodTemplateFileName), []byte("tolerations: []\n"), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	raw, err = loadBuilderPodTemplateOverlay(dir)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	if raw == nil || string(raw) != "tolerations: []\n" {
		t.Fatalf("unexpected overlay content %q", raw)
	}
}
