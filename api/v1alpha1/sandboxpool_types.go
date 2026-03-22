package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RuntimeType defines the isolation level for sandboxes in this pool.
type RuntimeType string

const (
	// RuntimeContainer is the default runc runtime (process-level isolation).
	RuntimeContainer RuntimeType = "container"
	// RuntimeGVisor uses gVisor with runsc (user-space kernel).
	RuntimeGVisor RuntimeType = "gvisor"
	// RuntimeKataQemu uses Kata Containers with QEMU hypervisor.
	RuntimeKataQemu RuntimeType = "kata-qemu"
	// RuntimeKataFc uses Kata Containers with Firecracker microVM.
	RuntimeKataFc RuntimeType = "kata-fc"
	// RuntimeKataClh uses Kata Containers with Cloud Hypervisor.
	RuntimeKataClh RuntimeType = "kata-clh"
)

// SandboxPoolSpec defines the desired state of SandboxPool.

type SandboxPoolSpec struct {
	Capacity PoolCapacity `json:"capacity"`

	MaxSandboxesPerPod int32 `json:"maxSandboxesPerPod,omitempty"`

	// RuntimeType specifies the secure runtime type for this pool.
	// Default: "container" (standard runc)
	// +kubebuilder:default=container
	RuntimeType RuntimeType `json:"runtimeType,omitempty"`

	// RuntimeClassName specifies the Kubernetes RuntimeClass to use for validation.
	// If not set, defaults to the string representation of RuntimeType.
	// Ignored when RuntimeType is "container".
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// ContainerdRuntimeHandler overrides the containerd runtime handler.
	// If not set, defaults based on RuntimeType.
	ContainerdRuntimeHandler string `json:"containerdRuntimeHandler,omitempty"`

	AgentTemplate corev1.PodTemplateSpec `json:"agentTemplate"`
}

// PoolCapacity describes the sizing policy of the agent pool.
type PoolCapacity struct {
	PoolMin   int32 `json:"poolMin"`
	PoolMax   int32 `json:"poolMax"`
	BufferMin int32 `json:"bufferMin"`
	BufferMax int32 `json:"bufferMax"`
}

// SandboxPoolStatus defines the observed state of SandboxPool.
type SandboxPoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	CurrentPods        int32              `json:"currentPods,omitempty"`
	ReadyPods          int32              `json:"readyPods,omitempty"`
	TotalAgents        int32              `json:"totalAgents,omitempty"`
	IdleAgents         int32              `json:"idleAgents,omitempty"`
	BusyAgents         int32              `json:"busyAgents,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// Pool condition types
const (
	PoolConditionRuntimeReady = "RuntimeReady"
)

// Pool condition reasons
const (
	ReasonRuntimeAvailable   = "RuntimeAvailable"
	ReasonRuntimeUnavailable = "RuntimeUnavailable"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SandboxPool is the Schema for the sandboxpools API.
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec,omitempty"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxPoolList contains a list of SandboxPool.
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func (in *SandboxPool) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SandboxPool)
	*out = *in
	return out
}

func (in *SandboxPoolList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SandboxPoolList)
	*out = *in
	return out
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
