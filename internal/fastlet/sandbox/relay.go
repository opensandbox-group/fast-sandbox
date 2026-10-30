package sandbox

import (
	"context"
	"errors"
	"fmt"

	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// InvokeHandler is the Fastlet delivery endpoint of the FastPath
// handler-relay channel: it forwards the relayed payload to the
// Pool-declared Action Handler over the Pod loopback and returns the
// Handler's HTTP status and body verbatim as the per-handler result. The
// identity fences guarantee a relayed request can never reach a replaced
// runtime, and the Handler's outcome is never reinterpreted: a 4xx/5xx
// Handler status is a result, while relay failures surface as Fastlet
// errors.
func (m *SandboxManager) InvokeHandler(ctx context.Context, req *fastletapi.InvokeHandlerRequest) (*fastletapi.InvokeHandlerResponse, error) {
	if req == nil || req.Handler == "" {
		failure := fastletError(fastletapi.ErrorConflict, "handler is required", false)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	if err := fastletapi.ValidateHandlerRelay(req.Method, req.Path); err != nil {
		failure := fastletError(fastletapi.ErrorConflict, err.Error(), false)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	if failure := m.validateIdentityTarget(&req.Identity); failure != nil {
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	m.mu.Lock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata == nil {
		m.mu.Unlock()
		failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
		m.mu.Unlock()
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	if metadata.Phase == sandboxStateCreating || metadata.Phase == sandboxStateTerminating || metadata.Phase == sandboxStateDeleting {
		m.mu.Unlock()
		failure := fastletError(fastletapi.ErrorInProgress, "runtime is not ready for handler relay", true)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	actionManager := m.actionManager
	attachment := actionAttachment(metadata)
	m.mu.Unlock()
	if actionManager == nil {
		failure := fastletError(fastletapi.ErrorActionUnavailable, "Fastlet has no Sandbox Action configuration", false)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	status, body, relayErr := actionManager.RelayHandler(ctx, attachment, req.Handler, req.Method, req.Path, req.Payload)
	if errors.Is(relayErr, fastletaction.ErrUnknownHandler) {
		failure := fastletError(fastletapi.ErrorNotFound, fmt.Sprintf("Action Handler %q is not declared", req.Handler), false)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	if relayErr != nil {
		failure := fastletErrorWithCause(fastletapi.ErrorActionUnavailable, fmt.Sprintf("relay to Action Handler %q: %v", req.Handler, relayErr), true, relayErr)
		return &fastletapi.InvokeHandlerResponse{Error: failure}, failure
	}
	return &fastletapi.InvokeHandlerResponse{Status: int32(status), Body: body}, nil
}
