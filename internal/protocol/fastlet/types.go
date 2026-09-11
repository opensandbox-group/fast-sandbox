package fastlet

import (
	"errors"
	"time"
)

type UserProcessStartSource string

const (
	UserProcessStartRuntimeDirect         UserProcessStartSource = "runtime_direct"
	UserProcessStartSandboxInitUnreported UserProcessStartSource = "sandbox_init_unreported"
	UserProcessStartExistingRuntime       UserProcessStartSource = "existing_runtime"
	UserProcessStartUnknown               UserProcessStartSource = "unknown"
)

// SandboxSpec is the desired runtime configuration sent to a Fastlet. Runtime
// identity and fencing are owned exclusively by SandboxIdentity.
type SandboxSpec struct {
	Image               string            `json:"image"`
	CPU                 string            `json:"cpu,omitempty"`
	Memory              string            `json:"memory,omitempty"`
	PIDs                int64             `json:"pids,omitempty"`
	RuntimeProfileHash  string            `json:"runtimeProfileHash,omitempty"`
	ResourceProfileHash string            `json:"resourceProfileHash,omitempty"`
	InfraRevision       string            `json:"infraRevision,omitempty"`
	Command             []string          `json:"command,omitempty"`
	Args                []string          `json:"args,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	WorkingDir          string            `json:"workingDir,omitempty"`
}

// RuntimeSandboxConfig is the stable runtime identity and desired
// configuration. The two authorities remain explicitly nested so runtime
// implementations cannot silently merge or overwrite duplicate fields.
type RuntimeSandboxConfig struct {
	Identity SandboxIdentity `json:"identity"`
	Spec     SandboxSpec     `json:"spec"`
}

// EnsureSandboxInput represents one idempotent Ensure call. RequestID
// correlates the invocation but is not part of the stable runtime identity.
type EnsureSandboxInput struct {
	RequestID string               `json:"requestId,omitempty"`
	Sandbox   RuntimeSandboxConfig `json:"sandbox"`
}

// NetworkAllocation is produced by a runtime driver after admission. It is
// never accepted as caller-owned desired configuration.
type NetworkAllocation struct {
	SlotID        string `json:"slotId,omitempty"`
	NamespacePath string `json:"namespacePath,omitempty"`
	IP            string `json:"ip,omitempty"`
	Gateway       string `json:"gateway,omitempty"`
	DNSPath       string `json:"dnsPath,omitempty"`
	PrivateCIDR   string `json:"privateCidr,omitempty"`
	HostVeth      string `json:"hostVeth,omitempty"`
}

// RuntimeAllocation contains resources actually assigned to one runtime.
// Allocation is observed/persisted state, separate from RuntimeSandboxConfig.
type RuntimeAllocation struct {
	Network NetworkAllocation `json:"network"`
}

type RuntimeState string

const (
	RuntimeStateUnknown     RuntimeState = "Unknown"
	RuntimeStatePending     RuntimeState = "Pending"
	RuntimeStateCreating    RuntimeState = "Creating"
	RuntimeStateReady       RuntimeState = "Ready"
	RuntimeStateStopping    RuntimeState = "Stopping"
	RuntimeStateStopped     RuntimeState = "Stopped"
	RuntimeStateFailed      RuntimeState = "Failed"
	RuntimeStateUnavailable RuntimeState = "Unavailable"
)

type DataPlaneState string

const (
	DataPlaneStateUnknown     DataPlaneState = "Unknown"
	DataPlaneStatePending     DataPlaneState = "Pending"
	DataPlaneStatePublishing  DataPlaneState = "Publishing"
	DataPlaneStateReady       DataPlaneState = "Ready"
	DataPlaneStateDraining    DataPlaneState = "Draining"
	DataPlaneStateFailed      DataPlaneState = "Failed"
	DataPlaneStateUnavailable DataPlaneState = "Unavailable"
)

type RuntimeObservation struct {
	State   RuntimeState `json:"state"`
	Message string       `json:"message,omitempty"`
}

type DataPlaneObservation struct {
	State   DataPlaneState `json:"state"`
	Message string         `json:"message,omitempty"`
}

// SandboxStatus is a structured Fastlet observation. Phase remains an
// implementation detail of SandboxManager and never crosses the wire.
type SandboxStatus struct {
	SandboxID          string                     `json:"sandboxId"`
	InstanceGeneration int64                      `json:"instanceGeneration,omitempty"`
	RuntimeInstanceID  string                     `json:"runtimeInstanceId,omitempty"`
	AssignmentAttempt  int64                      `json:"assignmentAttempt,omitempty"`
	RouteGeneration    int64                      `json:"routeGeneration,omitempty"`
	AcceptedGeneration int64                      `json:"acceptedGeneration,omitempty"`
	AppliedGeneration  int64                      `json:"appliedGeneration,omitempty"`
	Runtime            RuntimeObservation         `json:"runtime"`
	DataPlane          DataPlaneObservation       `json:"dataPlane"`
	InfraComponents    []InfraComponentDiagnostic `json:"infraComponents,omitempty"`
	ActionBindings     []ActionBindingStatus      `json:"actionBindings,omitempty"`
	CreatedAt          int64                      `json:"createdAt"` // Unix timestamp for orphan cleanup
}

// ActionBindingStatus is Fastlet-local observed state. The digest and fence
// fields are intentionally internal and are not projected to the public
// FastPath or Sandbox CRD status.
type ActionBindingStatus struct {
	Handler                    string    `json:"handler"`
	State                      string    `json:"state"`
	ObservedSpecGeneration     int64     `json:"observedSpecGeneration,omitempty"`
	DesiredInputDigest         string    `json:"desiredInputDigest,omitempty"`
	AppliedInputDigest         string    `json:"appliedInputDigest,omitempty"`
	ObservedAssignmentAttempt  int64     `json:"observedAssignmentAttempt,omitempty"`
	ObservedInstanceGeneration int64     `json:"observedInstanceGeneration,omitempty"`
	ObservedNetworkGeneration  int64     `json:"observedNetworkGeneration,omitempty"`
	LastTransitionTime         time.Time `json:"lastTransitionTime,omitempty"`
	Message                    string    `json:"message,omitempty"`
}

type InfraComponentDiagnostic struct {
	Component               string `json:"component"`
	Protocol                string `json:"protocol,omitempty"`
	Port                    uint32 `json:"port,omitempty"`
	State                   string `json:"state"`
	ObservedRouteGeneration int64  `json:"observedRouteGeneration,omitempty"`
	Message                 string `json:"message,omitempty"`
}

// FastletStatus represents the current status of a fastlet (internal use).
type FastletStatus struct {
	FastletID           string           `json:"fastletId"`
	NodeName            string           `json:"nodeName"`
	SandboxStatuses     []SandboxStatus  `json:"sandboxStatuses"`
	Admission           AdmissionStatus  `json:"admission"`
	RuntimeReady        bool             `json:"runtimeReady"`
	Recovering          bool             `json:"recovering"`
	Draining            bool             `json:"draining"`
	FastletPodUID       string           `json:"fastletPodUid,omitempty"`
	RuntimeProfileHash  string           `json:"runtimeProfileHash,omitempty"`
	ResourceProfileHash string           `json:"resourceProfileHash,omitempty"`
	InfraRevision       string           `json:"infraRevision,omitempty"`
	InfraReady          bool             `json:"infraReady"`
	PreparedArtifacts   []string         `json:"preparedArtifacts,omitempty"`
	RegistryRevision    string           `json:"registryRevision,omitempty"`
	WarmImages          []WarmImageState `json:"warmImages,omitempty"`
}

type WarmImageState struct {
	Image   string `json:"image"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type FastletErrorCode string

const (
	ErrorCapacityRejected   FastletErrorCode = "CapacityRejected"
	ErrorDraining           FastletErrorCode = "Draining"
	ErrorInProgress         FastletErrorCode = "InProgress"
	ErrorConflict           FastletErrorCode = "Conflict"
	ErrorStaleGeneration    FastletErrorCode = "StaleGeneration"
	ErrorStaleAssignment    FastletErrorCode = "StaleAssignment"
	ErrorRuntimeUnavailable FastletErrorCode = "RuntimeUnavailable"
	ErrorNetworkUnavailable FastletErrorCode = "NetworkUnavailable"
	ErrorInfraUnavailable   FastletErrorCode = "InfraUnavailable"
	ErrorActionUnavailable  FastletErrorCode = "ActionUnavailable"
	ErrorUnknownOutcome     FastletErrorCode = "UnknownOutcome"
	ErrorNotFound           FastletErrorCode = "NotFound"
	ErrorGenerationFenced   FastletErrorCode = "GenerationFenced"
	ErrorProfileMismatch    FastletErrorCode = "ProfileMismatch"
	// ErrorSnapshotInProgress reports the deterministic non-reentrancy
	// rejection: another non-terminal snapshot holds the target Sandbox.
	ErrorSnapshotInProgress FastletErrorCode = "SnapshotInProgress"
	// ErrorSnapshotUnsupported reports that the Fastlet's runtime driver
	// does not implement snapshots.
	ErrorSnapshotUnsupported FastletErrorCode = "SnapshotUnsupported"
)

type FastletError struct {
	Code      FastletErrorCode `json:"code"`
	Message   string           `json:"message"`
	Retryable bool             `json:"retryable"`
	Cause     error            `json:"-"`
}

func (e *FastletError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *FastletError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code) + ": " + e.Message
}

