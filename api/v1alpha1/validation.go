package v1alpha1

import (
	"errors"
	"fmt"
)

const InitialInstanceGeneration int64 = 1

var (
	ErrRuntimeImmutable   = errors.New("spec.runtime is immutable")
	ErrResourcesImmutable = errors.New("spec.sandboxResources is immutable")
)

// IsRuntimeName reports whether name identifies a built-in runtime profile.
func IsRuntimeName(name RuntimeName) bool {
	switch name {
	case RuntimeContainer, RuntimeGVisor, RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc, RuntimeBoxLite:
		return true
	default:
		return false
	}
}

// ValidateRuntime verifies that the Pool selects one built-in runtime profile.
func (s *SandboxPoolSpec) ValidateRuntime() error {
	if !IsRuntimeName(s.Runtime) {
		return fmt.Errorf("unsupported runtime %q", s.Runtime)
	}
	return nil
}

// ValidateSandboxPoolUpdate enforces the immutable scheduling and resource
// boundary shared by admission and reconciliation.
func ValidateSandboxPoolUpdate(oldSpec, newSpec *SandboxPoolSpec) error {
	if err := oldSpec.ValidateRuntime(); err != nil {
		return fmt.Errorf("old pool runtime: %w", err)
	}
	if err := newSpec.ValidateRuntime(); err != nil {
		return fmt.Errorf("new pool runtime: %w", err)
	}
	if oldSpec.Runtime != newSpec.Runtime {
		return ErrRuntimeImmutable
	}
	if oldSpec.SandboxResources.CPU.Cmp(newSpec.SandboxResources.CPU) != 0 ||
		oldSpec.SandboxResources.Memory.Cmp(newSpec.SandboxResources.Memory) != 0 ||
		oldSpec.SandboxResources.PIDs != newSpec.SandboxResources.PIDs {
		return ErrResourcesImmutable
	}
	return nil
}

// NextInstanceGeneration advances a generation fence. A newly created Sandbox
// has no status yet, so its first runtime instance starts at generation one.
func NextInstanceGeneration(current int64) int64 {
	if current < InitialInstanceGeneration {
		return InitialInstanceGeneration
	}
	return current + 1
}

// Validate verifies the assignment identity required for fencing.
func (a *SandboxAssignment) Validate() error {
	if a == nil {
		return errors.New("assignment is required")
	}
	if a.FastletName == "" {
		return errors.New("fastletName is required")
	}
	if a.FastletPodUID == "" {
		return errors.New("fastletPodUID is required")
	}
	if a.Attempt < 1 {
		return errors.New("attempt must be at least 1")
	}
	return nil
}
