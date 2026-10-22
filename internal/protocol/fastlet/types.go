package fastlet

import (
	"encoding/json"
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

// SandboxDesiredSpec is caller-owned desired state sent to a Fastlet. Runtime
// identity and fencing are owned exclusively by SandboxIdentity. Resolved
// resources travel separately and are validated against the immutable Fastlet
// resource profile before RuntimeSandboxConfig is built.
type SandboxDesiredSpec struct {
	Image               string            `json:"image"`
	RuntimeProfileHash  string            `json:"runtimeProfileHash,omitempty"`
	ResourceProfileHash string            `json:"resourceProfileHash,omitempty"`
	InfraRevision       string            `json:"infraRevision,omitempty"`
	Command             []string          `json:"command,omitempty"`
	Args                []string          `json:"args,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	WorkingDir          string            `json:"workingDir,omitempty"`
	// Restore, when set, materializes this runtime from a checkpoint
	// artifact set (SnapshotKindCheckpoint) instead of booting Image from
	// scratch: the driver delivers and validates the set, loads vmstate and
	// memory, and resumes the guest. Only a Sandbox resume produces it.
	Restore *RestoreSpec `json:"restore,omitempty"`
}

func (s SandboxDesiredSpec) DeepCopy() SandboxDesiredSpec {
	copied := s
	copied.Command = append([]string(nil), s.Command...)
	copied.Args = append([]string(nil), s.Args...)
	if s.Env != nil {
		copied.Env = make(map[string]string, len(s.Env))
		for key, value := range s.Env {
			copied.Env[key] = value
		}
	}
	if s.Restore != nil {
		restore := *s.Restore
		copied.Restore = &restore
	}
	return copied
}

// SandboxRuntimeLimits is the Fastlet-resolved resource boundary consumed by
// RuntimeDriver implementations. It is never accepted from an HTTP caller.
type SandboxRuntimeLimits struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	PIDs   int64  `json:"pids,omitempty"`
}

// SandboxResourceVector is a CPU and memory vector on the internal wire.
type SandboxResourceVector struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// ResolvedSandboxResources is the Controller-resolved admission and runtime
// contract. Fastlet validates Requests against Limits and its immutable Pool
// policy before reserving either resource.
type ResolvedSandboxResources struct {
	Requests SandboxResourceVector `json:"requests"`
	Limits   SandboxRuntimeLimits  `json:"limits"`
}

// RestoreSpec selects the checkpoint artifact set a runtime is restored
// from. It is produced by a Checkpoint snapshot and consumed by
// EnsureSandbox at resume.
type RestoreSpec struct {
	// ManifestRef is the s3:// URI of the checkpoint manifest, in the
	// standard artifact-set layout (rootfs/vmstate/memory/SHA256SUMS).
	ManifestRef string `json:"manifestRef"`
	// ArtifactDigest is the sha256 of the manifest document; it addresses
	// the artifact set independently of ManifestRef.
	ArtifactDigest string `json:"artifactDigest"`
}

// RuntimeSandboxConfig is the stable runtime identity and desired
// configuration. The two authorities remain explicitly nested so runtime
// implementations cannot silently merge or overwrite duplicate fields.
type RuntimeSandboxConfig struct {
	Identity SandboxIdentity      `json:"identity"`
	Desired  SandboxDesiredSpec   `json:"desired"`
	Limits   SandboxRuntimeLimits `json:"limits"`
}

// UnmarshalJSON keeps durable runtime state written before desired state and
// resolved limits were separated readable during an in-place Fastlet upgrade.
// HTTP Create requests do not use RuntimeSandboxConfig and therefore do not
// inherit this compatibility path.
func (c *RuntimeSandboxConfig) UnmarshalJSON(data []byte) error {
	type legacyRuntimeSandboxSpec struct {
		SandboxDesiredSpec
		CPU    string `json:"cpu,omitempty"`
		Memory string `json:"memory,omitempty"`
		PIDs   int64  `json:"pids,omitempty"`
	}
	var wire struct {
		Identity SandboxIdentity           `json:"identity"`
		Desired  *SandboxDesiredSpec       `json:"desired"`
		Limits   *SandboxRuntimeLimits     `json:"limits"`
		Spec     *legacyRuntimeSandboxSpec `json:"spec"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Desired != nil && wire.Spec != nil {
		return errors.New("runtime Sandbox config cannot contain both desired and legacy spec")
	}
	*c = RuntimeSandboxConfig{Identity: wire.Identity}
	if wire.Spec != nil {
		c.Desired = wire.Spec.SandboxDesiredSpec
		c.Limits = SandboxRuntimeLimits{CPU: wire.Spec.CPU, Memory: wire.Spec.Memory, PIDs: wire.Spec.PIDs}
		return nil
	}
	if wire.Desired != nil {
		c.Desired = *wire.Desired
	}
	if wire.Limits != nil {
		c.Limits = *wire.Limits
	}
	return nil
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
	Reason    AdmissionReason  `json:"reason,omitempty"`
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
	Capacity  int                      `json:"capacity"`
	Creating  int                      `json:"creating"`
	Running   int                      `json:"running"`
	Deleting  int                      `json:"deleting"`
	Used      int                      `json:"used"`
	Resources *ResourceAdmissionStatus `json:"resources,omitempty"`
}

type AdmissionState string

const (
	AdmissionStateOpen     AdmissionState = "OPEN"
	AdmissionStateDegraded AdmissionState = "DEGRADED"
	AdmissionStateBlocked  AdmissionState = "BLOCKED"
	AdmissionStateUnknown  AdmissionState = "UNKNOWN"
)

type AdmissionReason string

const (
	AdmissionReasonSlotFull                   AdmissionReason = "SLOT_FULL"
	AdmissionReasonStaticCPUExhausted         AdmissionReason = "STATIC_CPU_EXHAUSTED"
	AdmissionReasonStaticMemoryExhausted      AdmissionReason = "STATIC_MEMORY_EXHAUSTED"
	AdmissionReasonFastletMemoryHighWatermark AdmissionReason = "FASTLET_MEMORY_HIGH_WATERMARK"
	AdmissionReasonFastletMemoryPSI           AdmissionReason = "FASTLET_MEMORY_PSI"
	AdmissionReasonFastletOOMEvent            AdmissionReason = "FASTLET_OOM_EVENT"
	AdmissionReasonNodeMemoryPressure         AdmissionReason = "NODE_MEMORY_PRESSURE"
	AdmissionReasonCPUSaturation              AdmissionReason = "CPU_SATURATION"
	AdmissionReasonCPUThrottling              AdmissionReason = "CPU_THROTTLING"
	AdmissionReasonTelemetryStale             AdmissionReason = "TELEMETRY_STALE"
	AdmissionReasonTelemetryMissing           AdmissionReason = "TELEMETRY_MISSING"
	AdmissionReasonRecoveryIncomplete         AdmissionReason = "RECOVERY_INCOMPLETE"
	AdmissionReasonUnsupportedProtocol        AdmissionReason = "UNSUPPORTED_PROTOCOL"
)

type ResourceAdmissionStatus struct {
	Version   string                `json:"version"`
	Epoch     string                `json:"epoch"`
	Revision  uint64                `json:"revision"`
	Capacity  SandboxResourceVector `json:"capacity"`
	Committed SandboxResourceVector `json:"committed"`
	State     AdmissionState        `json:"state"`
	Reason    AdmissionReason       `json:"reason,omitempty"`
}

type CreateSandboxRequest struct {
	RequestID      string                    `json:"requestId,omitempty"`
	Identity       SandboxIdentity           `json:"identity"`
	Sandbox        SandboxDesiredSpec        `json:"sandbox"`
	Resources      *ResolvedSandboxResources `json:"resources,omitempty"`
	SpecGeneration int64                     `json:"specGeneration,omitempty"`
	ActionBindings []ActionBindingInput      `json:"actionBindings,omitempty"`
	Completion     CreateCompletion          `json:"completion,omitempty"`
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
		failure := &CreateCallError{}
		if errors.As(current, &failure) {
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
	Admission AdmissionStatus `json:"admission"`
	Error     *FastletError   `json:"error,omitempty"`
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

// InvokeHandlerRequest relays one opaque payload to a Pool-declared Action
// Handler on the Sandbox's Pod loopback (the FastPath handler-relay channel).
// The Fastlet is the delivery endpoint: it forwards the payload verbatim and
// returns the handler's HTTP status and body verbatim; it never interprets
// either side.
type InvokeHandlerRequest struct {
	Identity SandboxIdentity `json:"identity"`
	// Handler is the Pool-declared Action Handler name; the Fastlet resolves
	// the handler's declared loopback port from its Pool settings.
	Handler string `json:"handler"`
	// Payload is opaque bytes relayed to the handler as the request body.
	Payload []byte `json:"payload,omitempty"`
	// Method is the HTTP method of the relayed request; empty defaults to
	// POST.
	Method string `json:"method,omitempty"`
	// Path is the loopback request path addressed on the handler; it must
	// start with '/'.
	Path string `json:"path"`
}

// InvokeHandlerResponse carries the handler's HTTP response verbatim. Status
// is the handler's HTTP status code (100-599): 0 means the relay produced no
// handler result, and FastPath rejects any other value as Unavailable. Error
// is reserved for relay failures (unknown Sandbox, undeclared handler,
// unreachable handler) and never carries the handler's own outcome.
type InvokeHandlerResponse struct {
	Status int32         `json:"status"`
	Body   []byte        `json:"body,omitempty"`
	Error  *FastletError `json:"error,omitempty"`
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

// SnapshotKind selects what a snapshot task publishes.
type SnapshotKind string

const (
	// SnapshotKindTemplate publishes the artifact set under a template name
	// so later Sandbox creates can boot from it (SandboxSnapshot
	// semantics: the set becomes a CreateSandbox image).
	SnapshotKindTemplate SnapshotKind = "Template"
	// SnapshotKindCheckpoint publishes an instance-private checkpoint that
	// only the owning Sandbox resumes from: no template-index object is
	// written, so the set is never addressable as a CreateSandbox image.
	SnapshotKindCheckpoint SnapshotKind = "Checkpoint"
)

// SnapshotIdentity fences one snapshot task. For Template snapshots
// SnapshotUID is the SandboxSnapshot object UID and Namespace/Name identify
// that object; for Checkpoint snapshots SnapshotUID is the checkpoint task
// identity derived from the owning Sandbox (UID + spec generation + pause
// attempt) and Namespace/Name identify nothing further. Sandbox carries the
// full target Sandbox identity fence (validated like every other Sandbox
// request).
type SnapshotIdentity struct {
	SnapshotUID string          `json:"snapshotUid"`
	Namespace   string          `json:"namespace"`
	Name        string          `json:"name"`
	Sandbox     SandboxIdentity `json:"sandbox"`
}

// SnapshotSpec is the desired snapshot configuration.
type SnapshotSpec struct {
	// Kind selects the publish mode. Empty is treated as SnapshotKindTemplate
	// so callers that predate checkpoints keep working.
	Kind SnapshotKind `json:"kind,omitempty"`
	// TemplateName is required for native template snapshots and selects their
	// artifact-store index key. OCI templates use SnapshotID instead and need
	// no name index. Must be empty for Checkpoint.
	TemplateName string `json:"templateName,omitempty"`
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
