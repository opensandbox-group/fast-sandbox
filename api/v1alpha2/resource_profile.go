package v1alpha2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

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

// NewResourceAwareSandboxResourceProfile constructs a profile whose flat CPU
// and memory values are default runtime limits and whose requests are derived.
func NewResourceAwareSandboxResourceProfile(
	cpuLimit, memoryLimit resource.Quantity,
	cpuRatio, memoryRatio resource.Quantity,
	pids int64,
) SandboxResourceProfile {
	return SandboxResourceProfile{
		Mode: SandboxResourceModeResourceAware,
		CPU:  &cpuLimit, Memory: &memoryLimit, PIDs: pids,
		OvercommitRatio: &SandboxOvercommitRatio{CPU: cpuRatio, Memory: memoryRatio},
	}
}

// ResourceMode reports the selected resource policy while preserving the
// historical mode-less flat representation as LegacySlot.
func (p SandboxResourceProfile) ResourceMode() SandboxResourceMode {
	if p.CPU == nil || p.Memory == nil {
		return SandboxResourceModeInvalid
	}
	switch p.Mode {
	case "", SandboxResourceModeLegacySlot:
		if p.OvercommitRatio != nil {
			return SandboxResourceModeInvalid
		}
		return SandboxResourceModeLegacySlot
	case SandboxResourceModeResourceAware:
		if p.OvercommitRatio == nil {
			return SandboxResourceModeInvalid
		}
		return SandboxResourceModeResourceAware
	default:
		return SandboxResourceModeInvalid
	}
}

// LimitResources returns the per-Sandbox runtime isolation limits.
func (p SandboxResourceProfile) LimitResources() (SandboxComputeResources, bool) {
	if p.ResourceMode() == SandboxResourceModeInvalid {
		return SandboxComputeResources{}, false
	}
	return SandboxComputeResources{CPU: p.CPU.DeepCopy(), Memory: p.Memory.DeepCopy()}, true
}

// RequestResources returns the default per-Sandbox commitment demand. Legacy
// profiles deliberately have no inferred request.
func (p SandboxResourceProfile) RequestResources() (SandboxComputeResources, bool) {
	if p.ResourceMode() != SandboxResourceModeResourceAware {
		return SandboxComputeResources{}, false
	}
	limits, _ := p.LimitResources()
	requests, err := p.DeriveRequests(limits)
	return requests, err == nil
}

