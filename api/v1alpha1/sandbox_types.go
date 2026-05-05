package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "sandbox.fast.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

// FailurePolicy defines the action to take when the fastlet becomes unreachable.
type FailurePolicy string

const (
	// FailurePolicyManual means only report the failure in status, do nothing automatically.
	FailurePolicyManual FailurePolicy = "Manual"
	// FailurePolicyAutoRecreate means automatically reschedule the sandbox after timeout.
	FailurePolicyAutoRecreate FailurePolicy = "AutoRecreate"
)

// SandboxPhase defines the lifecycle phase of a Sandbox in the Controller.
// +kubebuilder:validation:Enum=Pending;Bound;Running;Terminating;Expired;Failed;Lost
type SandboxPhase string

const (
	// PhasePending - Sandbox has been scheduled to a Fastlet but container not yet created.
	PhasePending SandboxPhase = "Pending"
	// PhaseBound - Container creation request sent to Fastlet, waiting for confirmation.
	PhaseBound SandboxPhase = "Bound"
	// PhaseRunning - Container is running on the Fastlet (synced from Fastlet status).
	PhaseRunning SandboxPhase = "Running"
	// PhaseTerminating - Sandbox is being deleted, waiting for Fastlet to confirm cleanup.
	PhaseTerminating SandboxPhase = "Terminating"
	// PhaseExpired - Sandbox has expired, runtime resources cleaned but CRD preserved.
	PhaseExpired SandboxPhase = "Expired"
	// PhaseFailed - Sandbox creation or operation failed.
	PhaseFailed SandboxPhase = "Failed"
	// PhaseLost - Fastlet Pod was lost under Manual failure policy, waiting for user intervention.
	PhaseLost SandboxPhase = "Lost"
)

// FastletSandboxPhase defines the lifecycle phase reported by the Fastlet.
type FastletSandboxPhase string

const (
	// FastletPhaseCreating - Fastlet is creating the container.
	FastletPhaseCreating FastletSandboxPhase = "creating"
	// FastletPhaseRunning - Container is running.
	FastletPhaseRunning FastletSandboxPhase = "running"
	// FastletPhaseStopped - Container has stopped.
	FastletPhaseStopped FastletSandboxPhase = "stopped"
	// FastletPhaseFailed - Container creation or execution failed.
	FastletPhaseFailed FastletSandboxPhase = "failed"
	// FastletPhaseTerminated - Container has been deleted and cleaned up.
	FastletPhaseTerminated FastletSandboxPhase = "terminated"
)

// SandboxSpec defines the desired state of Sandbox.
type SandboxSpec struct {
	Image      string          `json:"image"`
	Command    []string        `json:"command,omitempty"`
	Args       []string        `json:"args,omitempty"`
	Envs       []corev1.EnvVar `json:"envs,omitempty"`
	WorkingDir string          `json:"workingDir,omitempty"`

	// ExpireTime specifies when this sandbox should expire and be garbage collected.
	// If not set, the sandbox will not expire automatically.
	ExpireTime *metav1.Time `json:"expireTime,omitempty"`

	// ExposedPorts specifies the ports that the sandbox application will listen on.
	// The controller ensures no port conflicts on the same Fastlet Pod during scheduling.
	ExposedPorts []int32 `json:"exposedPorts,omitempty"`

	// FailurePolicy defines the recovery strategy when the fastlet is lost.
	// Defaults to "Manual".
	// +kubebuilder:default="Manual"
	FailurePolicy FailurePolicy `json:"failurePolicy,omitempty"`

	// RecoveryTimeoutSeconds is the duration to wait before taking action after losing contact with fastlet.
	// Defaults to 60 seconds.
	// +kubebuilder:default=60
	RecoveryTimeoutSeconds int32 `json:"recoveryTimeoutSeconds,omitempty"`

	// ResetRevision is an opaque token (usually a timestamp) used to trigger a manual reset.
	// When Spec.ResetRevision > Status.AcceptedResetRevision, the sandbox will be rescheduled.
	ResetRevision *metav1.Time `json:"resetRevision,omitempty"`

	// +kubebuilder:validation:Required
	// PoolRef specifies which SandboxPool this sandbox should be scheduled to.
	// This field is required.
	PoolRef string `json:"poolRef"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	Phase           string             `json:"phase,omitempty"`
	AssignedFastlet string             `json:"assignedFastlet,omitempty"`
	NodeName        string             `json:"nodeName,omitempty"`
	SandboxID       string             `json:"sandboxID,omitempty"`
	Endpoints       []string           `json:"endpoints,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`

	// AcceptedResetRevision reflects the latest reset revision that was processed by the controller.
	AcceptedResetRevision *metav1.Time `json:"acceptedResetRevision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Sandbox is the Schema for the sandboxes API.
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec,omitempty"`
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func (in *Sandbox) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(Sandbox)
	*out = *in
	return out
}

func (in *SandboxList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SandboxList)
	*out = *in
	return out
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
