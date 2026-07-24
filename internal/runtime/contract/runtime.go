// Package contract defines the runtime-neutral lifecycle boundary consumed by
// Fastlet. Runtime implementations deliberately exclude exec, file, and proxy
// protocols from this interface.
package contract

import (
	"context"
	"fmt"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	infracontract "fast-sandbox/internal/infra/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type Metadata struct {
	fastletapi.SandboxSpec
	ContainerID                string
	PID                        int
	Phase                      string
	CreatedAt                  int64
	UserProcessStartedAt       time.Time
	UserProcessStartSource     fastletapi.UserProcessStartSource
	InfraServices              []infracontract.ServiceEndpoint
	InfraUpstreamHeadersByPort map[uint32]map[string]string
	InfraDiagnostics           []infracontract.ComponentDiagnostic
}

type Driver interface {
	Initialize(ctx context.Context, socketPath string) error
	SetNamespace(ns string)
	ProbeCapabilities(ctx context.Context) CapabilityReport
	EnsureSandbox(ctx context.Context, config *fastletapi.SandboxSpec) (*Metadata, error)
	InspectSandbox(ctx context.Context, sandboxID string) (*Metadata, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	ListManagedSandboxes(ctx context.Context) ([]*Metadata, error)
	Close() error
}

type ArtifactCache interface {
	ListImages(ctx context.Context) ([]string, error)
	PullImage(ctx context.Context, image string) error
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
	Handler     string
	RuntimePath string
	ConfigPath  string
	NeedsTTY    bool
	OptionsType string
}

type CapabilityReport struct {
	Runtime     apiv1alpha1.RuntimeName        `json:"runtime"`
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

func ValidateProfile(existing *Metadata, requested *fastletapi.SandboxSpec) error {
	if existing == nil || requested == nil {
		return fmt.Errorf("%w: existing and requested runtime specs are required", ErrSandboxProfileMismatch)
	}
	if existing.RuntimeProfileHash != requested.RuntimeProfileHash ||
		existing.ResourceProfileHash != requested.ResourceProfileHash ||
		existing.InfraProfile != requested.InfraProfile || existing.InfraProfileHash != requested.InfraProfileHash ||
		existing.CPU != requested.CPU || existing.Memory != requested.Memory || existing.PIDs != requested.PIDs {
		return fmt.Errorf("%w: existing runtime identity %q has different runtime/resource profile", ErrSandboxProfileMismatch, requested.SandboxID)
	}
	return nil
}
