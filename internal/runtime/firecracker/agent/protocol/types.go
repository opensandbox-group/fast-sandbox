// Package protocol defines the versioned UDS API shared by the
// firecracker-runtime server and the fastlet driver client. Messages
// are JSON over HTTP on a Unix socket (design docs §2.2).
//
// The served surface covers image lifecycle (PinImage / UnpinImage),
// device leases (LeaseDevices / ReleaseDevices / ListLeases), node
// capability reporting (Compatibility / Health), and artifact publication
// (PublishImage). Snapshot RPCs (PinSnapshot / LeaseSnapshotDevices /
// SealSnapshot) arrive with stage 4.
package protocol

import "time"

const ProtocolVersionV1 = "v1"

// RPC routes.
const (
	RoutePinImage       = "/v1/pin-image"
	RouteUnpinImage     = "/v1/unpin-image"
	RouteLeaseDevices   = "/v1/lease-devices"
	RouteReleaseDevices = "/v1/release-devices"
	RouteListLeases     = "/v1/list-leases"
	RouteCompatibility  = "/v1/compatibility"
	RouteHealth         = "/v1/health"
	RoutePublishImage   = "/v1/publish-image"
)

// Identity is the caller identity carried by every request. The server
// rejects empty PodUID (403) and binds idempotency keys and leases to the
// PodUID so cross-pod replays or releases fail with Conflict.
type Identity struct {
	RequestID string `json:"requestId"`
	Namespace string `json:"namespace,omitempty"`
	PodUID    string `json:"podUid"`
}

// ErrorCode is a stable wire error classification the driver maps onto
// runtime contract errors.
type ErrorCode string

const (
	ErrorInvalidRequest ErrorCode = "InvalidRequest" // 400
	ErrorUnauthorized   ErrorCode = "Unauthorized"   // 403 (identity missing or empty)
	ErrorConflict       ErrorCode = "Conflict"       // 409 (idempotency key or ownership mismatch)
	ErrorNotFound       ErrorCode = "NotFound"       // 404 (image not published)
	ErrorForbidden      ErrorCode = "Forbidden"      // 403 (store not writable: no write credential)
	ErrorInternal       ErrorCode = "Internal"       // 500
)

// ErrorResponse is the wire shape of a failed RPC.
type ErrorResponse struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// Publish kinds select the publication mode of an artifact set.
const (
	// PublishKindTemplate publishes a template set and writes the image
	// index last, so `image`-addressed creates resolve it.
	PublishKindTemplate = "template"
	// PublishKindCheckpoint publishes an instance-private set with NO image
	// index: the caller addresses it by the returned manifestRef +
	// artifactDigest and only the owning Sandbox resumes from it. Checkpoint
	// sets are never resolvable as CreateSandbox images.
	PublishKindCheckpoint = "checkpoint"
)

// PinImageRequest pulls and keeps an image pinned on the node.
type PinImageRequest struct {
	Identity
	Image string `json:"image"`
	// ManifestRef/ArtifactDigest, when both set, pin an artifact set
	// addressed directly (a pause checkpoint) instead of resolving the
	// image index. Image must carry the canonical checkpoint reference
	// (artifacts.CheckpointReference) so the local cache key is stable
	// across nodes.
	ManifestRef    string `json:"manifestRef,omitempty"`
	ArtifactDigest string `json:"artifactDigest,omitempty"`
}

// PinImageResponse reports the pinned image manifest digest.
type PinImageResponse struct {
	ManifestDigest string `json:"manifestDigest"`
	Ready          bool   `json:"ready"`
}

// UnpinImageRequest drops one pin reference of an image.
type UnpinImageRequest struct {
	Identity
	Image string `json:"image"`
}

// LeaseDevicesRequest creates a device lease for one Sandbox. In the native
// stage the lease returns the shared cache file paths; the device semantics
// arrive with the overlaybd stage. MemSizeMiB and RootfsWritable are carried
// now so the protocol is stable across stages.
type LeaseDevicesRequest struct {
	Identity
	SandboxID      string `json:"sandboxId"`
	Image          string `json:"image"`
	MemSizeMiB     int    `json:"memSizeMiB"`
	RootfsWritable bool   `json:"rootfsWritable"`
}

// LeaseDevicesResponse returns the lease handle and the device (or native
// cache file) paths for the Sandbox.
type LeaseDevicesResponse struct {
	LeaseID        string `json:"leaseId"`
	RootfsDev      string `json:"rootfsDev"`
	MemDev         string `json:"memDev"`
	ManifestDigest string `json:"manifestDigest"`
}

// ReleaseDevicesRequest drops a device lease.
type ReleaseDevicesRequest struct {
	Identity
	LeaseID string `json:"leaseId"`
}

// Lease is the durable view of a device lease, shared between the state
// journal and the protocol.
type Lease struct {
	LeaseID   string    `json:"leaseId"`
	SandboxID string    `json:"sandboxId"`
	Image     string    `json:"image"`
	PodUID    string    `json:"podUid"`
	Namespace string    `json:"namespace,omitempty"`
	RootfsDev string    `json:"rootfsDev"`
	MemDev    string    `json:"memDev,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListLeasesResponse returns every lease on the node (recovery/audit).
type ListLeasesResponse struct {
	Leases []Lease `json:"leases"`
}

// PublishImageRequest publishes a node-local artifact set (a live Sandbox
// snapshot staged by the firecracker driver) to the artifact store in the
// SandboxTemplate layout. Key is the index key the set becomes addressable
// under (the snapshot's template name); Dir is the staging directory holding
// rootfs.ext4, vmstate.snap, memory.snap, manifest.json, and SHA256SUMS.
// The upload order is artifacts first, manifest last within the digest
// namespace, and the index object last overall, so consumers never observe a
// half-published set. Idempotent: re-publishing identical bytes overwrites
// the same keys harmlessly.
//
// Kind selects the publication mode: template (or empty) requires Key and
// writes the index; checkpoint requires an empty Key and writes no index
// (the caller addresses the returned manifestRef + artifactDigest).
type PublishImageRequest struct {
	Identity
	Kind string `json:"kind,omitempty"`
	Key  string `json:"key,omitempty"`
	Dir  string `json:"dir"`
}

// PublishImageResponse reports the published manifest reference and digest.
type PublishImageResponse struct {
	ManifestRef    string `json:"manifestRef"`
	ArtifactDigest string `json:"artifactDigest"`
}

// CompatibilityResponse returns the node compatibility class (stage 3
// restore validation; a placeholder in the native stage).
type CompatibilityResponse struct {
	CompatibilityClass string `json:"compatibilityClass"`
}

// HealthResponse reports the agent's runtime health.
type HealthResponse struct {
	OK         bool  `json:"ok"`
	CacheBytes int64 `json:"cacheBytes"`
	LeaseCount int   `json:"leaseCount"`
	PinCount   int   `json:"pinCount"`
	ImageCount int   `json:"imageCount"`
	// DartUp reports whether the node-local DART P2P daemon answered its
	// last admin-plane probe. It is informational: the agent's own health
	// never depends on DART (a broken gateway keeps artifact pulls on the
	// direct S3 fallback path).
	DartUp bool `json:"dartUp"`
	// HostReady reports the last node-readiness check outcome (the KVM/
	// storage/asset check behind the scheduling labels). Nil when the
	// readiness manager is not running (no node name configured).
	HostReady *bool `json:"hostReady,omitempty"`
	// HostState is the readiness summary line ("3 checks: 2 pass, 1
	// warn, 0 fail"), or "check pending" before the first pass.
	HostState string `json:"hostState,omitempty"`
}
