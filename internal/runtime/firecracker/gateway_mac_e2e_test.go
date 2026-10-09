//go:build firecracker

package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/guestnetwork"
)

// TestFirecrackerDriverE2ECachedGatewayMAC restores concurrent clones of a
// fresh snapshot containing a resolved guest gateway neighbor. No readiness
// warm-up, guest neighbor flush, ping retry, or sandbox recreate is allowed.
func TestFirecrackerDriverE2ECachedGatewayMAC(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root: netns, tap, and iptables setup")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("requires /dev/kvm")
	}
	require.NotEmpty(t, os.Getenv("FC_JAILER"), "per-clone netns requires FC_JAILER")
	_, err := exec.LookPath("tcpdump")
	require.NoError(t, err, "tcpdump is required to inspect the first reply's Ethernet destination")

	// External/cached templates do not guarantee a resolved neighbor. Always
	// prepare our own, retaining the caller's XFS StateRoot filesystem.
	parent := os.Getenv("FC_STATE_ROOT")
	if parent != "" {
		require.NoError(t, os.MkdirAll(parent, 0o750))
	}
	stateRoot, err := os.MkdirTemp(parent, "fc-gateway-mac-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })
	t.Setenv("FC_STATE_ROOT", stateRoot)
	t.Setenv("FC_SKIP_PREP", "0")
	// The checkpoint is local to this test, not pinned through runtime-agent.
	t.Setenv("FC_AGENT_SOCKET", "")

	const cloneCount = 5
	env := newE2EEnvironment(t, cloneCount+1, false)
	defer env.teardown()

	errors := make([]error, cloneCount)
	var wg sync.WaitGroup
	for index := range cloneCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errors[index] = env.driver.EnsureSandbox(context.Background(), ensureInput(env.spec(index+1)))
		}()
	}
	wg.Wait()
	for _, err := range errors {
		require.NoError(t, err)
	}
	// Only clear host-side leftovers from preceding tests. Guest neighbors
	// remain exactly as restored from the snapshot.
	output, err := exec.Command("ip", "neigh", "flush", "dev", "fsb0").CombinedOutput()
	require.NoError(t, err, "flush host bridge neighbors: %s", output)
	t.Run("concurrent-first-replies", func(t *testing.T) {
		for index := range cloneCount {
			t.Run(fmt.Sprintf("clone-%d", index+1), func(t *testing.T) {
				t.Parallel()
				slot, ok := env.manager.Lookup(env.spec(index + 1).Identity.SandboxUID)
				require.True(t, ok)
				require.NoError(t, probeCachedGatewayReply(t, slot.NetNSName, slot.GuestTap, slot.Gateway, slot.IP))
			})
		}
	})

	// Negative control: changing only the runtime TAP MAC must lose the
	// first reply, still addressed to the snapshot's cached gateway MAC.
	// This proves that the positive case cannot pass by re-learning ARP.
	t.Run("changed-tap-mac-loses-first-reply", func(t *testing.T) {
		spec := env.spec(cloneCount + 1)
		_, err := env.driver.EnsureSandbox(context.Background(), ensureInput(spec))
		require.NoError(t, err)
		slot, ok := env.manager.Lookup(spec.Identity.SandboxUID)
		require.True(t, ok)
		output, err := exec.Command("ip", "-n", slot.NetNSName, "link", "set", "dev", slot.GuestTap,
			"address", "02:00:00:00:00:fe").CombinedOutput()
		require.NoError(t, err, "change control TAP MAC: %s", output)
		require.Error(t, probeCachedGatewayReply(t, slot.NetNSName, slot.GuestTap, slot.Gateway, slot.IP),
			"a different TAP MAC must drop the first cached-neighbor reply")
	})
}

// probeCachedGatewayReply sends exactly one echo request and captures its
// first reply, even when the host kernel drops it for the wrong destination
// MAC. A permanent HOST neighbor prevents incoming ARP from refreshing the
// guest's cached gateway entry before the probe. tcpdump uses no promiscuous
// mode and sees guest frames without changing guest or TAP addressing.
func probeCachedGatewayReply(t *testing.T, namespace, tap, gateway, slotIP string) error {
	t.Helper()
	output, err := exec.Command("ip", "-n", namespace, "neigh", "replace", e2ePrepGuestIP,
		"lladdr", e2ePrepMAC, "nud", "permanent", "dev", tap).CombinedOutput()
	require.NoError(t, err, "set host-only guest neighbor: %s", output)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stderrPath := filepath.Join(t.TempDir(), "tcpdump.log")
	stderr, err := os.Create(stderrPath)
	require.NoError(t, err)
	defer stderr.Close()
	capture := exec.CommandContext(ctx, "ip", "netns", "exec", namespace, "tcpdump", "-p", "-n", "-e", "-l",
		"-i", tap, "-c", "1", "icmp and src host "+e2ePrepGuestIP+" and icmp[icmptype] = icmp-echoreply")
	var frames bytes.Buffer
	capture.Stdout, capture.Stderr = &frames, stderr
	require.NoError(t, capture.Start())
	defer func() { _ = capture.Process.Kill(); _ = capture.Wait() }()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(stderrPath)
		return err == nil && bytes.Contains(data, []byte("listening on"))
	}, time.Second, 10*time.Millisecond, "tcpdump must be listening before the first probe")

	started := time.Now()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	output, pingErr := exec.CommandContext(pingCtx, "ping", "-I", gateway, "-c", "1", "-W", "1", slotIP).CombinedOutput()
	probeDuration := time.Since(started)
	require.NoError(t, capture.Wait(), "capture the first guest reply")
	require.Contains(t, frames.String(), e2ePrepMAC+" > "+guestnetwork.GatewayMAC,
		"the first reply must use the gateway MAC already cached in the snapshot")
	t.Logf("first reply via %s in %s: %s", slotIP, probeDuration, frames.String())
	if pingErr != nil {
		return fmt.Errorf("first ping through slot %s: %w: %s", slotIP, pingErr, output)
	}
	return nil
}
