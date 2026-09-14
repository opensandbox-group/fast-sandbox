package sandbox

// boot_lifecycle.go drives a cold Sandbox from the image-pending phase to a
// committed runtime. The Create RPC parks the Sandbox the moment the runtime
// reports ImageDelivering (the artifact delivery runs in the background on the
// node); this worker polls the delivery state and, once the image is
// committed, boots the runtime through the exact same EnsureSandbox -> commit
// -> data-plane sequence the synchronous create path uses. Delivery failures
// surface as a terminal create-failed phase instead of retrying forever.

import (
	"context"
	"errors"
	"fmt"
	"time"

	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"k8s.io/klog/v2"
)

const (
	// initialImageBootPoll is the first delivery poll delay; it doubles up to
	// maxImageBootPoll between polls.
	initialImageBootPoll = 300 * time.Millisecond
	maxImageBootPoll     = 5 * time.Second
	// imageDeliveryFailLimit is the number of consecutive reported delivery
	// failures after which the Sandbox transitions to create-failed.
	imageDeliveryFailLimit = 3
	// imageBootTimeout bounds one EnsureSandbox boot attempt of the worker.
	imageBootTimeout = 5 * time.Minute
)

type imageBootWorker struct {
	metadata *SandboxMetadata
	cancel   context.CancelFunc
}

// deliveryForCreate selects the asynchronous artifact-delivery path of one
// create/resume: a Sandbox carrying Restore is delivered as a checkpoint set
// (manifestRef + digest), everything else as an image. It reports false when
// the runtime does not implement the matching optional extension, so callers
// fall back to the synchronous path exactly as before.
func (m *SandboxManager) deliveryForCreate(input *fastletapi.EnsureSandboxInput) (func(context.Context) (ImageDeliveryStatus, error), bool) {
	if input == nil {
		return nil, false
	}
	if restore := input.Sandbox.Spec.Restore; restore != nil {
		delivery, ok := m.runtime.(CheckpointDelivery)
		if !ok {
			return nil, false
		}
		return func(ctx context.Context) (ImageDeliveryStatus, error) {
			return delivery.DeliverCheckpoint(ctx, restore)
		}, true
	}
	delivery, ok := m.runtime.(ImageDelivery)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context) (ImageDeliveryStatus, error) {
		return delivery.DeliverImage(ctx, input.Sandbox.Spec.Image)
	}, true
}

// parkForImageDelivery parks a cold Sandbox: the placeholder transitions to
// image-pending, the boot worker takes over delivery -> boot progression, and
// the Create RPC returns immediately with a Created observation (runtime
// Creating). Callers of a READY completion poll the Sandbox status instead of
// holding the RPC open for the artifact transfer.
func (m *SandboxManager) parkForImageDelivery(req *fastletapi.CreateSandboxRequest, input *fastletapi.EnsureSandboxInput, placeholder *SandboxMetadata, started time.Time) (*fastletapi.CreateSandboxResponse, error) {
	sandboxUID := input.Sandbox.Identity.SandboxUID
	message := fmt.Sprintf("sandbox image %q is being delivered to the node", input.Sandbox.Spec.Image)
	m.mu.Lock()
	if m.sandboxes[sandboxUID] != placeholder {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox changed before image delivery parked", true), admission)
	}
	if placeholder.Phase == "terminating" {
		admission := m.admissionStatusLocked()
		go m.asyncDelete(sandboxUID, placeholder)
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox was deleted while image delivery was starting", false), admission)
	}
	placeholder.Phase = "image-pending"
	m.runtimeMessages[sandboxUID] = message
	status := m.sandboxStatusLocked(placeholder)
	admission := m.admissionStatusLocked()
	m.mu.Unlock()

	m.startImageBootWorker(placeholder, req, input, started)
	m.recordDiagnostic(sandboxUID, "info", "runtime", "image-pending", message)
	klog.InfoS("sandbox parked for async image delivery", "sandboxId", sandboxUID, "image", input.Sandbox.Spec.Image)
	return &fastletapi.CreateSandboxResponse{
		Disposition: fastletapi.CreateDispositionCreated,
		Sandbox:     &status,
		Admission:   admission,
	}, nil
}

// startImageBootWorker parks the delivery progression of a cold Sandbox. It is
// called after the Create RPC decided to park (parkForImageDelivery). One
// worker per Sandbox; a replacement metadata instance cancels its predecessor.
func (m *SandboxManager) startImageBootWorker(metadata *SandboxMetadata, req *fastletapi.CreateSandboxRequest, input *fastletapi.EnsureSandboxInput, started time.Time) {
	uid := metadata.Config.Identity.SandboxUID
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if existing, found := m.imageBootWorkers[uid]; found {
		if existing.metadata == metadata {
			m.mu.Unlock()
			cancel()
			return
		}
		existing.cancel()
	}
	m.imageBootWorkers[uid] = imageBootWorker{metadata: metadata, cancel: cancel}
	m.mu.Unlock()

	reqCopy := *req
	reqCopy.ActionBindings = append([]fastletapi.ActionBindingInput(nil), req.ActionBindings...)
	inputCopy := *input
	go m.runImageBootWorker(ctx, metadata, &reqCopy, &inputCopy, started)
}

