package firecracker

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fast-sandbox/internal/artifacts"
)

// Covers the skip paths that must not touch the VMM client (a nil client
// proves no dial was attempted).
func TestSyncGuestClockSkipsUncalibratableInstants(t *testing.T) {
	d := &Driver{}
	state := &SandboxState{PID: 1}
	state.Config.Identity.SandboxUID = "sandbox-uid"
	manifest := artifacts.VMManifest{GuestControl: &artifacts.VMGuestControl{Port: 1234}}
	d.syncGuestClock(t.Context(), state, artifacts.VMManifest{}, nil)
	d.syncGuestClock(t.Context(), nil, manifest, nil)
	d.syncGuestClock(t.Context(), &SandboxState{}, manifest, nil)
}

// TestSyncGuestClockBoundsStalledVMM proves the whole calibration is
// time-bounded, config lookup included: a VMM that stops answering after
// Resume must not hold up startup recovery (which calls in with
// context.Background() and a client without request timeouts).
func TestSyncGuestClockBoundsStalledVMM(t *testing.T) {
	dir, err := os.MkdirTemp("", "fsclock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // stall exactly like an unresponsive VMM
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()

	state := &SandboxState{PID: 1}
	state.Config.Identity.SandboxUID = "sandbox-uid"
	manifest := newMetadataTestVMManifest()
	manifest.GuestControl = &artifacts.VMGuestControl{Protocol: "fsb-freeze-v1", Transport: "vsock", Port: 1234, HelperVersion: "sha256:test-helper"}

	budget := 250 * time.Millisecond
	previous := clockSyncTimeout
	clockSyncTimeout = budget
	t.Cleanup(func() { clockSyncTimeout = previous })

	started := time.Now()
	(&Driver{}).syncGuestClock(t.Context(), state, manifest, NewClient(listener.Addr().String()))
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("stalled VMM held the sync for %v (budget %v)", elapsed, budget)
	}
}

// Regression test: a paused VM's manifest must resolve on both backends —
// OCI instances keep it at the prepared instance mnt, where the native image
// cache lookup never looks, and the resolution decides whether the resumed
// VM's clock sync runs at all.
func TestResumeSourceManifestResolvesBothBackends(t *testing.T) {
	newState := func(sandboxUID, image string) *SandboxState {
		state := &SandboxState{PID: 1}
		state.Config.Identity.SandboxUID = sandboxUID
		state.Config.Desired.Image = image
		return state
	}
	// Metadata-only fixture: marshals identically on every host.
	manifest := newMetadataTestVMManifest()
	manifest.GuestControl = &artifacts.VMGuestControl{Protocol: "fsb-freeze-v1", Transport: "vsock", Port: 1234, HelperVersion: "sha256:test-helper"}
	payload, err := artifacts.MarshalVMManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}

	// OCI: the instance manifest lives under the prepared instance mnt.
	ociRoot := t.TempDir()
	home := filepath.Join(ociRoot, "sandboxes", "oci-sandbox", "mnt")
	if err = os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "manifest.json"), payload, 0o640); err != nil {
		t.Fatal(err)
	}
	got, ok, err := resumeSourceManifest(filepath.Join(ociRoot, "state"), ociRoot, newState("oci-sandbox", "oci://registry/templates:golden"))
	if err != nil || !ok || got.GuestControl == nil {
		t.Fatalf("OCI prepared manifest not resolved: ok=%v guestControl=%v err=%v", ok, got.GuestControl, err)
	}

	// Native: the manifest is the image cache commit point.
	stateRoot := t.TempDir()
	cache := filepath.Dir(cachedManifestPath(stateRoot, "native-image"))
	if err = os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cachedManifestPath(stateRoot, "native-image"), payload, 0o640); err != nil {
		t.Fatal(err)
	}
	got, ok, err = resumeSourceManifest(stateRoot, ociRoot, newState("native-sandbox", "native-image"))
	if err != nil || !ok || got.GuestControl == nil {
		t.Fatalf("native cached manifest not resolved: ok=%v guestControl=%v err=%v", ok, got.GuestControl, err)
	}

	// An absent manifest (hand-seeded cache) reports false without failing.
	if _, ok, err = resumeSourceManifest(t.TempDir(), ociRoot, newState("absent-sandbox", "absent-image")); err != nil || ok {
		t.Fatalf("absent manifest must report false: ok=%v err=%v", ok, err)
	}
}
