package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "sandbox.fast.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

// FailurePolicy defines the action to take when the fastlet becomes unreachable.
// +kubebuilder:validation:Enum=Manual;AutoRecreate
type FailurePolicy string

const (
	// FailurePolicyManual means only report the failure in status, do nothing automatically.
	FailurePolicyManual FailurePolicy = "Manual"
	// FailurePolicyAutoRecreate means automatically reschedule the sandbox after timeout.
	FailurePolicyAutoRecreate FailurePolicy = "AutoRecreate"
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

// ObservedState is the independently observed state of a Sandbox subsystem.
// +kubebuilder:validation:Enum=Unknown;Pending;Creating;Ready;Draining;Stopped;Failed;Unavailable
type ObservedState string

const (
	ObservedStateUnknown     ObservedState = "Unknown"
	ObservedStatePending     ObservedState = "Pending"
	ObservedStateCreating    ObservedState = "Creating"
	ObservedStateReady       ObservedState = "Ready"
	ObservedStateDraining    ObservedState = "Draining"
	ObservedStateStopped     ObservedState = "Stopped"
	ObservedStateFailed      ObservedState = "Failed"
	ObservedStateUnavailable ObservedState = "Unavailable"
)

// Sandbox condition types form the durable lifecycle contract shared by the
// imperative create path, declarative controllers, Janitor, and clients.
const (
	SandboxConditionRuntimeReady   = "RuntimeReady"
	SandboxConditionDataPlaneReady = "DataPlaneReady"
)

// SandboxAssignment is the authoritative placement selected through a status
// resourceVersion compare-and-swap. FastletPodUID fences Pod replacement, while
// Attempt fences reassignment to a different Fastlet.
type SandboxAssignment struct {
	FastletName   string `json:"fastletName"`
	FastletPodUID string `json:"fastletPodUID"`
	NodeName      string `json:"nodeName,omitempty"`
	Attempt       int64  `json:"attempt"`
}

// SandboxSpec defines the desired state of Sandbox.
type SandboxSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image      string          `json:"image"`
	Command    []string        `json:"command,omitempty"`
	Args       []string        `json:"args,omitempty"`
	Envs       []corev1.EnvVar `json:"envs,omitempty"`
	WorkingDir string          `json:"workingDir,omitempty"`

	// ExpireTime specifies when this sandbox should expire and be garbage collected.
	// If not set, the sandbox will not expire automatically.
	ExpireTime *metav1.Time `json:"expireTime,omitempty"`

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
	// +kubebuilder:validation:MinLength=1
	// PoolRef specifies which SandboxPool this sandbox should be scheduled to.
	// This field is required.
	PoolRef string `json:"poolRef"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// Assignment is the authoritative placement for the active instance.
	Assignment *SandboxAssignment `json:"assignment,omitempty"`
	// AssignmentAttempt is the monotonic high-water mark retained even while
	// Assignment is cleared, so a reschedule can never reuse an old fence.
	AssignmentAttempt int64 `json:"assignmentAttempt,omitempty"`

	// InstanceGeneration fences reset/recreate operations for the same CRD UID.
	InstanceGeneration int64 `json:"instanceGeneration,omitempty"`

	// RouteGeneration fences stale local and cluster proxy routes.
	RouteGeneration int64 `json:"routeGeneration,omitempty"`

	RuntimeState     ObservedState `json:"runtimeState,omitempty"`
	DataPlaneState   ObservedState `json:"dataPlaneState,omitempty"`
	UserProcessState ObservedState `json:"userProcessState,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AcceptedResetRevision reflects the latest reset revision that was processed by the controller.
	AcceptedResetRevision *metav1.Time `json:"acceptedResetRevision,omitempty"`
}

// HasCondition reports whether a canonical condition currently has the given
// status and reason.
func (s *SandboxStatus) HasCondition(conditionType string, conditionStatus metav1.ConditionStatus, reason string) bool {
	if s == nil {
		return false
	}
	for index := range s.Conditions {
		condition := &s.Conditions[index]
		if condition.Type == conditionType && condition.Status == conditionStatus && condition.Reason == reason {
			return true
		}
	}
	return false
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

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
