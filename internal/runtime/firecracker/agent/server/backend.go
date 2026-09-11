package server

import (
	"context"
	"fmt"

	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// Backend implements the agent service logic behind the UDS HTTP transport.
// The server layer enforces identity, idempotency, and error mapping; the
// backend owns the pull and device-side effects.
type Backend interface {
	PinImage(context.Context, agentprotocol.PinImageRequest) (agentprotocol.PinImageResponse, error)
	UnpinImage(context.Context, agentprotocol.UnpinImageRequest) error
	LeaseDevices(context.Context, agentprotocol.LeaseDevicesRequest) (agentprotocol.LeaseDevicesResponse, error)
	ReleaseDevices(context.Context, agentprotocol.ReleaseDevicesRequest) error
	ListLeases(context.Context, agentprotocol.Identity) ([]agentprotocol.Lease, error)
	Compatibility(context.Context) (string, error)
	Health(context.Context) (agentprotocol.HealthResponse, error)
	PublishImage(context.Context, agentprotocol.PublishImageRequest) (agentprotocol.PublishImageResponse, error)
}

// Error is a wire-classified backend error; plain errors map to Internal.
type Error struct {
	Code    agentprotocol.ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// invalidRequest builds a 400 backend error.
func invalidRequest(format string, args ...any) *Error {
	return &Error{Code: agentprotocol.ErrorInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// unauthorized builds a 403 backend error.
func unauthorized(format string, args ...any) *Error {
	return &Error{Code: agentprotocol.ErrorUnauthorized, Message: fmt.Sprintf(format, args...)}
}

// forbidden builds a 403 backend error for a store-level refusal (no write
// credential) as opposed to a caller-identity rejection.
func forbidden(format string, args ...any) *Error {
	return &Error{Code: agentprotocol.ErrorForbidden, Message: fmt.Sprintf(format, args...)}
}
