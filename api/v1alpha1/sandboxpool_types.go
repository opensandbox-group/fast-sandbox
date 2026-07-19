package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeName is the canonical runtime profile selected by a SandboxPool.
// +kubebuilder:validation:Enum=container;gvisor;kata-qemu;kata-clh;kata-fc;boxlite
type RuntimeName string

// RuntimeType is retained as a source-compatible alias during the v1alpha1
// migration. New code should use RuntimeName and SandboxPoolSpec.Runtime.
type RuntimeType = RuntimeName

const (
	// RuntimeContainer is the default runc runtime (process-level isolation).
	RuntimeContainer RuntimeName = "container"
	// RuntimeGVisor uses gVisor with runsc (user-space kernel).
	RuntimeGVisor RuntimeName = "gvisor"
	// RuntimeKataQemu uses Kata Containers with QEMU hypervisor.
	RuntimeKataQemu RuntimeName = "kata-qemu"
	// RuntimeKataFc uses Kata Containers with Firecracker microVM.
	RuntimeKataFc RuntimeName = "kata-fc"
	// RuntimeKataClh uses Kata Containers with Cloud Hypervisor.
	RuntimeKataClh RuntimeName = "kata-clh"
	// RuntimeBoxLite uses the BoxLite microVM runtime driver.
	RuntimeBoxLite RuntimeName = "boxlite"
)

// SandboxResourceProfile defines the fixed resource limit for every Sandbox in
// a Pool. Fastlet is the component that enforces these values at runtime.
type SandboxResourceProfile struct {
	CPU    resource.Quantity `json:"cpu,omitempty"`
	Memory resource.Quantity `json:"memory,omitempty"`
	// +kubebuilder:validation:Minimum=0
	PIDs int64 `json:"pids,omitempty"`
}

// SandboxPoolSpec defines the desired state of SandboxPool.
// +kubebuilder:validation:XValidation:rule="!(has(self.runtime) && (has(self.runtimeType) || has(self.runtimeClassName) || has(self.containerdRuntimeHandler)))",message="spec.runtime cannot be combined with deprecated runtime fields"
type SandboxPoolSpec struct {
	Capacity PoolCapacity `json:"capacity"`

	// +kubebuilder:validation:Minimum=1
	MaxSandboxesPerPod int32 `json:"maxSandboxesPerPod,omitempty"`

	// Runtime selects one immutable, platform-owned runtime profile.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime is immutable"
	Runtime RuntimeName `json:"runtime,omitempty"`

	// SandboxResources is the immutable resource profile applied to each
	// Sandbox created from this Pool.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sandboxResources is immutable"
	SandboxResources SandboxResourceProfile `json:"sandboxResources,omitempty"`

	// WarmImages are asynchronously pulled and protected from ordinary cache GC.
	// Fastlet readiness does not wait for this list to finish warming.
	WarmImages []string `json:"warmImages,omitempty"`

	// InfraProfile selects a platform-controlled Runtime Augmentation profile.
	// +kubebuilder:default=minimal
	InfraProfile string `json:"infraProfile,omitempty"`

	// RuntimeType is the deprecated runtime field accepted only for old objects.
	// New objects must write Runtime. Runtime and RuntimeType may not coexist.
	// +deprecated
	RuntimeType RuntimeName `json:"runtimeType,omitempty"`

	// RuntimeClassName specifies the Kubernetes RuntimeClass to use for validation.
	// If not set, defaults to the string representation of RuntimeType.
	// Ignored when RuntimeType is "container".
	// +deprecated
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// ContainerdRuntimeHandler overrides the containerd runtime handler.
	// If not set, defaults based on RuntimeType.
	// +deprecated
	ContainerdRuntimeHandler string `json:"containerdRuntimeHandler,omitempty"`

	// FastletTemplate is intentionally preserved as a Kubernetes-native pod
	// template. Fast Sandbox validates the platform-owned fields separately.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	FastletTemplate corev1.PodTemplateSpec `json:"fastletTemplate"`
}

// PoolCapacity describes the sizing policy of the fastlet pool.
type PoolCapacity struct {
	// +kubebuilder:validation:Minimum=0
	PoolMin int32 `json:"poolMin"`
	// +kubebuilder:validation:Minimum=0
	PoolMax int32 `json:"poolMax"`
	// +kubebuilder:validation:Minimum=0
	BufferMin int32 `json:"bufferMin"`
	// +kubebuilder:validation:Minimum=0
	BufferMax int32 `json:"bufferMax"`
}

// SandboxPoolStatus defines the observed state of SandboxPool.
type SandboxPoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	CurrentPods        int32              `json:"currentPods,omitempty"`
	ReadyPods          int32              `json:"readyPods,omitempty"`
	TotalFastlets      int32              `json:"totalFastlets,omitempty"`
	IdleFastlets       int32              `json:"idleFastlets,omitempty"`
	BusyFastlets       int32              `json:"busyFastlets,omitempty"`
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

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
