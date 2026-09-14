package firecracker

// image_delivery.go implements the asynchronous artifact delivery of the
// driver (runtimecontract.ImageDelivery): a create whose rootfs image is not
// cached never blocks on the node-side pull. DeliverImage guarantees one
// background delivery attempt per image is in flight and reports the image as
// Delivering; the caller (Fastlet) parks the Sandbox and polls. A finished
// attempt reports its result: Delivered once the committed cache is visible,
// or the sticky attempt error so polling callers can fail the Sandbox with a
// terminal observation instead of retrying forever.
//
// Delivery attempts are single-flight per image: concurrent create intents
// for the same cold image coalesce into one PinImage (the agent journal
// dedupes replays by the pod-scoped warm-pull request id anyway). A failed
// attempt stays sticky for a short report window so polling callers observe
// the failure at least once; after the window a later call starts a fresh
// attempt, which lets a re-created Sandbox recover once the image becomes
// available.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	"k8s.io/klog/v2"
)

// Driver implements the optional artifact-delivery extensions.
var (
	_ runtimecontract.ImageDelivery      = (*Driver)(nil)
	_ runtimecontract.CheckpointDelivery = (*Driver)(nil)
)

const (
	// defaultImageDeliveryAttemptTimeout bounds one background delivery
	// attempt (image index + manifest + artifact set transfer).
	defaultImageDeliveryAttemptTimeout = 30 * time.Minute

	// defaultImageDeliveryFailureWindow keeps a failed delivery sticky for
	// this long: within the window DeliverImage reports the attempt error
	// instead of immediately starting a doomed attempt, so a polling worker
	// can count failures; after it, a new attempt is started on demand.
	defaultImageDeliveryFailureWindow = 30 * time.Second
)

// imageDelivery tracks the delivery state of one image.
type imageDelivery struct {
	mu        sync.Mutex
	inFlight  bool      // a delivery attempt is running
	failedErr error     // result of the last finished attempt, when it failed
	failedAt  time.Time // when the last attempt failed
}

// DeliverImage implements runtimecontract.ImageDelivery. An image whose
// complete restore set is cached reports Delivered without touching the
// agent. A missing image with no runtime-agent configured is an error (local
// mode cannot deliver); otherwise one background delivery attempt per image
// is kept in flight and the image reports Delivering. A recent attempt
// failure is reported once (sticky window); afterwards the next call starts
// a fresh attempt.
func (d *Driver) DeliverImage(_ context.Context, image string) (runtimecontract.ImageDeliveryStatus, error) {
	if strings.TrimSpace(image) == "" {
		return "", fmt.Errorf("%w: image reference is required", ErrInvalidConfig)
	}
	return d.deliverReference(image, func(ctx context.Context) error {
		return d.pinImageForDelivery(ctx, image)
	})
}

// DeliverCheckpoint implements runtimecontract.CheckpointDelivery: the resume
// counterpart of DeliverImage, addressed by the checkpoint manifest ref +
// digest instead of an image index. Both paths share the local cache layout,
// the single-flight tracker, and the sticky failure window; only the pin call
// differs (agent pull by ref vs. pull by image index).
func (d *Driver) DeliverCheckpoint(_ context.Context, restore *fastletapi.RestoreSpec) (runtimecontract.ImageDeliveryStatus, error) {
	if restore == nil || strings.TrimSpace(restore.ManifestRef) == "" || strings.TrimSpace(restore.ArtifactDigest) == "" {
		return "", fmt.Errorf("%w: checkpoint manifestRef and artifactDigest are required", ErrInvalidConfig)
	}
	reference := artifacts.CheckpointReference(restore.ArtifactDigest)
	return d.deliverReference(reference, func(ctx context.Context) error {
		return d.pinCheckpointForDelivery(ctx, reference, restore)
	})
}

// deliverReference is the shared asynchronous delivery engine of images and
// checkpoints. reference keys both the local cache and the single-flight
// tracker; pin performs one background pull attempt.
func (d *Driver) deliverReference(reference string, pin func(context.Context) error) (runtimecontract.ImageDeliveryStatus, error) {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	if err := verifyRestorableImage(stateRoot, reference); err == nil {
		d.touchImage(reference)
		return runtimecontract.ImageDelivered, nil
	}
	client, err := d.agentClientOrNil()
	if err != nil {
		return "", err
	}
	if client == nil {
		return "", fmt.Errorf("%w: %q is not cached and no runtime-agent is configured to deliver it", ErrImageNotReady, reference)
	}

	d.mu.Lock()
	entry := d.imageDeliveryLocked(reference)
	d.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.inFlight {
		return runtimecontract.ImageDelivering, nil
	}
	if entry.failedErr != nil && time.Since(entry.failedAt) < d.deliveryFailureWindowSetting() {
		failed := entry.failedErr
		return "", failed
	}
	entry.inFlight = true
	entry.failedErr = nil
	entry.failedAt = time.Time{}
	go d.runDeliveryAttempt(reference, pin, entry)
	return runtimecontract.ImageDelivering, nil
}

