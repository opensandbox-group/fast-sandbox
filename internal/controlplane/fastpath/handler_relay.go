package fastpath

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// InvokeHandlerAction is the generic handler-relay verb of the FastPath v2
// control plane: it validates the tenant-scoped Sandbox ownership, resolves
// the hosting Fastlet, and relays the opaque payload to the Pool-declared
// Action Handler. The handler's HTTP status and body are returned verbatim —
// the handler's response is the synchronous ack, and a 4xx/5xx handler
// outcome is a result, not an RPC error. Relay failures (unknown Sandbox,
// undeclared handler, unreachable Fastlet or handler) are surfaced to the
// caller and never swallowed.
func (s *Server) InvokeHandlerAction(ctx context.Context, request *fastpathv2.InvokeHandlerActionRequest) (_ *fastpathv2.InvokeHandlerActionResponse, resultErr error) {
	started := time.Now()
	defer func() {
		handlerRelayDuration.WithLabelValues(grpcMetricResult(resultErr)).Observe(time.Since(started).Seconds())
	}()
	if request == nil || request.Sandbox == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	if request.Handler == "" {
		return nil, status.Error(codes.InvalidArgument, "handler is required")
	}
	sandbox, err := s.sandboxFromReferenceAtGeneration(ctx, request.Sandbox, 0)
	if err != nil {
		return nil, err
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{
		Namespace: sandbox.Namespace, SandboxName: sandbox.Name, SandboxUID: string(sandbox.UID),
	})
	pool, err := s.getPool(ctx, sandbox.Namespace, sandbox.Spec.PoolRef)
	if err != nil {
		return nil, err
	}
	if !poolDeclaresHandler(pool, request.Handler) {
		return nil, status.Errorf(codes.NotFound, "Action Handler %q is not declared by Pool %q", request.Handler, pool.Name)
	}
	fastlet, envelope, err := s.relayTarget(sandbox)
	if err != nil {
		return nil, err
	}
	// The generic control timeout applies: the Fastlet bounds the handler
	// hop itself, so the relay stays within one bounded control round trip.
	client, ok := s.Orchestrator.FastletClient.(interface {
		InvokeHandler(context.Context, string, *fastletapi.InvokeHandlerRequest) (*fastletapi.InvokeHandlerResponse, error)
	})
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "Fastlet client does not support handler relay")
	}
	response, callErr := client.InvokeHandler(ctx, fastlet.PodIP, &fastletapi.InvokeHandlerRequest{
		Identity: internalIdentity(sandbox, envelope),
		Handler:  request.Handler,
		Payload:  request.Payload,
	})
	var failure *fastletapi.FastletError
	if errors.As(callErr, &failure) {
		if failure.Code == fastletapi.ErrorNotFound {
			return nil, status.Errorf(codes.NotFound, "relay rejected by Fastlet: %v", failure)
		}
		return nil, status.Errorf(codes.Unavailable, "handler relay failed: %v", failure)
	}
	if callErr != nil {
		return nil, status.Errorf(codes.Unavailable, "invoke handler on Fastlet: %v", callErr)
	}
	if response == nil || response.Status == 0 {
		return nil, status.Error(codes.Unavailable, "Fastlet returned no handler result")
	}
	return &fastpathv2.InvokeHandlerActionResponse{
		Result: &fastpathv2.HandlerActionResult{Status: uint32(response.Status), Body: response.Body},
	}, nil
}

// relayTarget resolves the Sandbox's durable assignment to a reachable
// Fastlet under the same fences as live inspection: the assignment envelope
// must be effective and the registered Fastlet must still be the assigned
// Pod with a routable IP.
func (s *Server) relayTarget(sandbox *apiv1alpha2.Sandbox) (placement.FastletInfo, *assignment.AssignmentEnvelope, error) {
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil || envelope == nil {
		return placement.FastletInfo{}, nil, status.Error(codes.Unavailable, "Sandbox assignment is not ready")
	}
	if s.Orchestrator == nil || s.Orchestrator.Registry == nil || s.Orchestrator.FastletClient == nil {
		return placement.FastletInfo{}, nil, status.Error(codes.FailedPrecondition, "Fastlet relay client is not configured")
	}
	fastlet, found := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !found || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" {
		return placement.FastletInfo{}, nil, status.Error(codes.Unavailable, "assigned Fastlet is unavailable")
	}
	return fastlet, envelope, nil
}

func poolDeclaresHandler(pool *apiv1alpha2.SandboxPool, handler string) bool {
	for _, candidate := range pool.Spec.ActionHandlers {
		if candidate.Name == handler {
			return true
		}
	}
	return false
}