// runImageBootWorker polls the async image delivery until it commits, boots
// the runtime, and hands over to the data-plane lifecycle. It owns the
// cleanup of a parked Sandbox that is deleted while the worker is alive.
func (m *SandboxManager) runImageBootWorker(ctx context.Context, metadata *SandboxMetadata, req *fastletapi.CreateSandboxRequest, input *fastletapi.EnsureSandboxInput, started time.Time) {
	uid := metadata.Config.Identity.SandboxUID
	deliver, ok := m.deliveryForCreate(input)
	if !ok {
		m.markCreateFailed(metadata, ErrUnsupportedRuntime)
		return
	}
	defer func() {
		m.mu.Lock()
		if worker, found := m.imageBootWorkers[uid]; found && worker.metadata == metadata {
			delete(m.imageBootWorkers, uid)
		}
		terminating := m.sandboxes[uid] == metadata && metadata.Phase == "terminating"
		m.mu.Unlock()
		if terminating {
			go m.asyncDelete(uid, metadata)
		}
	}()

	pollDelay := initialImageBootPoll
	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		current := m.sandboxes[uid]
		if current != metadata || metadata.Phase != "image-pending" {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()

		status, err := deliver(ctx)
		if err != nil {
			failures++
			if failures >= imageDeliveryFailLimit {
				m.markCreateFailed(metadata, fmt.Errorf("deliver sandbox artifacts for %q: %w", uid, err))
				return
			}
			if !sleepImageBootPoll(ctx, pollDelay) {
				return
			}
			pollDelay = min(pollDelay*2, maxImageBootPoll)
			continue
		}
		failures = 0
		if status == ImageDelivering {
			if !sleepImageBootPoll(ctx, pollDelay) {
				return
			}
			pollDelay = min(pollDelay*2, maxImageBootPoll)
			continue
		}

		// Image delivered: boot the runtime inside the worker. The commit
		// below is identical to the synchronous create tail, so lifecycle
		// hooks, data-plane reconciliation, and readiness observations stay
		// the same regardless of the delivery path.
		klog.InfoS("sandbox artifacts delivered; booting runtime", "sandboxId", uid, "image", input.Sandbox.Spec.Image)
		bootCtx, cancel := context.WithTimeout(ctx, imageBootTimeout)
		result, ensureErr := m.runtime.EnsureSandbox(bootCtx, input)
		cancel()
		if ensureErr != nil {
			if errors.Is(ensureErr, ErrImageNotReady) {
				// The committed image vanished under us (cache race): the
				// delivery layer restarts the pull on the next poll.
				pollDelay = initialImageBootPoll
				continue
			}
			m.markCreateFailed(metadata, ensureErr)
			return
		}
		m.mu.Lock()
		stillPending := m.sandboxes[uid] == metadata && metadata.Phase == "image-pending"
		if stillPending {
			metadata.Phase = "creating"
		}
		m.mu.Unlock()
		if !stillPending {
			return
		}
		_, _, dataPlaneReady, commitFailure := m.commitRuntimeCreate(req, *input, metadata, result)
		if commitFailure != nil {
			return
		}
		m.recordRuntimeReadyAndDispatchHooks(result, req, dataPlaneReady)
		m.continueDataPlaneCreation(result, started, dataPlaneReady)
		return
	}
}

// markCreateFailed projects the terminal create-failed observation of a cold
// Sandbox whose background boot cannot converge. The Sandbox stays managed so
// the Controller observes the failure; it does not occupy admission capacity
// and is removed by the regular delete path.
func (m *SandboxManager) markCreateFailed(metadata *SandboxMetadata, cause error) {
	uid := metadata.Config.Identity.SandboxUID
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sandboxes[uid] != metadata {
		return
	}
	if metadata.Phase == "terminating" || metadata.Phase == "deleting" {
		return
	}
	metadata.Phase = "create-failed"
	m.runtimeMessages[uid] = cause.Error()
	m.cacheProtection.ProtectHotUntil(metadata.Config.Spec.Image, m.clock.Now().Add(time.Hour))
	m.recordDiagnosticLocked(uid, "error", "runtime", "create-failed", cause.Error())
	klog.ErrorS(cause, "cold Sandbox create failed", "sandboxId", uid, "image", metadata.Config.Spec.Image)
}

func sleepImageBootPoll(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// cancelImageBootWorkerLocked stops the image boot worker of a Sandbox. The
// caller holds m.mu.
func (m *SandboxManager) cancelImageBootWorkerLocked(metadata *SandboxMetadata) {
	uid := metadata.Config.Identity.SandboxUID
	worker, found := m.imageBootWorkers[uid]
	if !found || worker.metadata != metadata {
		return
	}
	delete(m.imageBootWorkers, uid)
	worker.cancel()
}
