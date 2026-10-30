// Package action defines the versioned Pod-local Action Handler protocol.
package action

const APIVersion = "sandbox.fast.io/actions/v1"
const APIVersionV2 = "sandbox.fast.io/actions/v2"

// HeaderSandboxUID is the trusted sandbox identity header the Fastlet
// injects on relayed handler requests. The relayed method/path/body are
// caller-controlled, so Handlers must scope per-subject state by this
// platform-endorsed value (fence-validated by the Fastlet) instead of any
// caller-controlled path segment.
const HeaderSandboxUID = "X-Sandbox-Uid"
const HeaderOwner = "X-Fast-Sandbox-Owner"

const (
	OwnerStateCurrent = "current"
	OwnerStateRetired = "retired"
	OwnerStateAbsent  = "absent"
)

type Operation string

const (
	OperationSetBinding    Operation = "SET_BINDING"
	OperationLifecycleHook Operation = "LIFECYCLE_HOOK"
	OperationRemoveBinding Operation = "REMOVE_BINDING"
	// Vault snapshot transfer across pause/resume (OSEP-0022 "Vault
	// Lifecycle"): export copies the subject's vault file into the snapshot
	// bundle strictly before the runtime teardown; import lands the
	// re-stamped bundle file before data-plane-ready at restore — the only
	// authorized incarnation transfer. Driver-invoked via the Fastlet.
	OperationVaultExport Operation = "VAULT_EXPORT"
	OperationVaultImport Operation = "VAULT_IMPORT"
	// Policy snapshot transfer across pause/resume: export copies the
	// subject's recorded network policy file into the snapshot bundle
	// strictly before the runtime teardown (for checkpoint AND template
	// snapshots — the policy is desired state, not a secret, so a template
	// clone inherits it); import lands the re-stamped bundle file before
	// data-plane-ready at restore, and the imported record stays
	// authoritative over later binding replays. The envelope shape is the
	// vault transfer's (SnapshotTransfer{Path}, registered fence), served by
	// the same egress Handler.
	OperationPolicyExport Operation = "POLICY_EXPORT"
	OperationPolicyImport Operation = "POLICY_IMPORT"
)

type LifecycleHook string

const (
	LifecycleHookRuntimeReady   LifecycleHook = "sandbox.runtime-ready"
	LifecycleHookDataPlaneReady LifecycleHook = "sandbox.data-plane-ready"
)

type SandboxIdentity struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type Revision struct {
	SpecGeneration    int64  `json:"specGeneration"`
	RuntimeInstanceID string `json:"runtimeInstanceId,omitempty"`
	AttachmentID      string `json:"attachmentId,omitempty"`
	RouteGeneration   int64  `json:"routeGeneration,omitempty"`
}

// Owner is the full platform-validated runtime fence carried on Actions v2.
// It fences delayed relay and lifecycle calls but is not caller authentication.
type Owner struct {
	SandboxUID         string `json:"sandboxUid"`
	FastletPodUID      string `json:"fastletPodUid"`
	RuntimeInstanceID  string `json:"runtimeInstanceId"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
	RouteGeneration    int64  `json:"routeGeneration"`
	AttachmentID       string `json:"attachmentId"`
}

func (o Owner) Valid() bool {
	return o.SandboxUID != "" && o.FastletPodUID != "" && o.RuntimeInstanceID != "" &&
		o.InstanceGeneration > 0 && o.AssignmentAttempt > 0 && o.RouteGeneration > 0 && o.AttachmentID != ""
}

func (o Owner) Matches(other Owner) bool { return o == other }

type OwnerObservation struct {
	State string `json:"state"`
	Owner Owner  `json:"owner"`
}

type NetworkAttachment struct {
	IP          string `json:"ip,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	PrivateCIDR string `json:"privateCidr,omitempty"`
	HostVeth    string `json:"hostVeth,omitempty"`
}

type Attachment struct {
	Network NetworkAttachment `json:"network"`
}

// BindingPayload uses pointer presence deliberately. A non-nil Input sets the
// exact opaque value (including "" or the literal "null"); nil removes a
// Binding from a still-live Sandbox.
type BindingPayload struct {
	Input *string `json:"input"`
}

type LifecycleHookPayload struct {
	Name     LifecycleHook `json:"name"`
	Sequence int64         `json:"sequence"`
}

// SnapshotTransfer names the driver-provided staging file for one subject's
// state snapshot: the export target inside the snapshot bundle, or the
// bundle source at restore. One file per invocation; the path is Pod-local
// and validated by the Handler (absolute, no parent traversal). The bytes
// never cross the envelope: the Handler copies/reads the file itself.
type SnapshotTransfer struct {
	Path string `json:"path"`
	// Seal governs the subject latch on export (absent = sealed, the
	// checkpoint semantics): a sealed subject answers its relay surface
	// with 409 until an import re-owns the record — the pause window must
	// stay consistent. Template carries export with seal=false: the
	// template's completion path never imports, so sealing would lock the
	// still-running source out of its state CRUD; the record is copied
	// verbatim and the source keeps mutating. Handlers without the
	// template-carry capability ignore the field and always seal — callers
	// gate seal=false on the HandlerStatus.TemplateStateExport flag.
	Seal *bool `json:"seal,omitempty"`
}

type Request struct {
	Mode         string                `json:"mode,omitempty"`
	APIVersion   string                `json:"apiVersion"`
	Operation    Operation             `json:"operation"`
	InvocationID string                `json:"invocationId"`
	Sandbox      SandboxIdentity       `json:"sandbox"`
	Revision     Revision              `json:"revision"`
	Owner        *Owner                `json:"owner,omitempty"`
	Attachment   Attachment            `json:"attachment"`
	Binding      *BindingPayload       `json:"binding,omitempty"`
	Hook         *LifecycleHookPayload `json:"hook,omitempty"`
	Snapshot     *SnapshotTransfer     `json:"snapshot,omitempty"`
}

type HandlerStatus struct {
	APIVersion  string `json:"apiVersion"`
	Ready       bool   `json:"ready"`
	InstanceID  string `json:"instanceId"`
	Message     string `json:"message,omitempty"`
	OwnerStatus bool   `json:"ownerStatus,omitempty"`
	// VaultTransfer reports that the Handler can carry a subject's vault
	// file across pause/resume (VAULT_EXPORT / VAULT_IMPORT): it serves the
	// vault operations and persists vault files. Absent (older Handler
	// builds) decodes as false: vault transfer is skipped and the
	// checkpoint proceeds without a vault — the pre-transfer semantics.
	VaultTransfer bool `json:"vaultTransfer,omitempty"`
	// PolicyTransfer reports that the Handler can carry a subject's
	// recorded network policy file across pause/resume (POLICY_EXPORT /
	// POLICY_IMPORT): it serves the policy operations and persists policy
	// files. Absent (older Handler builds) decodes as false: policy
	// transfer is skipped and the snapshot records the CRD binding input
	// only — the pre-transfer semantics, where SET_BINDING is the only
	// policy channel.
	PolicyTransfer bool `json:"policyTransfer,omitempty"`
	// TemplateStateExport reports that the Handler supports the TEMPLATE
	// state-carry semantics on top of the transfer operations: seal-free
	// exports (SnapshotTransfer.Seal=false — the source keeps its relay
	// CRUD; a template's completion path never imports) and imports that
	// land a template-borne record for the NEXT registration of a fresh
	// subject (a clone boots with the template's state). Absent decodes as
	// false: template snapshots export nothing and clones carry no
	// template-borne state.
	TemplateStateExport bool `json:"templateStateExport,omitempty"`
}
