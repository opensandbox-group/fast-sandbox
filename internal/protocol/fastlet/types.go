package fastlet

import "time"

type UserProcessStartSource string

const (
	UserProcessStartRuntimeDirect         UserProcessStartSource = "runtime_direct"
	UserProcessStartSandboxInitUnreported UserProcessStartSource = "sandbox_init_unreported"
	UserProcessStartExistingRuntime       UserProcessStartSource = "existing_runtime"
	UserProcessStartUnknown               UserProcessStartSource = "unknown"
)

// SandboxSpec describes the desired state of a sandbox on a fastlet.
type SandboxSpec struct {
	SandboxID           string            `json:"sandboxId"`
	RequestID           string            `json:"requestId,omitempty"`
	ClaimUID            string            `json:"claimUid"`
	ClaimNamespace      string            `json:"claimNamespace,omitempty"`
	ClaimName           string            `json:"claimName"`
	InstanceGeneration  int64             `json:"instanceGeneration,omitempty"`
	RuntimeInstanceID   string            `json:"runtimeInstanceId,omitempty"`
	AssignmentAttempt   int64             `json:"assignmentAttempt,omitempty"`
	RouteGeneration     int64             `json:"routeGeneration,omitempty"`
	FastletPodUID       string            `json:"fastletPodUid,omitempty"`
	Image               string            `json:"image"`
	CPU                 string            `json:"cpu,omitempty"`
	Memory              string            `json:"memory,omitempty"`
	PIDs                int64             `json:"pids,omitempty"`
	RuntimeProfileHash  string            `json:"runtimeProfileHash,omitempty"`
	ResourceProfileHash string            `json:"resourceProfileHash,omitempty"`
	InfraProfile        string            `json:"infraProfile,omitempty"`
	InfraProfileHash    string            `json:"infraProfileHash,omitempty"`
	Command             []string          `json:"command,omitempty"`
	Args                []string          `json:"args,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	WorkingDir          string            `json:"workingDir,omitempty"`
	// Network fields are Fastlet-local runtime material. They are populated
	// only after admission and are never accepted from the public/RPC API.
	NetworkSlotID        string `json:"-"`
	NetworkNamespacePath string `json:"-"`
	NetworkIP            string `json:"-"`
	NetworkGateway       string `json:"-"`
	NetworkDNSPath       string `json:"-"`
}

// SandboxStatus represents the observed state of a sandbox on a fastlet.
type SandboxStatus struct {
	SandboxID          string                     `json:"sandboxId"`
	ClaimUID           string                     `json:"claimUid"`
	InstanceGeneration int64                      `json:"instanceGeneration,omitempty"`
	RuntimeInstanceID  string                     `json:"runtimeInstanceId,omitempty"`
	AssignmentAttempt  int64                      `json:"assignmentAttempt,omitempty"`
	RouteGeneration    int64                      `json:"routeGeneration,omitempty"`
	Phase              string                     `json:"phase"`
	Message            string                     `json:"message,omitempty"`
	InfraDiagnostics   []InfraComponentDiagnostic `json:"infraDiagnostics,omitempty"`
	CreatedAt          int64                      `json:"createdAt"` // Unix timestamp for orphan cleanup
}

type InfraComponentDiagnostic struct {
	Component string `json:"component"`
	Service   string `json:"service"`
	Required  bool   `json:"required"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
}

// FastletStatus represents the current status of a fastlet (internal use).
type FastletStatus struct {
	FastletID           string          `json:"fastletId"`
	NodeName            string          `json:"nodeName"`
	Capacity            int             `json:"capacity"`
	Images              []string        `json:"images,omitempty"`
	SandboxStatuses     []SandboxStatus `json:"sandboxStatuses"`
	Admission           AdmissionStatus `json:"admission"`
	RuntimeReady        bool            `json:"runtimeReady"`
	Recovering          bool            `json:"recovering"`
	Draining            bool            `json:"draining"`
	FastletPodUID       string          `json:"fastletPodUid,omitempty"`
	ResourceProfileHash string          `json:"resourceProfileHash,omitempty"`
	InfraProfile        string          `json:"infraProfile,omitempty"`
	InfraProfileHash    string          `json:"infraProfileHash,omitempty"`
	InfraReady          bool            `json:"infraReady"`
	PreparedArtifacts   []string        `json:"preparedArtifacts,omitempty"`
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
	ErrorUnknownOutcome     FastletErrorCode = "UnknownOutcome"
	ErrorNotFound           FastletErrorCode = "NotFound"
	ErrorGenerationFenced   FastletErrorCode = "GenerationFenced"
	ErrorProfileMismatch    FastletErrorCode = "ProfileMismatch"
)

type FastletOutcome string

const (
	OutcomeRejectedBeforeSideEffects FastletOutcome = "RejectedBeforeSideEffects"
	OutcomeInProgress                FastletOutcome = "InProgress"
	OutcomeCreated                   FastletOutcome = "Created"
	OutcomeFailedNeedsCleanup        FastletOutcome = "FailedNeedsCleanup"
	OutcomeUnknown                   FastletOutcome = "Unknown"
	OutcomeGenerationFenced          FastletOutcome = "GenerationFenced"
)

type FastletError struct {
	Code      FastletErrorCode `json:"code"`
	Message   string           `json:"message"`
	Retryable bool             `json:"retryable"`
	Outcome   FastletOutcome   `json:"outcome"`
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
	RequestID          string `json:"requestId,omitempty"`
	SandboxUID         string `json:"sandboxUid"`
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
	Identity SandboxIdentity `json:"identity"`
	Sandbox  SandboxSpec     `json:"sandbox"`
}

type CreateSandboxResponse struct {
	Accepted   bool            `json:"accepted"`
	Created    bool            `json:"created"`
	InProgress bool            `json:"inProgress"`
	Sandbox    *SandboxStatus  `json:"sandbox,omitempty"`
	Admission  AdmissionStatus `json:"admission"`
	Error      *FastletError   `json:"error,omitempty"`
}

type InspectSandboxRequest struct {
	Identity SandboxIdentity `json:"identity"`
}

type InspectSandboxResponse struct {
	Sandbox *SandboxStatus `json:"sandbox,omitempty"`
	Error   *FastletError  `json:"error,omitempty"`
}

type DeleteSandboxV2Request struct {
	Identity SandboxIdentity `json:"identity"`
}

type DeleteSandboxV2Response struct {
	Accepted bool          `json:"accepted"`
	Error    *FastletError `json:"error,omitempty"`
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
	InfraProfile       string `json:"infraProfile,omitempty"`
	InfraProfileHash   string `json:"infraProfileHash,omitempty"`
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
	Sequence    uint64             `json:"sequence"`
	ObservedAt  time.Time          `json:"observedAt"`
	Cache       CacheSnapshot      `json:"cache"`
	Diagnostics RuntimeDiagnostics `json:"diagnostics"`
}