type SandboxIdentity struct {
	SandboxUID         string `json:"sandboxUid"`
	Namespace          string `json:"namespace"`
	Name               string `json:"name"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	RuntimeInstanceID  string `json:"runtimeInstanceId"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
	RouteGeneration    int64  `json:"routeGeneration,omitempty"`
	FastletPodUID      string `json:"fastletPodUid"`
}

type AdmissionStatus struct {
	Capacity int `json:"capacity"`
	Creating int `json:"creating"`
	Running  int `json:"running"`
	Deleting int `json:"deleting"`
	Used     int `json:"used"`
}

type CreateSandboxRequest struct {
	RequestID      string               `json:"requestId,omitempty"`
	Identity       SandboxIdentity      `json:"identity"`
	Sandbox        SandboxSpec          `json:"sandbox"`
	SpecGeneration int64                `json:"specGeneration,omitempty"`
	ActionBindings []ActionBindingInput `json:"actionBindings,omitempty"`
	Completion     CreateCompletion     `json:"completion,omitempty"`
}

type CreateCompletion string

const (
	CreateCompletionReady        CreateCompletion = "READY"
	CreateCompletionRuntimeReady CreateCompletion = "RUNTIME_READY"
)

type CreateDisposition string

const (
	CreateDispositionCreated                   CreateDisposition = "CREATED"
	CreateDispositionExisting                  CreateDisposition = "EXISTING"
	CreateDispositionInProgress                CreateDisposition = "IN_PROGRESS"
	CreateDispositionRejectedBeforeSideEffects CreateDisposition = "REJECTED_BEFORE_SIDE_EFFECTS"
	CreateDispositionFailedNeedsCleanup        CreateDisposition = "FAILED_NEEDS_CLEANUP"
	CreateDispositionUnknown                   CreateDisposition = "UNKNOWN"
	CreateDispositionGenerationFenced          CreateDisposition = "GENERATION_FENCED"
)

