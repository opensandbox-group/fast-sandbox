package server

// Service implements the Backend interface: it ties the pull layer
// (agent.Client), the durable state (agent/state), and the shared
// node cache (<StateRoot>/images/<sha256(image)>/) together. The native
// stage returns cache file paths as devices; overlaybd ublk devices arrive
// with stage 3.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
)

// imagePuller pulls a published image into the node cache. The pull client
// satisfies it; tests inject a fake.
type imagePuller interface {
	PullImage(ctx context.Context, stateRoot, image string) error
	// PullCheckpoint materializes an instance-private checkpoint set
	// addressed by manifest ref + digest (no image index).
	PullCheckpoint(ctx context.Context, stateRoot, reference, manifestRef, artifactDigest string) error
}

// artifactPublisher publishes a node-local artifact set to the store. The
// pull client satisfies it too; the service only wires it when the injected
// puller also publishes (tests inject a bare fake puller).
type artifactPublisher interface {
	PublishImage(ctx context.Context, kind, key, dir string) (agentpull.PublishResult, error)
}

// compatibilityPlaceholder is the stage-1 compatibility class. The full
// class (CPU/kernel/Firecracker digests, design doc §8) arrives with the
// snapshot stages.
const compatibilityPlaceholder = "native-stage-1"

// Service is the concrete agent backend.
type Service struct {
	pull      imagePuller
	publisher artifactPublisher
	state     *agentstate.State
	stateRoot string
	now       func() time.Time
	// dartUp reports the node-local DART daemon state; nil when DART is not
	// configured (stage-1 local mode).
	dartUp func() bool
	// hostReady reports the node-readiness check outcome (ready flag +
	// summary); nil when the hostready manager is not running.
	hostReady func() (bool, string)
}

