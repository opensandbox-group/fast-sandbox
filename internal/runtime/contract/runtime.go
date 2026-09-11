// Package contract defines the runtime-neutral lifecycle boundary consumed by
// Fastlet. Runtime implementations deliberately exclude exec, file, and proxy
// protocols from this interface.
package contract

import (
	"context"
	"fmt"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	infracontract "fast-sandbox/internal/infra/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type Metadata struct {
	Config                 fastletapi.RuntimeSandboxConfig
	Allocation             fastletapi.RuntimeAllocation
	ContainerID            string
	PID                    int
	Phase                  string
	CreatedAt              int64
	UserProcessStartedAt   time.Time
	UserProcessStartSource fastletapi.UserProcessStartSource
	InfraServices          []infracontract.ServiceEndpoint
	InfraDiagnostics       []infracontract.ComponentDiagnostic
	AcceptedGeneration     int64
	AppliedGeneration      int64
	ActionBindingStatuses  []fastletapi.ActionBindingStatus
}

type Driver interface {
	Initialize(ctx context.Context, socketPath string) error
	SetNamespace(ns string)
	ProbeCapabilities(ctx context.Context) CapabilityReport
	EnsureSandbox(ctx context.Context, input *fastletapi.EnsureSandboxInput) (*Metadata, error)
	InspectSandbox(ctx context.Context, sandboxID string) (*Metadata, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	ListManagedSandboxes(ctx context.Context) ([]*Metadata, error)
	Close() error
}

type ArtifactCache interface {
	ListImages(ctx context.Context) ([]string, error)
	PullImage(ctx context.Context, image string) error
}

// ImageDeliveryStatus reports the delivery state of an image that is not
// (yet) present in the local cache.
type ImageDeliveryStatus string

const (
	// ImageDelivering reports that the artifact delivery is in flight on the
	// node; the caller must poll again later instead of blocking.
	ImageDelivering ImageDeliveryStatus = "Delivering"
	// ImageDelivered reports that the image is committed in the local cache
	// and the Sandbox can boot from it.
	ImageDelivered ImageDeliveryStatus = "Delivered"
)

// ImageDelivery is the optional runtime extension for asynchronous artifact
// delivery. Runtimes whose first boot can require a cold transfer of the
// Sandbox image (Firecracker) implement it so the create path never blocks on
// the network: a missing image parks the Sandbox in a delivery phase while a
// node-side pull runs in the background.
//
// DeliverImage guarantees that an attempt to deliver image is in flight (it is
// idempotent and safe to call concurrently) and reports the current state
// without waiting for the transfer to finish. A non-nil error means delivery
// is impossible right now (e.g. no runtime-agent in local mode) and the caller
// must not park; delivery failures that happen asynchronously are reported by
// a later call.
type ImageDelivery interface {
	DeliverImage(ctx context.Context, image string) (ImageDeliveryStatus, error)
}

// SnapshotResult reports the artifact set produced by one snapshot.
type SnapshotResult struct {
	// SnapshotID is the node-local identity of the snapshot; it scopes the
	// on-disk staging directory and any cleanup.
	SnapshotID string
	// ManifestRef locates the published manifest in the artifact store
	// (SandboxTemplate layout), e.g. s3://bucket/prefix/<digest>/manifest.json.
	ManifestRef string
	// ArtifactDigest is the sha256 of the published manifest document.
	ArtifactDigest string
	// SizeBytes is the total logical size of the artifact set.
	SizeBytes int64
}

// SnapshotActionBinding is one source-Sandbox action binding recorded in
// the published manifest: the durable policy record that outlives the
// SandboxSnapshot CR (the CR is only the in-cluster auto-apply path).
type SnapshotActionBinding struct {
	Handler string `json:"handler"`
	Input   string `json:"input"`
}

// SnapshotInput carries one snapshot request to the runtime driver.
type SnapshotInput struct {
	// SandboxID identifies the running Sandbox to snapshot.
	SandboxID string
	// SnapshotID is the node-local identity of the snapshot; it scopes the
	// staging directory and any cleanup.
	SnapshotID string
	// TemplateName is the artifact-store index key the artifact set is
	// published under.
	TemplateName string
	// OnPublishing is invoked once the pause window has closed and the
	// local artifact set is complete — right before the store upload
	// begins. The caller uses it to surface the Publishing phase, which no
	// longer touches the VM and therefore does not block the next snapshot
	// of the same Sandbox. Optional; drivers must tolerate nil.
	OnPublishing func()
	// ActionBindings are the source Sandbox's bindings, recorded verbatim
	// in the published manifest so the artifact set is self-contained.
	ActionBindings []SnapshotActionBinding
}

// Snapshotter is the optional runtime extension for snapshotting a running
// Sandbox in place. Runtimes that cannot snapshot (containerd, kata, boxlite)
// simply do not implement it; Fastlet then rejects the request with
// ErrSnapshotUnsupported instead of attempting a partial fallback.
//
// CreateSnapshot is one-shot per (SandboxID, SnapshotID) pair and blocking;
// it must resume the Sandbox on every in-process failure path. A host or
// Fastlet crash mid-dump can still leave the runtime paused — implementers
// that also implement ResourceRecoverer must resume such runtimes during
// RecoverRuntimeResources so recovery never strands a paused guest.
// DeleteSnapshot discards node-local artifacts of a previous snapshot; it
// never unpublishes stored objects.
type Snapshotter interface {
	CreateSnapshot(ctx context.Context, input *SnapshotInput) (*SnapshotResult, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) error
}

type ResourceRecoverer interface {
	RecoverRuntimeResources(ctx context.Context, managed []*Metadata) error
}

type ResourceAdmission interface {
	RuntimeResourceAvailable() bool
}

type AccessDescriptorProvider interface {
	GetAccessDescriptor(sandboxID string) (dataplane.AccessDescriptor, error)
}

type Config struct {
	Namespace   string
	Snapshotter string
	Handler     string
	RuntimePath string
	ConfigPath  string
	NeedsTTY    bool
	OptionsType string
}

type CapabilityReport struct {
	Runtime     apiv1alpha2.RuntimeName        `json:"runtime"`
	ProfileHash string                         `json:"profileHash"`
	State       runtimecatalog.CapabilityState `json:"state"`
	Reason      string                         `json:"reason,omitempty"`
	Message     string                         `json:"message,omitempty"`
	Missing     []string                       `json:"missing,omitempty"`
}

func (r CapabilityReport) Ready() bool {
	return r.State == runtimecatalog.CapabilityReady
}

type CapabilityProber interface {
	Probe(ctx context.Context, profile runtimecatalog.RuntimeProfile, socketPath string) CapabilityReport
}

func ValidateProfile(existing *Metadata, requested *fastletapi.RuntimeSandboxConfig) error {
	if existing == nil || requested == nil {
		return fmt.Errorf("%w: existing and requested runtime specs are required", ErrSandboxProfileMismatch)
	}
	if existing.Config.Spec.RuntimeProfileHash != requested.Spec.RuntimeProfileHash ||
		existing.Config.Spec.ResourceProfileHash != requested.Spec.ResourceProfileHash ||
		existing.Config.Spec.InfraRevision != requested.Spec.InfraRevision ||
		existing.Config.Spec.CPU != requested.Spec.CPU || existing.Config.Spec.Memory != requested.Spec.Memory || existing.Config.Spec.PIDs != requested.Spec.PIDs {
		return fmt.Errorf("%w: existing runtime identity %q has different runtime/resource profile", ErrSandboxProfileMismatch, requested.Identity.SandboxUID)
	}
	return nil
}

// SameRuntimeIdentity reports whether an observed runtime belongs to the exact
// Sandbox incarnation and placement represented by requested. Desired runtime
// configuration is intentionally excluded and validated separately.
func SameRuntimeIdentity(existing, requested fastletapi.SandboxIdentity) bool {
	return existing.SandboxUID == requested.SandboxUID &&
		existing.Namespace == requested.Namespace &&
		existing.Name == requested.Name &&
		existing.FastletPodUID == requested.FastletPodUID &&
		existing.InstanceGeneration == requested.InstanceGeneration &&
		existing.RuntimeInstanceID == requested.RuntimeInstanceID &&
		existing.AssignmentAttempt == requested.AssignmentAttempt &&
		existing.RouteGeneration == requested.RouteGeneration
}
