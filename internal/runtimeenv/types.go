// Package runtimeenv resolves platform-owned runtime definitions against the
// container runtime installation present on a class of Kubernetes nodes.
package runtimeenv

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	actionapi "fast-sandbox/internal/protocol/action"
)

const (
	ConfigMapName      = "fast-sandbox-runtime-environments"
	ConfigMapKey       = "runtime-environments.yaml"
	SystemNamespace    = "fast-sandbox-system"
	DefaultEnvironment = "default"
	ConfigVersion      = "v1alpha2"
	PlanVersion        = "v1"
	PlanFileName       = "plan.json"
	PlanMountPath      = "/etc/fast-sandbox/runtime"
	ConfigMountPath    = "/etc/fast-sandbox/runtime-environments"
	ConfigFilePath     = ConfigMountPath + "/" + ConfigMapKey
)

type Config struct {
	Version           string                            `json:"version"`
	ActionsAPIVersion string                            `json:"actionsApiVersion,omitempty"`
	Environments      map[string]NodeRuntimeEnvironment `json:"environments"`
}

// NodeRuntimeEnvironment describes where a platform administrator installed
// containerd, kubelet and optional runtime-specific dependencies. Pool users
// cannot supply these privileged host paths.
type NodeRuntimeEnvironment struct {
	NodeSelector map[string]string                          `json:"nodeSelector,omitempty"`
	Containerd   ContainerdEnvironment                      `json:"containerd"`
	Kubelet      KubeletEnvironment                         `json:"kubelet"`
	HostPaths    []runtimecatalog.HostPathRequirement       `json:"hostPaths,omitempty"`
	Runtimes     map[apiv1alpha2.RuntimeName]RuntimeBinding `json:"runtimes,omitempty"`
}

type ContainerdEnvironment struct {
	Socket             string `json:"socket"`
	Namespace          string `json:"namespace"`
	DefaultSnapshotter string `json:"defaultSnapshotter"`
	Root               string `json:"root"`
}

type KubeletEnvironment struct {
	Root string `json:"root"`
}

type ContainerdEndpoint struct {
	Socket     string
	Namespaces []string
}

// RuntimeBinding contains installation values which may vary without changing
// the logical runtime definition.
type RuntimeBinding struct {
	Handler     string                               `json:"handler,omitempty"`
	Snapshotter string                               `json:"snapshotter,omitempty"`
	RuntimePath string                               `json:"runtimePath,omitempty"`
	ConfigPath  string                               `json:"configPath,omitempty"`
	OptionsType string                               `json:"optionsType,omitempty"`
	NeedsTTY    *bool                                `json:"needsTTY,omitempty"`
	HostPaths   []runtimecatalog.HostPathRequirement `json:"hostPaths,omitempty"`
	// FastletOverhead is the fixed CPU and memory budget consumed once by the
	// runtime-owner process. It is platform-owned and never enters the
	// per-Sandbox commitment ledger.
	FastletOverhead corev1.ResourceList `json:"fastletOverhead,omitempty"`
	// Firecracker carries the installation values of the Firecracker
	// runtime driver (binary/jailer/kernel paths and the state root). Only
	// non-empty fields override the builtin profile defaults; the profile's
	// Deployment.HostPaths are regenerated from the resolved configuration.
	// +optional
	Firecracker *FirecrackerBinding `json:"firecracker,omitempty"`
}

// FirecrackerBinding is the environment-side override of the Firecracker
// driver configuration (internal/catalog/runtime.FirecrackerConfig). Every
// field is optional; empty values keep the builtin profile default.
type FirecrackerBinding struct {
	BinaryPath string `json:"binaryPath,omitempty"`
	JailerPath string `json:"jailerPath,omitempty"`
	KernelPath string `json:"kernelPath,omitempty"`
	RootfsPath string `json:"rootfsPath,omitempty"`
	StateRoot  string `json:"stateRoot,omitempty"`
}

// ResolvedContainerdEnvironment is the per-runtime view consumed by Fastlet.
// Snapshotter has already applied the runtime binding override on top of the
// node environment default.
type ResolvedContainerdEnvironment struct {
	Socket      string `json:"socket"`
	Namespace   string `json:"namespace"`
	Snapshotter string `json:"snapshotter"`
	Root        string `json:"root"`
}

// ResolvedRuntimePlan is the immutable contract shared by the Pool
// controller, Fastlet and lifecycle placement. Revision is also used as the
// runtime profile identity advertised by Fastlet heartbeats.
type ResolvedRuntimePlan struct {
	Version           string                        `json:"version"`
	Environment       string                        `json:"environment"`
	Revision          string                        `json:"revision"`
	ActionsAPIVersion string                        `json:"actionsApiVersion,omitempty"`
	Profile           runtimecatalog.RuntimeProfile `json:"profile"`
	Containerd        ResolvedContainerdEnvironment `json:"containerd"`
	Kubelet           KubeletEnvironment            `json:"kubelet"`
}

// EffectiveActionsAPIVersion preserves pre-field runtime plans as legacy v1.
// New controller plans write an explicit version into their content digest.
func (p ResolvedRuntimePlan) EffectiveActionsAPIVersion() string {
	if p.ActionsAPIVersion == "" {
		return actionapi.APIVersion
	}
	return p.ActionsAPIVersion
}

func (p ResolvedRuntimePlan) Marshal() ([]byte, error) {
	return json.Marshal(p)
}
