package api

import "time"

// ConsistencyMode defines the consistency mode for sandbox creation.
type ConsistencyMode string

const (
	// ConsistencyModeFast creates sandbox on fastlet first, then writes CRD asynchronously.
	// Lowest latency, but CRD write failure may cause running sandbox to be cleaned up.
	ConsistencyModeFast ConsistencyMode = "fast"

	// ConsistencyModeStrong writes CRD first, then creates sandbox on fastlet.
	// Higher latency, but guarantees strong consistency.
	ConsistencyModeStrong ConsistencyMode = "strong"
)

// SandboxSpec describes the desired state of a sandbox on a fastlet.
type SandboxSpec struct {
	SandboxID           string            `json:"sandboxId"`
	RequestID           string            `json:"requestId,omitempty"`
	ClaimUID            string            `json:"claimUid"`
	ClaimNamespace      string            `json:"claimNamespace,omitempty"`
	ClaimName           string            `json:"claimName"`
	InstanceGeneration  int64             `json:"instanceGeneration,omitempty"`
	AssignmentAttempt   int64             `json:"assignmentAttempt,omitempty"`
	FastletPodUID       string            `json:"fastletPodUid,omitempty"`
	Image               string            `json:"image"`
	CPU                 string            `json:"cpu,omitempty"`
	Memory              string            `json:"memory,omitempty"`
	PIDs                int64             `json:"pids,omitempty"`
	RuntimeProfileHash  string            `json:"runtimeProfileHash,omitempty"`
	ResourceProfileHash string            `json:"resourceProfileHash,omitempty"`
	Command             []string          `json:"command,omitempty"`
	Args                []string          `json:"args,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	WorkingDir          string            `json:"workingDir,omitempty"`
}

// SandboxStatus represents the observed state of a sandbox on a fastlet.
type SandboxStatus struct {
	SandboxID          string `json:"sandboxId"`
	ClaimUID           string `json:"claimUid"`
	InstanceGeneration int64  `json:"instanceGeneration,omitempty"`
	AssignmentAttempt  int64  `json:"assignmentAttempt,omitempty"`
	Phase              string `json:"phase"`
	Message            string `json:"message,omitempty"`
	CreatedAt          int64  `json:"createdAt"` // Unix timestamp for orphan cleanup
}

// CreateSandboxRequest is sent to create a single sandbox on a fastlet.
type CreateSandboxRequest struct {
	Sandbox SandboxSpec `json:"sandbox"`
}

// CreateSandboxResponse is returned after creating a sandbox.
type CreateSandboxResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	SandboxID string `json:"sandboxId"`
	CreatedAt int64  `json:"createdAt"` // Unix timestamp when sandbox was created
}

// DeleteSandboxRequest is sent to delete a single sandbox from a fastlet.
type DeleteSandboxRequest struct {
	SandboxID string `json:"sandboxId"`
}

// DeleteSandboxResponse is returned after deleting a sandbox.
type DeleteSandboxResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// FastletStatus represents the current status of a fastlet (internal use).
type FastletStatus struct {
	FastletID           string          `json:"fastletId"`
	NodeName            string          `json:"nodeName"`
	Capacity            int             `json:"capacity"`
	Allocated           int             `json:"allocated"`
	Images              []string        `json:"images,omitempty"`
	SandboxStatuses     []SandboxStatus `json:"sandboxStatuses"`
	Admission           AdmissionStatus `json:"admission"`
	RuntimeReady        bool            `json:"runtimeReady"`
	Recovering          bool            `json:"recovering"`
	Draining            bool            `json:"draining"`
	FastletPodUID       string          `json:"fastletPodUid,omitempty"`
	ResourceProfileHash string          `json:"resourceProfileHash,omitempty"`
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
	RequestID          string `json:"requestId,omitempty"`
	SandboxUID         string `json:"sandboxUid"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
	FastletPodUID      string `json:"fastletPodUid"`
}

type AdmissionStatus struct {
	Capacity     int `json:"capacity"`
	Reservations int `json:"reservations"`
	Creating     int `json:"creating"`
	Running      int `json:"running"`
	Deleting     int `json:"deleting"`
	Used         int `json:"used"`
}

type ReserveSandboxRequest struct {
	RequestID           string `json:"requestId"`
	CreateSpecHash      string `json:"createSpecHash"`
	ClaimNamespace      string `json:"claimNamespace"`
	ClaimName           string `json:"claimName"`
	FastletPodUID       string `json:"fastletPodUid"`
	RuntimeProfileHash  string `json:"runtimeProfileHash"`
	ResourceProfileHash string `json:"resourceProfileHash"`
}

type ReserveSandboxResponse struct {
	ReservationToken string          `json:"reservationToken"`
	FastletPodUID    string          `json:"fastletPodUid"`
	ExpiresAt        time.Time       `json:"expiresAt"`
	Admission        AdmissionStatus `json:"admission"`
	Error            *FastletError   `json:"error,omitempty"`
}

type CancelReservationRequest struct {
	RequestID        string `json:"requestId"`
	ReservationToken string `json:"reservationToken"`
}

type CancelReservationResponse struct {
	Canceled bool          `json:"canceled"`
	Error    *FastletError `json:"error,omitempty"`
}

type EnsureSandboxRequest struct {
	Identity         SandboxIdentity `json:"identity"`
	ReservationToken string          `json:"reservationToken,omitempty"`
	CreateSpecHash   string          `json:"createSpecHash,omitempty"`
	Sandbox          SandboxSpec     `json:"sandbox"`
}

type EnsureSandboxResponse struct {
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
	State              string `json:"state"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
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
