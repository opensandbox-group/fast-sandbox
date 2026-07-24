package network

import (
	"context"
	"errors"
	"time"

	dataplane "fast-sandbox/internal/dataplane/contract"
)

var (
	ErrNoCleanSlot       = errors.New("no clean network slot is available")
	ErrSlotNotFound      = errors.New("network slot not found")
	ErrOwnerConflict     = errors.New("network slot is bound to another sandbox identity")
	ErrStateInconsistent = errors.New("network state is inconsistent with runtime state")
)

type SlotPhase string

const (
	SlotPhaseClean      SlotPhase = "Clean"
	SlotPhaseBound      SlotPhase = "Bound"
	SlotPhaseDestroying SlotPhase = "Destroying"
)

type AccessKind = dataplane.AccessKind

const (
	AccessKindDirectIP     = dataplane.AccessKindDirectIP
	AccessKindLocalForward = dataplane.AccessKindLocalForward
)

// Owner fences a network binding with the same identity used by Fastlet
// admission. A stale create/delete cannot take over a newer assignment.
type Owner struct {
	SandboxUID         string `json:"sandboxUid"`
	SandboxName        string `json:"sandboxName,omitempty"`
	SandboxNamespace   string `json:"sandboxNamespace,omitempty"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	RuntimeInstanceID  string `json:"runtimeInstanceId,omitempty"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
}

func (o Owner) Equal(other Owner) bool {
	return o.SandboxUID == other.SandboxUID &&
		o.InstanceGeneration == other.InstanceGeneration &&
		o.RuntimeInstanceID == other.RuntimeInstanceID &&
		o.AssignmentAttempt == other.AssignmentAttempt
}

type AccessDescriptor = dataplane.AccessDescriptor

// Slot is the durable description of one prepared Linux network namespace.
// HostNetNSPath is consumed by host containerd; NetNSPath is the path visible
// inside the Fastlet container for lifecycle operations.
type Slot struct {
	Version        int              `json:"version"`
	ID             string           `json:"id"`
	OwnerPodUID    string           `json:"ownerPodUid"`
	OwnerPodName   string           `json:"ownerPodName,omitempty"`
	OwnerNamespace string           `json:"ownerNamespace,omitempty"`
	Phase          SlotPhase        `json:"phase"`
	Owner          Owner            `json:"owner,omitempty"`
	NetNSName      string           `json:"netnsName"`
	NetNSPath      string           `json:"netnsPath"`
	HostNetNSPath  string           `json:"hostNetnsPath"`
	HostVeth       string           `json:"hostVeth"`
	PeerVeth       string           `json:"peerVeth"`
	Bridge         string           `json:"bridge"`
	Address        string           `json:"address"`
	IP             string           `json:"ip"`
	Gateway        string           `json:"gateway"`
	PrivateCIDR    string           `json:"privateCidr"`
	DNSPath        string           `json:"dnsPath"`
	MTU            int              `json:"mtu"`
	EgressDevice   string           `json:"egressDevice"`
	Access         AccessDescriptor `json:"access"`
	CreatedAt      time.Time        `json:"createdAt"`
	BoundAt        *time.Time       `json:"boundAt,omitempty"`
}

type Driver interface {
	Prepare(ctx context.Context, slot *Slot) error
	Validate(ctx context.Context, slot *Slot) error
	Destroy(ctx context.Context, slot *Slot) error
}

type StateStore interface {
	LoadAll(ctx context.Context) ([]*Slot, error)
	Save(ctx context.Context, slot *Slot) error
	Delete(ctx context.Context, slotID string) error
}

type Snapshot struct {
	Capacity   int
	Clean      int
	Bound      int
	Destroying int
	Hit        uint64
	Miss       uint64
}
