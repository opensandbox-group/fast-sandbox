// Package runtime is the shared, platform-owned source of truth for
// Sandbox runtime profiles. Both Controllers and Fastlets resolve the same
// canonical profile; Pool users never provide backend handlers or paths.
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

type DriverKind string

const (
	DriverKindContainerd DriverKind = "containerd"
	DriverKindBoxLite    DriverKind = "boxlite"
)

type NetworkMode string

const (
	NetworkModeLinuxNetNS NetworkMode = "linux-netns"
	NetworkModeKata       NetworkMode = "kata"
	NetworkModeBoxLite    NetworkMode = "boxlite-gvproxy"
)

type InfraDeliveryMode string

const (
	InfraDeliveryBindMount      InfraDeliveryMode = "bind-mount"
	InfraDeliveryImageLayer     InfraDeliveryMode = "image-layer"
	InfraDeliveryPreinstalled   InfraDeliveryMode = "preinstalled"
	InfraDeliveryTemplateBake   InfraDeliveryMode = "template-bake"
	InfraDeliveryGuestCopy      InfraDeliveryMode = "guest-copy"
	InfraDeliveryArtifactVolume InfraDeliveryMode = "artifact-volume"
)

type CapabilityState string

const (
	CapabilityConfigured  CapabilityState = "Configured"
	CapabilityAvailable   CapabilityState = "Available"
	CapabilityReady       CapabilityState = "Ready"
	CapabilityDegraded    CapabilityState = "Degraded"
	CapabilityUnsupported CapabilityState = "Unsupported"
)

type ContainerdConfig struct {
	Handler     string `json:"handler"`
	RuntimePath string `json:"runtimePath,omitempty"`
	ConfigPath  string `json:"configPath,omitempty"`
	OptionsType string `json:"optionsType,omitempty"`
	NeedsTTY    bool   `json:"needsTTY,omitempty"`
}

type BoxLiteConfig struct {
	StateRoot       string `json:"stateRoot"`
	BinaryPath      string `json:"binaryPath"`
	ProxyBinary     string `json:"proxyBinary"`
	ControlSocket   string `json:"controlSocket"`
	ProtocolVersion string `json:"protocolVersion"`
	TunnelGuestPort uint32 `json:"tunnelGuestPort"`
	DefaultVCPUs    int32  `json:"defaultVCPUs"`
	DefaultMemory   string `json:"defaultMemory"`
}

type HostPathRequirement struct {
	Name             string                      `json:"name"`
	HostPath         string                      `json:"hostPath"`
	MountPath        string                      `json:"mountPath"`
	Type             corev1.HostPathType         `json:"type"`
	ReadOnly         bool                        `json:"readOnly,omitempty"`
	MountPropagation corev1.MountPropagationMode `json:"mountPropagation,omitempty"`
}

type DeploymentRequirements struct {
	Privileged    bool                  `json:"privileged"`
	RequiresKVM   bool                  `json:"requiresKVM,omitempty"`
	Sidecar       string                `json:"sidecar,omitempty"`
	ResourceOwner string                `json:"resourceOwner,omitempty"`
	NodeSelector  map[string]string     `json:"nodeSelector,omitempty"`
	HostPaths     []HostPathRequirement `json:"hostPaths,omitempty"`
	Overhead      corev1.ResourceList   `json:"overhead,omitempty"`
}

type Capabilities struct {
	DefaultState     CapabilityState `json:"defaultState"`
	SupportsNetwork  bool            `json:"supportsNetwork"`
	SupportsCache    bool            `json:"supportsCache"`
	SupportsRecovery bool            `json:"supportsRecovery"`
	Reason           string          `json:"reason,omitempty"`
}

type RuntimeProfile struct {
	Name               apiv1alpha1.RuntimeName `json:"name"`
	Version            string                  `json:"version"`
	ProfileHash        string                  `json:"profileHash"`
	Driver             DriverKind              `json:"driver"`
	Containerd         *ContainerdConfig       `json:"containerd,omitempty"`
	BoxLite            *BoxLiteConfig          `json:"boxlite,omitempty"`
	Deployment         DeploymentRequirements  `json:"deployment"`
	Capabilities       Capabilities            `json:"capabilities"`
	NetworkMode        NetworkMode             `json:"networkMode"`
	InfraDeliveryModes []InfraDeliveryMode     `json:"infraDeliveryModes"`
}

// UsesFastletNetNS reports whether the runtime consumes a Fastlet-owned Linux
// network namespace. Kata keeps a distinct NetworkMode because its shim turns
// that namespace into a guest NIC, but it still needs the same slot lifecycle
// and DirectIP access contract as the container and gVisor profiles.
func (p RuntimeProfile) UsesFastletNetNS() bool {
	return p.NetworkMode == NetworkModeLinuxNetNS || p.NetworkMode == NetworkModeKata
}

var ErrRuntimeNotFound = errors.New("runtime profile not found")

type Catalog struct {
	profiles map[apiv1alpha1.RuntimeName]RuntimeProfile
}

func Builtin() *Catalog {
	profiles := builtinProfiles()
	for name, profile := range profiles {
		profile.ProfileHash = mustProfileHash(profile)
		profiles[name] = profile
	}
	return &Catalog{profiles: profiles}
}

func (c *Catalog) Resolve(name apiv1alpha1.RuntimeName) (RuntimeProfile, error) {
	if name == "" {
		name = apiv1alpha1.RuntimeContainer
	}
	profile, ok := c.profiles[name]
	if !ok {
		return RuntimeProfile{}, fmt.Errorf("%w: %q", ErrRuntimeNotFound, name)
	}
	return cloneProfile(profile), nil
}

func (c *Catalog) Names() []apiv1alpha1.RuntimeName {
	names := make([]apiv1alpha1.RuntimeName, 0, len(c.profiles))
	for name := range c.profiles {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

func ProfileHash(profile RuntimeProfile) (string, error) {
	profile.ProfileHash = ""
	payload, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func mustProfileHash(profile RuntimeProfile) string {
	hash, err := ProfileHash(profile)
	if err != nil {
		panic(err)
	}
	return hash
}

func cloneProfile(profile RuntimeProfile) RuntimeProfile {
	clone := profile
	if profile.Containerd != nil {
		value := *profile.Containerd
		clone.Containerd = &value
	}
	if profile.BoxLite != nil {
		value := *profile.BoxLite
		clone.BoxLite = &value
	}
	clone.Deployment.NodeSelector = cloneStringMap(profile.Deployment.NodeSelector)
	clone.Deployment.HostPaths = append([]HostPathRequirement(nil), profile.Deployment.HostPaths...)
	clone.Deployment.Overhead = profile.Deployment.Overhead.DeepCopy()
	clone.InfraDeliveryModes = append([]InfraDeliveryMode(nil), profile.InfraDeliveryModes...)
	return clone
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
