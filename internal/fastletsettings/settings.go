// Package fastletsettings defines the Fastlet settings ConfigMap contract:
// pool-declared declarative configuration (warm images, action handlers, and
// the sandbox resource profile) is projected into an immutable,
// content-addressed ConfigMap and mounted into the Fastlet Pod as files,
// instead of travelling as environment variables.
package fastletsettings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

const (
	// MountPath is where the settings ConfigMap is mounted in the Fastlet container.
	MountPath = "/etc/fast-sandbox/settings"
	// EnvDir names the environment variable that points the Fastlet at MountPath.
	EnvDir = "FAST_SANDBOX_SETTINGS_DIR"
	// VolumeName is the platform-owned Pod volume carrying the settings ConfigMap.
	VolumeName = "fastlet-settings"
	// WarmImagesKey is the ConfigMap key holding the JSON warm image list.
	WarmImagesKey = "warm-images.json"
	// ActionHandlersKey is the ConfigMap key holding the JSON action handler list.
	ActionHandlersKey = "action-handlers.json"
	// ResourceProfileKey is the ConfigMap key holding the JSON sandbox resource profile.
	ResourceProfileKey = "resource-profile.json"
	// ResourceAdmissionKey is the optional ResourceAware commitment-capacity contract.
	ResourceAdmissionKey = "resource-admission.json"
)

// ResourceAdmissionSettings delivers the final runtime-owner Pod request and
// limit to Fastlet. Kubernetes requests cannot be discovered reliably from the
// cgroup hierarchy, so the controller projects the post-defaulting values.
type ResourceAdmissionSettings struct {
	Version             string                              `json:"version"`
	Mode                apiv1alpha2.SandboxResourceMode     `json:"mode"`
	Capacity            apiv1alpha2.SandboxComputeResources `json:"capacity"`
	RuntimeLimit        apiv1alpha2.SandboxComputeResources `json:"runtimeLimit"`
	ResourceProfileHash string                              `json:"resourceProfileHash"`
}

// Validate verifies that projected admission settings match the immutable
// Pool profile and can admit at least one Sandbox without exceeding the parent
// runtime limit.
func (s *ResourceAdmissionSettings) Validate(profile apiv1alpha2.SandboxResourceProfile) error {
	if s == nil {
		return fmt.Errorf("resource admission settings are required")
	}
	if s.Version != "v1" || s.Mode != apiv1alpha2.SandboxResourceModeResourceAware {
		return fmt.Errorf("unsupported resource admission settings version %q mode %q", s.Version, s.Mode)
	}
	if profile.ResourceMode() != apiv1alpha2.SandboxResourceModeResourceAware || s.ResourceProfileHash != profile.Hash() {
		return fmt.Errorf("resource admission settings do not match the Sandbox resource profile")
	}
	requests, _ := profile.RequestResources()
	limits, _ := profile.LimitResources()
	if s.Capacity.CPU.Sign() <= 0 || s.Capacity.Memory.Sign() <= 0 ||
		s.RuntimeLimit.CPU.Sign() <= 0 || s.RuntimeLimit.Memory.Sign() <= 0 {
		return fmt.Errorf("resource admission capacity and runtime limit must be greater than zero")
	}
	if s.Capacity.CPU.Cmp(s.RuntimeLimit.CPU) > 0 || s.Capacity.Memory.Cmp(s.RuntimeLimit.Memory) > 0 {
		return fmt.Errorf("resource admission capacity must not exceed the runtime limit")
	}
	if s.Capacity.CPU.Cmp(requests.CPU) < 0 || s.Capacity.Memory.Cmp(requests.Memory) < 0 {
		return fmt.Errorf("resource admission capacity cannot commit one Sandbox request")
	}
	if s.RuntimeLimit.CPU.Cmp(limits.CPU) < 0 || s.RuntimeLimit.Memory.Cmp(limits.Memory) < 0 {
		return fmt.Errorf("resource admission runtime limit cannot contain one Sandbox limit")
	}
	return nil
}

// Bundle is the pool-declared declarative configuration projected into the
// settings ConfigMap. It is the Fastlet-facing projection of SandboxPool
// fields that describe desired sandbox behavior rather than pod identity.
type Bundle struct {
	WarmImages        []string                           `json:"warmImages"`
	ActionHandlers    []apiv1alpha2.ActionHandler        `json:"actionHandlers,omitempty"`
	ResourceProfile   apiv1alpha2.SandboxResourceProfile `json:"resourceProfile"`
	ResourceAdmission *ResourceAdmissionSettings         `json:"resourceAdmission,omitempty"`
}

// Files renders the bundle into ConfigMap data keys, one JSON document per
// setting so each section stays independently readable.
func (b Bundle) Files() (map[string]string, error) {
	warmImages := b.WarmImages
	if warmImages == nil {
		warmImages = []string{}
	}
	actionHandlers := b.ActionHandlers
	if actionHandlers == nil {
		actionHandlers = []apiv1alpha2.ActionHandler{}
	}
	warmImagesJSON, err := json.Marshal(warmImages)
	if err != nil {
		return nil, fmt.Errorf("encode warm images: %w", err)
	}
	actionHandlersJSON, err := json.Marshal(actionHandlers)
	if err != nil {
		return nil, fmt.Errorf("encode action handlers: %w", err)
	}
	resourceProfile, err := json.Marshal(b.ResourceProfile)
	if err != nil {
		return nil, fmt.Errorf("encode resource profile: %w", err)
	}
	files := map[string]string{
		WarmImagesKey:      string(warmImagesJSON),
		ActionHandlersKey:  string(actionHandlersJSON),
		ResourceProfileKey: string(resourceProfile),
	}
	if b.ResourceAdmission != nil {
		resourceAdmission, err := json.Marshal(b.ResourceAdmission)
		if err != nil {
			return nil, fmt.Errorf("encode resource admission settings: %w", err)
		}
		files[ResourceAdmissionKey] = string(resourceAdmission)
	}
	return files, nil
}

// Revision returns the content-addressed revision ("sha256:...") of the
// bundle. ConfigMap names embed the short revision, so any settings change
// produces a new ConfigMap and a new Fastlet Pod template hash.
func (b Bundle) Revision() (string, error) {
	files, err := b.Files()
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	keys := []string{ActionHandlersKey, ResourceProfileKey, WarmImagesKey}
	if _, found := files[ResourceAdmissionKey]; found {
		keys = append(keys, ResourceAdmissionKey)
	}
	for _, key := range keys {
		digest.Write([]byte(key))
		digest.Write([]byte{0})
		digest.Write([]byte(files[key]))
		digest.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}
