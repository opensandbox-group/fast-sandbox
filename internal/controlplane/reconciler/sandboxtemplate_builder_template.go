package reconciler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The builder Pod template overlay is delivered as a ConfigMap mounted into
// the controller, mirroring the artifact-store pattern: the platform renders
// a fixed-name ConfigMap (fast-sandbox-builder-pod-template) in the
// controller's namespace, the chart mounts it at a fixed path, and every
// SandboxTemplate build Pod merges the whitelisted scheduling fields from it
// at creation time — so a ConfigMap edit applies to the next build without a
// controller restart. There is deliberately no flag: the name and path are
// conventions, and the content is structured YAML that does not belong on a
// command line.
const (
	builderPodTemplateDir      = "/etc/fast-sandbox/builder-pod-template"
	builderPodTemplateFileName = "podSpec.yaml"
)

// loadBuilderPodTemplateOverlay reads the optional overlay from dir. A
// missing directory or file means "no overlay configured"; any other read
// failure is an error so platform configuration mistakes surface instead of
// silently losing tolerations.
func loadBuilderPodTemplateOverlay(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, builderPodTemplateFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read builder pod template: %w", err)
	}
	return raw, nil
}

// applyBuilderPodTemplateOverlay merges the whitelisted scheduling fields of
// a raw YAML PodSpec fragment into spec. The build Pod is platform-owned:
// its security and placement shape (serviceAccountName, the KVM nodeSelector
// pin, the privileged builder container, volumes, deadlines) is enforced by
// the controller, so only scheduling relaxations are mergeable:
//
//   - tolerations: appended
//   - affinity: replaces when set
//   - topologySpreadConstraints: replaces when set
//
// Every other PodSpec field in the overlay is ignored, and unknown YAML
// fields fail parsing (strict decoding), so typos cannot silently no-op.
func applyBuilderPodTemplateOverlay(spec *corev1.PodSpec, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var overlay corev1.PodSpec
	if err := yaml.UnmarshalStrict(raw, &overlay); err != nil {
		return fmt.Errorf("parse builder pod template: %w", err)
	}
	spec.Tolerations = append(spec.Tolerations, overlay.Tolerations...)
	if overlay.Affinity != nil {
		spec.Affinity = overlay.Affinity
	}
	if len(overlay.TopologySpreadConstraints) > 0 {
		spec.TopologySpreadConstraints = overlay.TopologySpreadConstraints
	}
	return nil
}
