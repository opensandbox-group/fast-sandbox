package contract

import "errors"

var (
	ErrUnsupportedRuntime           = errors.New("unsupported container runtime")
	ErrSandboxNotFound              = errors.New("sandbox not found")
	ErrSandboxAlreadyExists         = errors.New("sandbox already exists")
	ErrRuntimeNotInitialized        = errors.New("runtime not initialized")
	ErrRuntimeCapabilityUnavailable = errors.New("runtime capability unavailable")
	ErrNetworkUnavailable           = errors.New("sandbox network unavailable")
	ErrInfraUnavailable             = errors.New("sandbox InfraProfile unavailable")
	ErrSandboxProfileMismatch       = errors.New("sandbox profile mismatch")
	ErrInvalidConfig                = errors.New("invalid sandbox config")
)
