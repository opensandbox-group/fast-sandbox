package api

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
	ClaimUID            string            `json:"claimUid"`
	ClaimName           string            `json:"claimName"`
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
	SandboxID string `json:"sandboxId"`
	ClaimUID  string `json:"claimUid"`
	Phase     string `json:"phase"`
	Message   string `json:"message,omitempty"`
	CreatedAt int64  `json:"createdAt"` // Unix timestamp for orphan cleanup
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
	FastletID       string          `json:"fastletId"`
	NodeName        string          `json:"nodeName"`
	Capacity        int             `json:"capacity"`
	Allocated       int             `json:"allocated"`
	Images          []string        `json:"images"`
	SandboxStatuses []SandboxStatus `json:"sandboxStatuses"`
}
