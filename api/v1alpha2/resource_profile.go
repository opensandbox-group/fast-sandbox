package v1alpha2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

var ErrInvalidSandboxResourceProfile = errors.New("invalid sandbox resource profile")

type SandboxResourceMode string

const (
	SandboxResourceModeInvalid       SandboxResourceMode = ""
	SandboxResourceModeLegacySlot    SandboxResourceMode = "LegacySlot"
	SandboxResourceModeResourceAware SandboxResourceMode = "ResourceAware"
)

// NewLegacySandboxResourceProfile constructs the flat resource limit shape
// used by existing SandboxPool clients.
func NewLegacySandboxResourceProfile(cpu, memory resource.Quantity, pids int64) SandboxResourceProfile {
	return SandboxResourceProfile{CPU: &cpu, Memory: &memory, PIDs: pids}
}

// ResourceMode reports which mutually-exclusive API representation is used.
func (p SandboxResourceProfile) ResourceMode() SandboxResourceMode {
	legacy := p.CPU != nil || p.Memory != nil
	resourceAware := p.Requests != nil || p.Limits != nil
	switch {
	case legacy && !resourceAware && p.CPU != nil && p.Memory != nil:
		return SandboxResourceModeLegacySlot
	case resourceAware && !legacy && p.Requests != nil && p.Limits != nil:
		return SandboxResourceModeResourceAware
	default:
		return SandboxResourceModeInvalid
	}
}

// LimitResources returns the per-Sandbox runtime isolation limits.
func (p SandboxResourceProfile) LimitResources() (SandboxComputeResources, bool) {
	switch p.ResourceMode() {
	case SandboxResourceModeLegacySlot:
		return SandboxComputeResources{CPU: p.CPU.DeepCopy(), Memory: p.Memory.DeepCopy()}, true
	case SandboxResourceModeResourceAware:
		return SandboxComputeResources{CPU: p.Limits.CPU.DeepCopy(), Memory: p.Limits.Memory.DeepCopy()}, true
	default:
		return SandboxComputeResources{}, false
	}
}

// RequestResources returns the per-Sandbox commitment demand. Legacy profiles
// deliberately have no inferred request.
func (p SandboxResourceProfile) RequestResources() (SandboxComputeResources, bool) {
	if p.ResourceMode() != SandboxResourceModeResourceAware {
		return SandboxComputeResources{}, false
	}
	return SandboxComputeResources{CPU: p.Requests.CPU.DeepCopy(), Memory: p.Requests.Memory.DeepCopy()}, true
}

// ValidateSandboxResourceProfile verifies either the legacy flat limit shape or
// the resource-aware requests/limits shape required by every SandboxPool.
func ValidateSandboxResourceProfile(p SandboxResourceProfile) error {
	if p.PIDs <= 0 {
		return fmt.Errorf("%w: pids must be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	limits, ok := p.LimitResources()
	if !ok {
		return fmt.Errorf("%w: exactly one of legacy cpu/memory or requests/limits must be specified", ErrInvalidSandboxResourceProfile)
	}
	if limits.CPU.Sign() <= 0 || limits.Memory.Sign() <= 0 {
		return fmt.Errorf("%w: cpu and memory limits must be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	if limits.CPU.MilliValue() < 10 {
		return fmt.Errorf("%w: cpu limit must be at least 10m for Linux CFS enforcement", ErrInvalidSandboxResourceProfile)
	}
	if p.ResourceMode() == SandboxResourceModeLegacySlot {
		return nil
	}
	requests, _ := p.RequestResources()
	if requests.CPU.Sign() <= 0 || requests.Memory.Sign() <= 0 {
		return fmt.Errorf("%w: cpu and memory requests must be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	if requests.CPU.Cmp(limits.CPU) > 0 || requests.Memory.Cmp(limits.Memory) > 0 {
		return fmt.Errorf("%w: requests must not exceed limits", ErrInvalidSandboxResourceProfile)
	}
	return nil
}

// Hash returns the canonical identity carried from SandboxPool through the
// control protocol to Fastlet admission.
func (p SandboxResourceProfile) Hash() string {
	var payload []byte
	var err error
	switch p.ResourceMode() {
	case SandboxResourceModeLegacySlot:
		// Preserve the exact historical payload so upgrading the controller does
		// not roll or fence every existing Pool.
		payload, err = json.Marshal(struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
			PIDs   int64  `json:"pids"`
		}{CPU: p.CPU.String(), Memory: p.Memory.String(), PIDs: p.PIDs})
	case SandboxResourceModeResourceAware:
		payload, err = json.Marshal(struct {
			Version  string `json:"version"`
			Requests struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"requests"`
			Limits struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"limits"`
			PIDs int64 `json:"pids"`
		}{
			Version: "resource-aware-v1",
			Requests: struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			}{CPU: p.Requests.CPU.String(), Memory: p.Requests.Memory.String()},
			Limits: struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			}{CPU: p.Limits.CPU.String(), Memory: p.Limits.Memory.String()},
			PIDs: p.PIDs,
		})
	default:
		panic(ErrInvalidSandboxResourceProfile)
	}
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
