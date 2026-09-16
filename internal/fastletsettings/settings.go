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
)

// Bundle is the pool-declared declarative configuration projected into the
// settings ConfigMap. It is the Fastlet-facing projection of SandboxPool
// fields that describe desired sandbox behavior rather than pod identity.
type Bundle struct {
	WarmImages      []string                           `json:"warmImages"`
	ActionHandlers  []apiv1alpha2.ActionHandler        `json:"actionHandlers,omitempty"`
	ResourceProfile apiv1alpha2.SandboxResourceProfile `json:"resourceProfile"`
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
	return map[string]string{
		WarmImagesKey:      string(warmImagesJSON),
		ActionHandlersKey:  string(actionHandlersJSON),
		ResourceProfileKey: string(resourceProfile),
	}, nil
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
	for _, key := range []string{ActionHandlersKey, ResourceProfileKey, WarmImagesKey} {
		digest.Write([]byte(key))
		digest.Write([]byte{0})
		digest.Write([]byte(files[key]))
		digest.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}
