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
type ImageDelivery = runtimecontract.ImageDelivery
type ImageDeliveryStatus = runtimecontract.ImageDeliveryStatus
type RuntimeSnapshotter = runtimecontract.Snapshotter
type SnapshotResult = runtimecontract.SnapshotResult
type RuntimeSnapshotInput = runtimecontract.SnapshotInput

const (
	ImageDelivering = runtimecontract.ImageDelivering
	ImageDelivered  = runtimecontract.ImageDelivered
)

var (
	ErrUnsupportedRuntime     = runtimecontract.ErrUnsupportedRuntime
	ErrSandboxNotFound        = runtimecontract.ErrSandboxNotFound
	ErrRuntimeNotInitialized  = runtimecontract.ErrRuntimeNotInitialized
	ErrNetworkUnavailable     = runtimecontract.ErrNetworkUnavailable
	ErrInfraUnavailable       = runtimecontract.ErrInfraUnavailable
	ErrSandboxProfileMismatch = runtimecontract.ErrSandboxProfileMismatch
	ErrInvalidConfig          = runtimecontract.ErrInvalidConfig
	ErrImageNotReady          = runtimecontract.ErrImageNotReady
	ErrSnapshotUnsupported    = runtimecontract.ErrSnapshotUnsupported
	ErrInsufficientStorage    = runtimecontract.ErrInsufficientStorage
)
