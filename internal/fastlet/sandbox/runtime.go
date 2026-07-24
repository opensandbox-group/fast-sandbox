package sandbox

import (
	dataplane "fast-sandbox/internal/dataplane/contract"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

type SandboxMetadata = runtimecontract.Metadata
type RuntimeDriver = runtimecontract.Driver
type RuntimeArtifactCache = runtimecontract.ArtifactCache
type RuntimeResourceRecoverer = runtimecontract.ResourceRecoverer
type RuntimeResourceAdmission = runtimecontract.ResourceAdmission
type AccessDescriptorProvider = runtimecontract.AccessDescriptorProvider
type CapabilityReport = runtimecontract.CapabilityReport
type RoutePublication = dataplane.RoutePublication
type RoutePublisher = dataplane.RoutePublisher

var (
	ErrUnsupportedRuntime     = runtimecontract.ErrUnsupportedRuntime
	ErrSandboxNotFound        = runtimecontract.ErrSandboxNotFound
	ErrRuntimeNotInitialized  = runtimecontract.ErrRuntimeNotInitialized
	ErrNetworkUnavailable     = runtimecontract.ErrNetworkUnavailable
	ErrInfraUnavailable       = runtimecontract.ErrInfraUnavailable
	ErrSandboxProfileMismatch = runtimecontract.ErrSandboxProfileMismatch
	ErrInvalidConfig          = runtimecontract.ErrInvalidConfig
)