// imageDeliveryLocked returns (creating if needed) the delivery tracker of an
// image. The caller holds d.mu.
func (d *Driver) imageDeliveryLocked(image string) *imageDelivery {
	if d.imageDeliveries == nil {
		d.imageDeliveries = make(map[string]*imageDelivery)
	}
	key := imageKey(image)
	entry := d.imageDeliveries[key]
	if entry == nil {
		entry = &imageDelivery{}
		d.imageDeliveries[key] = entry
	}
	return entry
}

// runDeliveryAttempt performs one PinImage/PinCheckpoint pull in the
// background and records the terminal result on the tracker. Success is
// verified locally against the complete restore set: the agent journal
// replays a committed PinImage without re-pulling, so a cache purged under a
// committed pin must surface as a failed attempt (with a diagnosable error)
// instead of a success that never materializes.
func (d *Driver) runDeliveryAttempt(reference string, pin func(context.Context) error, entry *imageDelivery) {
	timeout := d.deliveryAttemptTimeoutSetting()
	if timeout <= 0 {
		timeout = defaultImageDeliveryAttemptTimeout
	}
	attemptContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	klog.InfoS("firecracker artifact delivery attempt started", "reference", reference, "timeout", timeout.String())
	err := pin(attemptContext)
	if err == nil {
		d.mu.RLock()
		stateRoot := d.config.StateRoot
		d.mu.RUnlock()
		if resolveErr := verifyRestorableImage(stateRoot, reference); resolveErr != nil {
			err = fmt.Errorf("%w: runtime-agent reports %q delivered but no committed restore set exists in the local cache (cache purged under an idempotent pin?); rebuild the environment or clear the agent journal to re-pull", ErrImageNotReady, reference)
		} else {
			d.touchImage(reference)
		}
	}

	entry.mu.Lock()
	entry.inFlight = false
	if err == nil {
		entry.failedErr = nil
		entry.failedAt = time.Time{}
	} else {
		entry.failedErr = err
		entry.failedAt = time.Now()
	}
	entry.mu.Unlock()

	if err == nil {
		klog.InfoS("firecracker artifact delivery completed", "reference", reference)
		return
	}
	klog.ErrorS(err, "firecracker artifact delivery attempt failed", "reference", reference)
}

// pinImageForDelivery pins (and thereby pulls) an image on the runtime-agent.
// The pod-scoped warm-pull request id makes replays idempotent on the agent.
func (d *Driver) pinImageForDelivery(ctx context.Context, image string) error {
	client, err := d.agentClientOrNil()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("%w: runtime-agent disappeared while delivering %q", ErrImageNotReady, image)
	}
	if _, err := client.PinImage(ctx, d.warmPullRequestID(image), image); err != nil {
		return err
	}
	return nil
}

// pinCheckpointForDelivery pins (and thereby pulls) a checkpoint artifact set
// addressed by manifest ref + digest. reference is the canonical checkpoint
// cache reference used for the idempotent warm-pull request id.
func (d *Driver) pinCheckpointForDelivery(ctx context.Context, reference string, restore *fastletapi.RestoreSpec) error {
	client, err := d.agentClientOrNil()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("%w: runtime-agent disappeared while delivering checkpoint %q", ErrImageNotReady, reference)
	}
	if _, err := client.PinCheckpoint(ctx, d.warmPullRequestID(reference), reference, restore.ManifestRef, restore.ArtifactDigest); err != nil {
		return err
	}
	return nil
}

func (d *Driver) deliveryAttemptTimeoutSetting() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.imageDeliveryAttemptTimeout > 0 {
		return d.imageDeliveryAttemptTimeout
	}
	return defaultImageDeliveryAttemptTimeout
}

func (d *Driver) deliveryFailureWindowSetting() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.imageDeliveryFailureWindow > 0 {
		return d.imageDeliveryFailureWindow
	}
	return defaultImageDeliveryFailureWindow
}