type CreateSandboxResponse struct {
	Disposition CreateDisposition `json:"disposition"`
	Sandbox     *SandboxStatus    `json:"sandbox,omitempty"`
	Admission   AdmissionStatus   `json:"admission"`
	Error       *FastletError     `json:"error,omitempty"`
}

// CreateCallError attaches the response's single disposition to a decoded
// Fastlet error without duplicating it in the wire error object.
type CreateCallError struct {
	Disposition CreateDisposition
	Failure     *FastletError
}

func (e *CreateCallError) Error() string {
	if e == nil || e.Failure == nil {
		return "Fastlet Create failed"
	}
	return e.Failure.Error()
}

func (e *CreateCallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Failure
}

func CreateDispositionFromError(err error) CreateDisposition {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if failure, ok := current.(*CreateCallError); ok {
			return failure.Disposition
		}
	}
	return CreateDispositionUnknown
}

type InspectSandboxRequest struct {
	Identity SandboxIdentity `json:"identity"`
}

type InspectSandboxResponse struct {
	Sandbox *SandboxStatus `json:"sandbox,omitempty"`
	Error   *FastletError  `json:"error,omitempty"`
}

type DeleteSandboxRequest struct {
	Identity SandboxIdentity `json:"identity"`
}

type DeleteSandboxResponse struct {
	Error *FastletError `json:"error,omitempty"`
}

