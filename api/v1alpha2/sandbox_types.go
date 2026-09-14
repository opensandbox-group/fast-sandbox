package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "sandbox.fast.io", Version: "v1alpha2"}
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

const SandboxConditionReady = "Ready"

// SandboxConditionSuspended reflects whether the Sandbox runtime is
// checkpointed and released. It is True only while the pause is durably
// complete (status.runtime.state == Paused); Pausing and Resuming are
// reported as False with a phase-specific reason.
const SandboxConditionSuspended = "Suspended"

// SandboxState is the desired runtime lifecycle.
// +kubebuilder:validation:Enum=Running;Paused
type SandboxState string

const (
	// SandboxStateRunning requests a live runtime. A Sandbox that carries a
	// checkpoint (status.runtime.checkpoint) is resumed from it instead of
	// booting from scratch.
	SandboxStateRunning SandboxState = "Running"
	// SandboxStatePaused requests the runtime be checkpointed to the
	// artifact store and released: the Sandbox stops occupying Fastlet
	// capacity while its object, identity, and checkpoint address survive.
	// Pausing requires a Ready runtime and is one-way through the
	// checkpoint; resuming means flipping this field back to Running.
	SandboxStatePaused SandboxState = "Paused"
)

// SandboxSpec defines the desired state of Sandbox.
// +kubebuilder:validation:XValidation:rule="!has(self.actionBindings) || self.actionBindings.all(x, self.actionBindings.filter(y, y.handler == x.handler).size() == 1)",message="actionBindings must use unique Handler names"
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
	// When Spec.ResetRevision > Status.Runtime.AcceptedResetRevision, the sandbox will be rescheduled.
	ResetRevision *metav1.Time `json:"resetRevision,omitempty"`

	// State is the desired runtime lifecycle. Running (the default) keeps a
	// live runtime; Paused checkpoints the runtime to the artifact store
	// and releases it. Pausing requires a Ready runtime; flipping back to
	// Running resumes the recorded checkpoint, possibly on a different
	// Fastlet.
	// +kubebuilder:default=Running
	State SandboxState `json:"state,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// PoolRef specifies which SandboxPool this sandbox should be scheduled to.
	// This field is required.
	PoolRef string `json:"poolRef"`

	// ActionBindings is atomic because its order defines Handler invocation order.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	ActionBindings []ActionBinding `json:"actionBindings,omitempty"`
}

type ActionBinding struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Handler string `json:"handler"`

	// Input is opaque Handler-owned data. Fast Sandbox preserves the UTF-8
	// bytes exactly and never parses or canonicalizes its contents.
	// +kubebuilder:validation:MaxLength=65536
	Input string `json:"input"`
}

// RuntimeState is the lifecycle of the concrete Sandbox runtime.
// +kubebuilder:validation:Enum=Unknown;Pending;Creating;Ready;Pausing;Paused;Resuming;Stopping;Stopped;Failed;Unavailable
type RuntimeState string

const (
	RuntimeUnknown  RuntimeState = "Unknown"
	RuntimePending  RuntimeState = "Pending"
	RuntimeCreating RuntimeState = "Creating"
	RuntimeReady    RuntimeState = "Ready"
	// RuntimePausing means the checkpoint window is running: the Fastlet is
	// dumping vmstate/memory/rootfs and publishing the artifact set. The
	// runtime is neither usable nor resumable yet.
	RuntimePausing RuntimeState = "Pausing"
	// RuntimePaused means the checkpoint is durable in the artifact store,
	// the runtime has been released, and the Sandbox occupies no Fastlet
	// capacity. Setting spec.state back to Running resumes it.
	RuntimePaused RuntimeState = "Paused"
	// RuntimeResuming means a new runtime is materializing from
	// status.runtime.checkpoint on a (possibly different) Fastlet.
	RuntimeResuming    RuntimeState = "Resuming"
	RuntimeStopping    RuntimeState = "Stopping"
	RuntimeStopped     RuntimeState = "Stopped"
	RuntimeFailed      RuntimeState = "Failed"
	RuntimeUnavailable RuntimeState = "Unavailable"
)

type RuntimeStatus struct {
	State      RuntimeState `json:"state,omitempty"`
	Generation int64        `json:"generation,omitempty"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`

	AcceptedResetRevision *metav1.Time `json:"acceptedResetRevision,omitempty"`

	// PauseAttempt is the retry epoch of the pause FSM. The controller
	// increments it before triggering a new checkpoint attempt (after a
	// transient failure) and derives the Fastlet checkpoint task identity
	// from it, so retrying a lost outcome replays the same task while a new
	// attempt never reuses a terminated task id. Reset clears it.
	// +optional
	PauseAttempt int64 `json:"pauseAttempt,omitempty"`

	// Checkpoint is the artifact-store checkpoint a Paused Sandbox resumes
	// from. It is authoritative only while State is Pausing, Paused, or
	// Resuming (see CheckpointActive); in any other state it must be read
	// as "no checkpoint" and is cleared by the controller. A Paused
	// Sandbox without a checkpoint cannot be resumed and must be reset.
	// +optional
	Checkpoint *CheckpointStatus `json:"checkpoint,omitempty"`
}

// CheckpointStatus describes one published checkpoint: the artifact-store
// address a Paused Sandbox resumes from. Unlike a SandboxSnapshot it is
// instance-private — no template-name index is written, so the set is not
// addressable as a CreateSandbox image.
type CheckpointStatus struct {
	// CheckpointID is the Fastlet-side task identity that produced the
	// checkpoint, derived from the Sandbox UID, the spec generation that
	// requested the pause, and the pause attempt epoch.
	CheckpointID string `json:"checkpointID"`

	// ManifestRef is the s3:// URI of the checkpoint manifest, published in
	// the standard artifact-set layout (rootfs/vmstate/memory/SHA256SUMS).
	ManifestRef string `json:"manifestRef"`
	// ArtifactDigest is the sha256 of the manifest document. It addresses
	// the artifact set independently of ManifestRef.
	ArtifactDigest string `json:"artifactDigest"`
	// SizeBytes is the total logical size of the published artifact set.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// PausedAt is when the checkpoint became durable in the store.
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`
	// FastletName/FastletPodUID locate the Fastlet that captured the
	// checkpoint. Diagnostics only: the Fastlet may already be gone and is
	// never the resume target contract.
	// +optional
	FastletName string `json:"fastletName,omitempty"`
	// +optional
	FastletPodUID types.UID `json:"fastletPodUID,omitempty"`
}

