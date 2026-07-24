package factory

import runtimecontract "fast-sandbox/internal/runtime/contract"

type CapabilityReport = runtimecontract.CapabilityReport
type CapabilityProber = runtimecontract.CapabilityProber
type RuntimeDriver = runtimecontract.Driver

var (
	ErrUnsupportedRuntime           = runtimecontract.ErrUnsupportedRuntime
	ErrRuntimeCapabilityUnavailable = runtimecontract.ErrRuntimeCapabilityUnavailable
)
