package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrInvalidSandboxResourceProfile = errors.New("invalid sandbox resource profile")

// ValidateSandboxResourceProfile verifies the fixed, Fastlet-enforced resource
// profile required by every SandboxPool.
func ValidateSandboxResourceProfile(p SandboxResourceProfile) error {
	if p.CPU.Sign() <= 0 || p.Memory.Sign() <= 0 || p.PIDs <= 0 {
		return fmt.Errorf("%w: cpu, memory, and pids must all be greater than zero", ErrInvalidSandboxResourceProfile)
	}
	if p.CPU.MilliValue() < 10 {
		return fmt.Errorf("%w: cpu must be at least 10m for Linux CFS enforcement", ErrInvalidSandboxResourceProfile)
	}
	return nil
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
