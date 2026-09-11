package contract

import "errors"

var (
	ErrUnsupportedRuntime           = errors.New("unsupported container runtime")
	ErrSandboxNotFound              = errors.New("sandbox not found")
	ErrSandboxAlreadyExists         = errors.New("sandbox already exists")
	ErrRuntimeNotInitialized        = errors.New("runtime not initialized")
	ErrRuntimeCapabilityUnavailable = errors.New("runtime capability unavailable")
	ErrNetworkUnavailable           = errors.New("sandbox network unavailable")
	ErrInfraUnavailable             = errors.New("sandbox Infra Components unavailable")
	ErrSandboxProfileMismatch       = errors.New("sandbox profile mismatch")
	ErrInvalidConfig                = errors.New("invalid sandbox config")
	// ErrImageNotReady reports that a rootfs image has not been converted
	// and cached yet. It is shared between the driver's local cache and the
	// firecracker runtime-agent pull layer so both can fail a create with
	// the same sentinel.
	ErrImageNotReady = errors.New("rootfs image is not ready in the local cache")
	// ErrSnapshotUnsupported reports that the runtime driver does not
	// implement the optional Snapshotter extension.
	ErrSnapshotUnsupported = errors.New("runtime does not support sandbox snapshots")
	// ErrInsufficientStorage reports that the node-local staging space
	// cannot hold the snapshot artifact set (checked before the VM is
	// paused, after a best-effort cache GC). It is retryable: the caller
	// parks the task instead of failing it, and retries when space frees.
	ErrInsufficientStorage = errors.New("insufficient local storage for the snapshot artifact set")
)