// NewService assembles the stage-1 backend. When the pull client also
// implements artifactPublisher it is wired as the publish path.
func NewService(pull imagePuller, state *agentstate.State, stateRoot string, options ...ServiceOption) *Service {
	service := &Service{pull: pull, state: state, stateRoot: stateRoot, now: time.Now}
	if publisher, ok := pull.(artifactPublisher); ok {
		service.publisher = publisher
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// ServiceOption configures the service (tests).
type ServiceOption func(*Service)

// WithServiceClock overrides the clock.
func WithServiceClock(now func() time.Time) ServiceOption {
	return func(service *Service) { service.now = now }
}

// WithDARTProbe wires the node-local DART daemon state into Health: dartUp
// reports whether the DART admin plane answered its last probe. The agent's
// own health stays independent of DART (a broken gateway keeps pulls on the
// direct S3 fallback path).
func WithDARTProbe(dartUp func() bool) ServiceOption {
	return func(service *Service) { service.dartUp = dartUp }
}

// WithHostReadyProbe wires the node-readiness check outcome into Health
// (the flag behind the node labels and the FirecrackerReady condition).
// Like DART it is informational: the agent serves pulls regardless of the
// host verdict.
func WithHostReadyProbe(hostReady func() (bool, string)) ServiceOption {
	return func(service *Service) { service.hostReady = hostReady }
}

// PinImage pulls the image (if not already cached) and records one pin.
// The side effect runs inside the state's journaled two-phase execution,
// so replays return the recorded digest without re-pulling. A request that
// carries manifestRef + artifactDigest pins a checkpoint artifact set
// addressed directly instead of resolving the image index.
func (s *Service) PinImage(ctx context.Context, request agentprotocol.PinImageRequest) (agentprotocol.PinImageResponse, error) {
	direct := request.ManifestRef != "" || request.ArtifactDigest != ""
	if direct && (request.ManifestRef == "" || request.ArtifactDigest == "") {
		return agentprotocol.PinImageResponse{}, invalidRequest("manifestRef and artifactDigest must be set together")
	}
	digest, err := s.state.PinImage(request.Identity, request.Image, func() (string, error) {
		if direct {
			if err := s.ensureCheckpoint(ctx, request.Image, request.ManifestRef, request.ArtifactDigest); err != nil {
				return "", err
			}
			return request.ArtifactDigest, nil
		}
		if err := s.ensureImage(ctx, request.Image); err != nil {
			return "", err
		}
		manifestDigest, ok, err := agentpull.CachedManifestDigest(s.stateRoot, request.Image)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("image %q committed without a local manifest", request.Image)
		}
		return manifestDigest, nil
	})
	if err != nil {
		return agentprotocol.PinImageResponse{}, err
	}
	return agentprotocol.PinImageResponse{ManifestDigest: digest, Ready: true}, nil
}

// UnpinImage drops one pin reference.
func (s *Service) UnpinImage(_ context.Context, request agentprotocol.UnpinImageRequest) error {
	return s.state.UnpinImage(request.Identity, request.Image)
}

// LeaseDevices returns the native cache file paths as the device lease of
// one Sandbox. The image is ensured (pulled if needed) before the lease is
// committed.
func (s *Service) LeaseDevices(ctx context.Context, request agentprotocol.LeaseDevicesRequest) (agentprotocol.LeaseDevicesResponse, error) {
	lease, err := s.state.LeaseDevices(
		request.Identity, request.SandboxID, request.Image,
		request.MemSizeMiB, request.RootfsWritable,
		func(leaseID string) (agentprotocol.Lease, error) {
			// ensureImage verifies a committed pull (cacheComplete checks
			// the manifest and every artifact digest), so the manifest is
			// known to exist here; the response reads its digest once
			// below.
			if err := s.ensureImage(ctx, request.Image); err != nil {
				return agentprotocol.Lease{}, err
			}
			return agentprotocol.Lease{
				LeaseID: leaseID, SandboxID: request.SandboxID, Image: request.Image,
				PodUID: request.PodUID, Namespace: request.Namespace,
				RootfsDev: agentpull.ImageRootfsPath(s.stateRoot, request.Image),
				MemDev:    imageMemoryPath(s.stateRoot, request.Image),
				CreatedAt: s.now(),
			}, nil
		},
	)
	if err != nil {
		return agentprotocol.LeaseDevicesResponse{}, err
	}
	return agentprotocol.LeaseDevicesResponse{
		LeaseID: lease.LeaseID, RootfsDev: lease.RootfsDev, MemDev: lease.MemDev,
		ManifestDigest: s.manifestDigestFor(lease.Image),
	}, nil
}

// ReleaseDevices drops a device lease.
func (s *Service) ReleaseDevices(_ context.Context, request agentprotocol.ReleaseDevicesRequest) error {
	return s.state.ReleaseDevices(request.Identity, request.LeaseID)
}

// ListLeases returns every lease on the node.
func (s *Service) ListLeases(_ context.Context, _ agentprotocol.Identity) ([]agentprotocol.Lease, error) {
	return s.state.Snapshot().Leases, nil
}

// Compatibility returns the stage-1 placeholder compatibility class.
func (s *Service) Compatibility(_ context.Context) (string, error) {
	return compatibilityPlaceholder, nil
}

// Health reports the agent state and the cache footprint.
func (s *Service) Health(_ context.Context) (agentprotocol.HealthResponse, error) {
	snapshot := s.state.Snapshot()
	response := agentprotocol.HealthResponse{
		OK:         true,
		CacheBytes: cacheBytes(s.stateRoot),
		LeaseCount: snapshot.LeaseCount,
		PinCount:   snapshot.PinCount,
		ImageCount: snapshot.ImageCount,
	}
	if s.dartUp != nil {
		response.DartUp = s.dartUp()
	}
	if s.hostReady != nil {
		ready, summary := s.hostReady()
		response.HostReady = &ready
		response.HostState = summary
	}
	return response, nil
}

// PublishImage publishes a node-local snapshot artifact set. A template set
// is written under the requested index key; a checkpoint set writes no index
// and is addressed by the returned manifestRef + artifactDigest. A store
// without a write credential is refused with ErrorForbidden (pulls keep
// working); the staging directory must hold a complete native set including
// the manifest.
func (s *Service) PublishImage(ctx context.Context, request agentprotocol.PublishImageRequest) (agentprotocol.PublishImageResponse, error) {
	if s.publisher == nil {
		return agentprotocol.PublishImageResponse{}, forbidden("artifact publishing is not configured on this agent")
	}
	result, err := s.publisher.PublishImage(ctx, request.Kind, request.Key, request.Dir)
	if err != nil {
		if errors.Is(err, agentpull.ErrNotWritable) {
			return agentprotocol.PublishImageResponse{}, forbidden("%v", err)
		}
		if os.IsNotExist(err) {
			return agentprotocol.PublishImageResponse{}, invalidRequest("staged artifact set is incomplete: %v", err)
		}
		return agentprotocol.PublishImageResponse{}, err
	}
	return agentprotocol.PublishImageResponse{ManifestRef: result.ManifestRef, ArtifactDigest: result.ArtifactDigest}, nil
}

// manifestDigestFor re-reads the committed manifest digest of an image.
func (s *Service) manifestDigestFor(image string) string {
	digest, ok, err := agentpull.CachedManifestDigest(s.stateRoot, image)
	if err != nil || !ok {
		return ""
	}
	return digest
}

// ensureImage pulls the image unless the cache already holds a committed
// pull. A missing published index maps to ErrImageNotReady for the driver.
func (s *Service) ensureImage(ctx context.Context, image string) error {
	ready, err := agentpull.ImageReady(s.stateRoot, image)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	if err := s.pull.PullImage(ctx, s.stateRoot, image); err != nil {
		return err
	}
	ready, err = agentpull.ImageReady(s.stateRoot, image)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%w: %q", runtimecontract.ErrImageNotReady, image)
	}
	return nil
}

// ensureCheckpoint pulls a checkpoint artifact set (addressed by manifest
// ref + digest, e.g. the canonical checkpoint reference) unless the cache
// already holds the complete committed set.
func (s *Service) ensureCheckpoint(ctx context.Context, reference, manifestRef, artifactDigest string) error {
	ready, err := agentpull.ImageReady(s.stateRoot, reference)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	if err := s.pull.PullCheckpoint(ctx, s.stateRoot, reference, manifestRef, artifactDigest); err != nil {
		return err
	}
	ready, err = agentpull.ImageReady(s.stateRoot, reference)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%w: checkpoint %q", runtimecontract.ErrImageNotReady, reference)
	}
	return nil
}

// imageMemoryPath returns the cached native memory snapshot path of an
// image, or "" when the artifact set has none.
func imageMemoryPath(stateRoot, image string) string {
	path := filepath.Join(filepath.Dir(agentpull.ImageRootfsPath(stateRoot, image)), "memory.snap")
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

var _ Backend = (*Service)(nil)
