package containerd

import runtimecontract "fast-sandbox/internal/runtime/contract"

type SandboxMetadata = runtimecontract.Metadata
type RuntimeConfig = runtimecontract.Config
type CapabilityReport = runtimecontract.CapabilityReport

var (
	ErrSandboxNotFound        = runtimecontract.ErrSandboxNotFound
	ErrRuntimeNotInitialized  = runtimecontract.ErrRuntimeNotInitialized
	ErrNetworkUnavailable     = runtimecontract.ErrNetworkUnavailable
	ErrSandboxProfileMismatch = runtimecontract.ErrSandboxProfileMismatch
)