type ActionBindingInput struct {
	Handler string `json:"handler"`
	Input   string `json:"input"`
}

// ReconcileBindingsRequest carries persisted desired Binding state from
// the Sandbox Controller. Lifecycle Hooks are always dispatched by Fastlet.
type ReconcileBindingsRequest struct {
	Identity       SandboxIdentity      `json:"identity"`
	SpecGeneration int64                `json:"specGeneration"`
	ActionBindings []ActionBindingInput `json:"actionBindings"`
}

type ReconcileBindingsResponse struct {
	Sandbox *SandboxStatus `json:"sandbox,omitempty"`
	Error   *FastletError  `json:"error,omitempty"`
}

type SetDrainingRequest struct {
	Draining bool   `json:"draining"`
	Reason   string `json:"reason,omitempty"`
}

type SetDrainingResponse struct {
	Draining bool `json:"draining"`
}

type RuntimeDiagnostics struct {
	RuntimeProfileHash string `json:"runtimeProfileHash"`
	InfraRevision      string `json:"infraRevision,omitempty"`
	InfraState         string `json:"infraState,omitempty"`
	InfraMessage       string `json:"infraMessage,omitempty"`
	State              string `json:"state"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// SandboxDiagnosticEvent is a bounded Fastlet-side lifecycle record. It is
// platform diagnostics, not stdout/stderr from a process inside the Sandbox.
type SandboxDiagnosticEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Phase     string    `json:"phase,omitempty"`
	Message   string    `json:"message"`
}

type SandboxDiagnosticsRequest struct {
	Identity SandboxIdentity `json:"identity"`
	Limit    int             `json:"limit,omitempty"`
}

type SandboxDiagnosticsResponse struct {
	Sandbox *SandboxStatus           `json:"sandbox,omitempty"`
	Events  []SandboxDiagnosticEvent `json:"events,omitempty"`
	Error   *FastletError            `json:"error,omitempty"`
}

type CacheCursor struct {
	Epoch     string `json:"epoch,omitempty"`
	Revision  uint64 `json:"revision,omitempty"`
	ForceFull bool   `json:"forceFull,omitempty"`
}

type CacheSnapshot struct {
	Epoch    string   `json:"epoch"`
	Revision uint64   `json:"revision"`
	Full     bool     `json:"full"`
	Complete bool     `json:"complete"`
	Images   []string `json:"images,omitempty"`
}

type HeartbeatRequest struct {
	Cache CacheCursor `json:"cache"`
}

type HeartbeatResponse struct {
	FastletStatus
	Sequence uint64        `json:"sequence"`
	Cache    CacheSnapshot `json:"cache"`
}

// SnapshotPhase is the Fastlet-side lifecycle of one snapshot task. It
// mirrors the SandboxSnapshot CRD phase; Pending means accepted but the
// worker has not started the dump yet.
type SnapshotPhase string

const (
	SnapshotPhasePending    SnapshotPhase = "Pending"
	SnapshotPhaseCreating   SnapshotPhase = "Creating"
	SnapshotPhasePublishing SnapshotPhase = "Publishing"
	SnapshotPhaseSucceeded  SnapshotPhase = "Succeeded"
	SnapshotPhaseFailed     SnapshotPhase = "Failed"
)

// SnapshotPhaseTerminal reports whether the phase is final.
func SnapshotPhaseTerminal(phase SnapshotPhase) bool {
	return phase == SnapshotPhaseSucceeded || phase == SnapshotPhaseFailed
}

// SnapshotIdentity fences one snapshot task. SnapshotUID/Name/Namespace
// identify the SandboxSnapshot object; Sandbox carries the full target
// Sandbox identity fence (validated like every other Sandbox request).
type SnapshotIdentity struct {
	SnapshotUID string          `json:"snapshotUid"`
	Namespace   string          `json:"namespace"`
	Name        string          `json:"name"`
	Sandbox     SandboxIdentity `json:"sandbox"`
}

// SnapshotSpec is the desired snapshot configuration.
type SnapshotSpec struct {
	// TemplateName is the artifact-store index key the artifact set is
	// published under.
	TemplateName string `json:"templateName"`
}

// SnapshotStatus is the Fastlet observation of one snapshot task. Reason
// is a stable classification of a Failed task (e.g. InsufficientStorage);
// empty for in-flight and unclassified failures.
type SnapshotStatus struct {
	SnapshotID     string        `json:"snapshotId,omitempty"`
	Phase          SnapshotPhase `json:"phase"`
	Reason         string        `json:"reason,omitempty"`
	Message        string        `json:"message,omitempty"`
	ManifestRef    string        `json:"manifestRef,omitempty"`
	ArtifactDigest string        `json:"artifactDigest,omitempty"`
	SizeBytes      int64         `json:"sizeBytes,omitempty"`
	StartedAt      time.Time     `json:"startedAt,omitempty"`
	CompletedAt    time.Time     `json:"completedAt,omitempty"`
}

// CreateSnapshotRequest registers one snapshot task. The response returns
// after admission (EXISTING replays report the current task state); the
// dump/publish runs in a Fastlet worker and is observed via
// InspectSnapshotRequest.
type CreateSnapshotRequest struct {
	RequestID string           `json:"requestId,omitempty"`
	Identity  SnapshotIdentity `json:"identity"`
	Snapshot  SnapshotSpec     `json:"snapshot"`
	// ActionBindings are the source Sandbox's bindings, recorded in the
	// published manifest (durable policy provenance).
	ActionBindings []ActionBindingInput `json:"actionBindings,omitempty"`
}

type CreateSnapshotResponse struct {
	Disposition CreateDisposition `json:"disposition"`
	Snapshot    *SnapshotStatus   `json:"snapshot,omitempty"`
	Error       *FastletError     `json:"error,omitempty"`
}

type InspectSnapshotRequest struct {
	Identity SnapshotIdentity `json:"identity"`
}

type InspectSnapshotResponse struct {
	Snapshot *SnapshotStatus `json:"snapshot,omitempty"`
	Error    *FastletError   `json:"error,omitempty"`
}

// DeleteSnapshotRequest discards the node-local artifacts of a terminal
// snapshot task. A non-terminal task is rejected with ErrorInProgress: the
// Fastlet never aborts a running dump.
type DeleteSnapshotRequest struct {
	Identity SnapshotIdentity `json:"identity"`
}

type DeleteSnapshotResponse struct {
	Error *FastletError `json:"error,omitempty"`
}