// ResolveLimits selects the caller override or the Pool defaults and validates
// that the resulting runtime boundary can be represented by the platform.
func (p SandboxResourceProfile) ResolveLimits(override *SandboxResourceRequirements) (SandboxComputeResources, error) {
	limits, ok := p.LimitResources()
	if !ok {
		return SandboxComputeResources{}, ErrInvalidSandboxResourceProfile
	}
	if override != nil {
		if p.ResourceMode() != SandboxResourceModeResourceAware {
			return SandboxComputeResources{}, fmt.Errorf("%w: per-Sandbox resources require ResourceAware mode", ErrInvalidSandboxResourceProfile)
		}
		limits = SandboxComputeResources{CPU: override.Limits.CPU.DeepCopy(), Memory: override.Limits.Memory.DeepCopy()}
	}
	if limits.CPU.Sign() <= 0 || limits.Memory.Sign() <= 0 {
		return SandboxComputeResources{}, fmt.Errorf("%w: cpu and memory limits must be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	if limits.CPU.MilliValue() < 10 {
		return SandboxComputeResources{}, fmt.Errorf("%w: cpu limit must be at least 10m for Linux CFS enforcement", ErrInvalidSandboxResourceProfile)
	}
	return limits, nil
}

// DeriveRequests converts runtime limits into commitment requests with exact
// rational arithmetic and rounds up to a whole millicore/byte.
func (p SandboxResourceProfile) DeriveRequests(limits SandboxComputeResources) (SandboxComputeResources, error) {
	if p.ResourceMode() != SandboxResourceModeResourceAware || p.OvercommitRatio == nil {
		return SandboxComputeResources{}, fmt.Errorf("%w: ResourceAware overcommit policy is required", ErrInvalidSandboxResourceProfile)
	}
	cpuMilli, err := divideRoundUp(limits.CPU.MilliValue(), p.OvercommitRatio.CPU)
	if err != nil {
		return SandboxComputeResources{}, fmt.Errorf("derive cpu request: %w", err)
	}
	memoryBytes, err := divideRoundUp(limits.Memory.Value(), p.OvercommitRatio.Memory)
	if err != nil {
		return SandboxComputeResources{}, fmt.Errorf("derive memory request: %w", err)
	}
	return SandboxComputeResources{
		CPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
		Memory: *resource.NewQuantity(memoryBytes, resource.BinarySI),
	}, nil
}

// ResolveFastletCapacity derives the one commitment budget and runtime-owner
// envelope shared by Controller, FastPath, Fastlet, and Pool scaling.
func (p SandboxResourceProfile) ResolveFastletCapacity(maxSandboxes int32) (SandboxComputeResources, SandboxComputeResources, error) {
	if maxSandboxes <= 0 {
		return SandboxComputeResources{}, SandboxComputeResources{}, fmt.Errorf("maxSandboxesPerPod must be greater than zero")
	}
	requests, ok := p.RequestResources()
	if !ok {
		return SandboxComputeResources{}, SandboxComputeResources{}, fmt.Errorf("%w: ResourceAware requests cannot be derived", ErrInvalidSandboxResourceProfile)
	}
	limits, _ := p.LimitResources()
	capacity := SandboxComputeResources{CPU: requests.CPU.DeepCopy(), Memory: requests.Memory.DeepCopy()}
	capacity.CPU.Mul(int64(maxSandboxes))
	capacity.Memory.Mul(int64(maxSandboxes))
	envelope := SandboxComputeResources{CPU: capacity.CPU.DeepCopy(), Memory: capacity.Memory.DeepCopy()}
	if envelope.CPU.Cmp(limits.CPU) < 0 {
		envelope.CPU = limits.CPU.DeepCopy()
	}
	if envelope.Memory.Cmp(limits.Memory) < 0 {
		envelope.Memory = limits.Memory.DeepCopy()
	}
	return capacity, envelope, nil
}

func divideRoundUp(value int64, ratio resource.Quantity) (int64, error) {
	if value <= 0 || ratio.Sign() <= 0 {
		return 0, fmt.Errorf("value and ratio must be greater than zero")
	}
	ratioValue, ok := new(big.Rat).SetString(ratio.AsDec().String())
	if !ok || ratioValue.Sign() <= 0 {
		return 0, fmt.Errorf("invalid ratio %q", ratio.String())
	}
	quotient := new(big.Rat).Quo(new(big.Rat).SetInt64(value), ratioValue)
	numerator, denominator := quotient.Num(), quotient.Denom()
	result := new(big.Int).Quo(numerator, denominator)
	if new(big.Int).Mod(numerator, denominator).Sign() != 0 {
		result.Add(result, big.NewInt(1))
	}
	if !result.IsInt64() || result.Sign() <= 0 {
		return 0, fmt.Errorf("derived request is outside int64 range")
	}
	return result.Int64(), nil
}

// ValidateSandboxResourceProfile verifies either the legacy flat limit shape or
// the ResourceAware default limits and overcommit policy required by a Pool.
func ValidateSandboxResourceProfile(p SandboxResourceProfile) error {
	if p.PIDs <= 0 {
		return fmt.Errorf("%w: pids must be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	limits, ok := p.LimitResources()
	if !ok {
		return fmt.Errorf("%w: cpu and memory plus a valid mode-specific overcommit policy are required", ErrInvalidSandboxResourceProfile)
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
	if p.OvercommitRatio.CPU.Cmp(resource.MustParse("1")) < 0 || p.OvercommitRatio.Memory.Cmp(resource.MustParse("1")) < 0 {
		return fmt.Errorf("%w: cpu and memory overcommit ratios must be at least 1", ErrInvalidSandboxResourceProfile)
	}
	requests, err := p.DeriveRequests(limits)
	if err != nil {
		return fmt.Errorf("%w: cannot derive positive cpu and memory requests: %w", ErrInvalidSandboxResourceProfile, err)
	}
	if requests.CPU.Sign() <= 0 || requests.Memory.Sign() <= 0 {
		return fmt.Errorf("%w: cannot derive positive cpu and memory requests", ErrInvalidSandboxResourceProfile)
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
			Version string `json:"version"`
			Limits  struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"limits"`
			OvercommitRatio struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"overcommitRatio"`
			PIDs int64 `json:"pids"`
		}{
			Version: "resource-aware-v2",
			Limits: struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			}{CPU: p.CPU.String(), Memory: p.Memory.String()},
			OvercommitRatio: struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			}{CPU: p.OvercommitRatio.CPU.String(), Memory: p.OvercommitRatio.Memory.String()},
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
