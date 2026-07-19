package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

var ErrInvalidSandboxResourceProfile = errors.New("invalid sandbox resource profile")

func DefaultSandboxResourceProfile() SandboxResourceProfile {
	return SandboxResourceProfile{
		CPU: resource.MustParse("1"), Memory: resource.MustParse("512Mi"), PIDs: 256,
	}
}

// EffectiveSandboxResources gives legacy Pools that omitted the entire field a
// deterministic fixed profile. Partially specified or non-positive profiles
// fail closed so a Sandbox can never silently lose only one hard limit.
func (s SandboxPoolSpec) EffectiveSandboxResources() (SandboxResourceProfile, error) {
	p := s.SandboxResources
	if p.CPU.IsZero() && p.Memory.IsZero() && p.PIDs == 0 {
		return DefaultSandboxResourceProfile(), nil
	}
	if p.CPU.Sign() <= 0 || p.Memory.Sign() <= 0 || p.PIDs <= 0 {
		return SandboxResourceProfile{}, fmt.Errorf("%w: cpu, memory, and pids must all be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	if p.CPU.MilliValue() < 10 {
		return SandboxResourceProfile{}, fmt.Errorf("%w: cpu must be at least 10m for Linux CFS enforcement", ErrInvalidSandboxResourceProfile)
	}
	return p, nil
}

// Hash returns the canonical identity carried from SandboxPool through the
// control protocol to Fastlet admission.
func (p SandboxResourceProfile) Hash() string {
	payload, err := json.Marshal(struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
		PIDs   int64  `json:"pids"`
	}{CPU: p.CPU.String(), Memory: p.Memory.String(), PIDs: p.PIDs})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
