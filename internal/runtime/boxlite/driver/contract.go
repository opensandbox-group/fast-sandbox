package driver

import runtimecontract "fast-sandbox/internal/runtime/contract"

type SandboxMetadata = runtimecontract.Metadata
type CapabilityReport = runtimecontract.CapabilityReport
type RuntimeDriver = runtimecontract.Driver
type RuntimeArtifactCache = runtimecontract.ArtifactCache
type AccessDescriptorProvider = runtimecontract.AccessDescriptorProvider

var (
	ErrSandboxNotFound        = runtimecontract.ErrSandboxNotFound
	ErrSandboxAlreadyExists   = runtimecontract.ErrSandboxAlreadyExists
	ErrRuntimeNotInitialized  = runtimecontract.ErrRuntimeNotInitialized
	ErrNetworkUnavailable     = runtimecontract.ErrNetworkUnavailable
	ErrInfraUnavailable       = runtimecontract.ErrInfraUnavailable
	ErrSandboxProfileMismatch = runtimecontract.ErrSandboxProfileMismatch
	ErrInvalidConfig          = runtimecontract.ErrInvalidConfig
)

var validateExistingRuntimeProfile = runtimecontract.ValidateProfile
