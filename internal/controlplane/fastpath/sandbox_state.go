package fastpath

// sandbox_state.go implements PauseSandbox/ResumeSandbox: the durable pause
// intent is the Sandbox spec.state field, so both RPCs are compare-and-set
// spec patches that return the CRD projection immediately; the Controller
// converges (checkpoint task, runtime release) asynchronously and clients
// observe runtime_state PAUSED/READY via GetSandbox.

import (
	"context"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) PauseSandbox(ctx context.Context, request *fastpathv2.PauseSandboxRequest) (*fastpathv2.PauseSandboxResponse, error) {
	if request == nil || request.Sandbox == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	if request.RequestId != "" {
		if err := ValidateRequestID(request.RequestId); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	sandbox, err := s.sandboxFromReferenceAtGeneration(ctx, request.Sandbox, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	if sandbox.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox is being deleted")
	}
	// Pause is impossible only once the runtime is terminal; every other
	// state (including an in-flight create) simply waits for Ready in the
	// Controller. An effective pause replayed here is an idempotent success.
	switch sandbox.Status.Runtime.State {
	case apiv1alpha2.RuntimeStopped, apiv1alpha2.RuntimeFailed, apiv1alpha2.RuntimeStopping:
		return nil, status.Errorf(codes.FailedPrecondition, "Sandbox runtime is %q; only a Ready runtime can be paused", sandbox.Status.Runtime.State)
	}
	updated, err := s.patchSandboxState(ctx, sandbox, apiv1alpha2.SandboxStatePaused, request.ExpectedGeneration, request.Sandbox.ExpectedUid)
	if err != nil {
		return nil, err
	}
	return &fastpathv2.PauseSandboxResponse{Sandbox: sandboxInfoFromCRD(updated), Generation: updated.Generation}, nil
}

func (s *Server) ResumeSandbox(ctx context.Context, request *fastpathv2.ResumeSandboxRequest) (*fastpathv2.ResumeSandboxResponse, error) {
	if request == nil || request.Sandbox == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	if request.RequestId != "" {
		if err := ValidateRequestID(request.RequestId); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	sandbox, err := s.sandboxFromReferenceAtGeneration(ctx, request.Sandbox, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	if sandbox.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox is being deleted")
	}
	// The desired state is already Running (a resumed or never-paused
	// Sandbox): idempotent success. This also covers a replay while the
	// resume is still converging.
	if sandbox.Spec.State != apiv1alpha2.SandboxStatePaused {
		return &fastpathv2.ResumeSandboxResponse{Sandbox: sandboxInfoFromCRD(sandbox), Generation: sandbox.Generation}, nil
	}
	// The pause was released (runtime Paused): resuming requires the durable
	// checkpoint. Before the release a checkpoint may not exist yet and the
	// Resume intent simply cancels the pause.
	if sandbox.Status.Runtime.State == apiv1alpha2.RuntimePaused && sandbox.Status.Runtime.Checkpoint == nil {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox has no recorded checkpoint; resume is impossible, use resetRevision to start a fresh instance")
	}
	checkpoint := sandbox.Status.Runtime.Checkpoint
	if checkpoint != nil && request.ExpectedCheckpointId != "" && request.ExpectedCheckpointId != checkpoint.CheckpointID {
		return nil, status.Errorf(codes.Aborted, "checkpoint changed: expected %s, current %s", request.ExpectedCheckpointId, checkpoint.CheckpointID)
	}
	updated, err := s.patchSandboxState(ctx, sandbox, apiv1alpha2.SandboxStateRunning, request.ExpectedGeneration, request.Sandbox.ExpectedUid)
	if err != nil {
		return nil, err
	}
	return &fastpathv2.ResumeSandboxResponse{Sandbox: sandboxInfoFromCRD(updated), Generation: updated.Generation}, nil
}

// patchSandboxState compare-and-set patches spec.state, fenced by the
// optional expected generation and UID. A no-op patch (state already
// desired) still returns the current object, which makes both RPCs replay
// safe.
func (s *Server) patchSandboxState(ctx context.Context, sandbox *apiv1alpha2.Sandbox, state apiv1alpha2.SandboxState, expectedGeneration int64, expectedUID string) (*apiv1alpha2.Sandbox, error) {
	key := client.ObjectKeyFromObject(sandbox)
	var updated apiv1alpha2.Sandbox
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current apiv1alpha2.Sandbox
		if err := s.K8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		if err := checkExpectedGeneration(&current, expectedGeneration); err != nil {
			return err
		}
		if err := checkReferenceUID(&current, expectedUID); err != nil {
			return err
		}
		if current.Spec.State == state {
			updated = current
			return nil
		}
		current.Spec.State = state
		if err := s.K8sClient.Update(ctx, &current); err != nil {
			return err
		}
		updated = current
		return nil
	})
	if err != nil {
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, grpcKubernetesError(err)
	}
	return &updated, nil
}
