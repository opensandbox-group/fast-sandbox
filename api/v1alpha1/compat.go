package v1alpha1

import (
	"errors"
	"fmt"
)

const InitialInstanceGeneration int64 = 1

var (
	ErrRuntimeFieldConflict = errors.New("spec.runtime cannot be combined with deprecated runtime fields")
	ErrRuntimeImmutable     = errors.New("spec.runtime is immutable")
	ErrResourcesImmutable   = errors.New("spec.sandboxResources is immutable")
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

// EffectiveRuntime resolves a Pool's canonical runtime while retaining a
// bounded read path for objects written with the deprecated fields.
func (s *SandboxPoolSpec) EffectiveRuntime() (RuntimeName, error) {
	if s.Runtime != "" {
		if s.RuntimeType != "" || s.RuntimeClassName != "" || s.ContainerdRuntimeHandler != "" {
			return "", ErrRuntimeFieldConflict
		}
		if !IsRuntimeName(s.Runtime) {
			return "", fmt.Errorf("unsupported runtime %q", s.Runtime)
		}
		return s.Runtime, nil
	}

	legacy := s.RuntimeType
	if legacy == "" {
		legacy = RuntimeContainer
	}
	if !IsRuntimeName(legacy) {
		return "", fmt.Errorf("unsupported legacy runtime %q", legacy)
	}
	return legacy, nil
}

// ValidateSandboxPoolUpdate enforces the immutable scheduling and resource
// boundary. It is shared by webhook/controller tests and migration tooling.
func ValidateSandboxPoolUpdate(oldSpec, newSpec *SandboxPoolSpec) error {
	oldRuntime, err := oldSpec.EffectiveRuntime()
	if err != nil {
		return fmt.Errorf("old pool runtime: %w", err)
	}
	newRuntime, err := newSpec.EffectiveRuntime()
	if err != nil {
		return fmt.Errorf("new pool runtime: %w", err)
	}
	if oldRuntime != newRuntime {
		return ErrRuntimeImmutable
	}
	if oldSpec.SandboxResources.CPU.Cmp(newSpec.SandboxResources.CPU) != 0 ||
		oldSpec.SandboxResources.Memory.Cmp(newSpec.SandboxResources.Memory) != 0 ||
		oldSpec.SandboxResources.PIDs != newSpec.SandboxResources.PIDs {
		return ErrResourcesImmutable
	}
	return nil
}

// NextInstanceGeneration advances a generation fence and initializes legacy
// zero-valued objects at generation one.
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
