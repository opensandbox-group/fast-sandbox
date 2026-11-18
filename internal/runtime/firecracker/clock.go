package firecracker

import (
	"context"
	"errors"
	"os"
	"time"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/guestcontrol"
)

// resumeSourceManifest resolves the source manifest of a paused VM across
// both storage backends: OCI instances keep it at the prepared instance mnt,
// native restores in the image cache. Only an absent manifest reports false
// (hand-seeded caches); a malformed one fails.
func resumeSourceManifest(stateRoot, ociRoot string, state *SandboxState) (artifacts.VMManifest, bool, error) {
	reference, err := restoreReference(state.Config.Desired)
	if err != nil {
		return artifacts.VMManifest{}, false, err
	}
	payload, err := readSourceManifest(stateRoot, ociRoot, reference, state.Config.Identity.SandboxUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifacts.VMManifest{}, false, nil
		}
		return artifacts.VMManifest{}, false, err
	}
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return artifacts.VMManifest{}, false, err
	}
	return document, true, nil
}

// syncGuestClock sets the guest wall clock to the node clock after the VMM
// reached Running: a restored guest inherits the snapshot's clock and has no
// RTC or other external reference to correct it. It rides the guest-control
// vsock channel, not execd — clock state is lifecycle and must not depend on
// the user data plane — and is deliberately best-effort: no guest-control
// declaration, a helper predating settime, or any channel failure just
// logs; the restore itself already succeeded.
// clockSyncTimeout bounds one calibration, the VMM config lookup included:
// the Firecracker client has no request timeout and startup recovery calls
// in with context.Background(), so an unbounded sync could stall Fastlet
// recovery on a VMM that stops answering. Var so tests can shrink the
// budget.
var clockSyncTimeout = 10 * time.Second

func (d *Driver) syncGuestClock(ctx context.Context, state *SandboxState, manifest artifacts.VMManifest, client *Client) {
	if state == nil || state.PID == 0 || manifest.GuestControl == nil {
		return
	}
	// Derive before any I/O: the budget must cover the config lookup too.
	ctx, cancel := context.WithTimeout(ctx, clockSyncTimeout)
	defer cancel()
	sandboxID := state.Config.Identity.SandboxUID
	guest, err := resolveCaptureGuest(ctx, state, manifest, client)
	if err != nil {
		klog.InfoS("guest clock sync unavailable; guest keeps the snapshot clock",
			"sandboxID", sandboxID, "err", err)
		return
	}
	caps, err := guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
	if err != nil {
		klog.InfoS("guest clock sync skipped; guest control did not answer",
			"sandboxID", sandboxID, "err", err)
		return
	}
	response, err := guest.SetTime(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch}, time.Now())
	if err != nil {
		// Helpers predating settime land here; rebuild the template.
		klog.InfoS("guest clock sync failed; guest keeps the snapshot clock",
			"sandboxID", sandboxID, "helperVersion", caps.HelperVersion, "err", err)
		return
	}
	klog.V(2).InfoS("guest clock synced to host", "sandboxID", sandboxID,
		"guestTimeUnixNano", response.TimeUnixNano, "hostTimeUnixNano", time.Now().UnixNano())
}