// CheckpointActive reports whether Status.Runtime.Checkpoint is
// authoritative: only pause-FSM states may carry a resumable checkpoint.
func (s *RuntimeStatus) CheckpointActive() bool {
	if s == nil || s.Checkpoint == nil {
		return false
	}
	switch s.State {
	case RuntimePausing, RuntimePaused, RuntimeResuming:
		return true
	default:
		return false
	}
}

// DataPlaneState is the lifecycle of the Sandbox interaction route.
// +kubebuilder:validation:Enum=Unknown;Pending;Publishing;Ready;Draining;Failed;Unavailable
type DataPlaneState string

const (
	DataPlaneUnknown     DataPlaneState = "Unknown"
	DataPlanePending     DataPlaneState = "Pending"
	DataPlanePublishing  DataPlaneState = "Publishing"
	DataPlaneReady       DataPlaneState = "Ready"
	DataPlaneDraining    DataPlaneState = "Draining"
	DataPlaneFailed      DataPlaneState = "Failed"
	DataPlaneUnavailable DataPlaneState = "Unavailable"
)

type DataPlaneStatus struct {
	State           DataPlaneState `json:"state,omitempty"`
	RouteGeneration int64          `json:"routeGeneration,omitempty"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

type PlacementStatus struct {
	Attempt int64 `json:"attempt,omitempty"`

	FastletName   string    `json:"fastletName,omitempty"`
	FastletPodUID types.UID `json:"fastletPodUID,omitempty"`

	Recovery *RecoveryStatus `json:"recovery,omitempty"`
}

type RecoveryStatus struct {
	DetectedAt metav1.Time `json:"detectedAt"`
	Deadline   metav1.Time `json:"deadline"`
}

// ActionState is the observed lifecycle of one Action Binding.
// +kubebuilder:validation:Enum=Pending;Applying;Ready;Failed
type ActionState string

const (
	ActionPending  ActionState = "Pending"
	ActionApplying ActionState = "Applying"
	ActionReady    ActionState = "Ready"
	ActionFailed   ActionState = "Failed"
)

type ActionBindingStatus struct {
	Handler string      `json:"handler"`
	State   ActionState `json:"state"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Placement PlacementStatus `json:"placement,omitempty"`
	Runtime   RuntimeStatus   `json:"runtime,omitempty"`
	DataPlane DataPlaneStatus `json:"dataPlane,omitempty"`

	// +listType=map
	// +listMapKey=name
	InfraComponents []InfraComponentStatus `json:"infraComponents,omitempty"`

	// +listType=atomic
	ActionBindings []ActionBindingStatus `json:"actionBindings,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InfraComponentState is the lifecycle of one named local component route.
// +kubebuilder:validation:Enum=Starting;Ready;Failed
type InfraComponentState string

const (
	InfraComponentStarting InfraComponentState = "Starting"
	InfraComponentReady    InfraComponentState = "Ready"
	InfraComponentFailed   InfraComponentState = "Failed"
)

type InfraComponentStatus struct {
	Name  string              `json:"name"`
	State InfraComponentState `json:"state"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
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
