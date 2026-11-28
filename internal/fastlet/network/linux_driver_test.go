package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *recordingRunner) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	line := command + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.commands = append(r.commands, line)
	r.mu.Unlock()
	if line == "ip link show dev fsb0" || strings.Contains(line, "iptables -t nat -C") {
		return nil, errors.New("not found")
	}
	if line == "ip -4 route show default" {
		return []byte("default via 10.0.0.1 dev eth0\n"), nil
	}
	return nil, nil
}

func TestLinuxNetNSDriverPrepareBuildsIsolatedNetwork(t *testing.T) {
	root := t.TempDir()
	resolver := filepath.Join(root, "resolv.conf")
	require.NoError(t, os.WriteFile(resolver, []byte("nameserver 10.96.0.10\n"), 0o644))
	runner := &recordingRunner{}
	driver := NewLinuxNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: resolver})
	slot := &Slot{
		ID: "slot-a", NetNSName: "fsb-test", NetNSPath: filepath.Join(root, "netns", "fsb-test"),
		HostNetNSPath: "/run/fast-sandbox/netns/fsb-test", HostVeth: "fh123", PeerVeth: "fp123",
		Bridge: "fsb0", Address: "172.30.0.2/24", IP: "172.30.0.2", Gateway: "172.30.0.1",
		PrivateCIDR: "172.30.0.0/24", DNSPath: filepath.Join(root, "state", "slot-a.resolv.conf"), MTU: 1450,
	}
	require.NoError(t, driver.Prepare(context.Background(), slot))
	require.Equal(t, "eth0", slot.EgressDevice)
	resolverData, err := os.ReadFile(slot.DNSPath)
	require.NoError(t, err)
	require.Contains(t, string(resolverData), "10.96.0.10")

	joined := strings.Join(runner.commands, "\n")
	require.Contains(t, joined, "ip netns add fsb-test")
	require.Contains(t, joined, "ip link add fh123 mtu 1450 type veth peer name fp123")
	require.Contains(t, joined, "ip -n fsb-test addr add 172.30.0.2/24 dev eth0")
	require.Contains(t, joined, "iptables -t nat -A POSTROUTING")
	require.Contains(t, joined, "ip netns exec fsb-test iptables -A OUTPUT -d 172.30.0.1/32 -j ACCEPT")
	require.Contains(t, joined, "ip netns exec fsb-test iptables -A OUTPUT -d 172.30.0.0/24 -j REJECT")
	// The shared bridge gets faster ARP re-resolution so stale neighbour
	// entries recover in ~100 ms instead of seconds.
	require.Contains(t, joined, "sysctl -w net.ipv4.neigh.fsb0.retrans_time_ms=100")
}

func TestLinuxNetNSDriverDestroyRemovesBridgeNeighbour(t *testing.T) {
	runner := &recordingRunner{}
	driver := NewLinuxNetNSDriver(LinuxDriverConfig{Runner: runner})
	slot := &Slot{
		NetNSName: "fsb-test", HostVeth: "fh123", Bridge: "fsb0",
		Address: "172.30.0.2/24", IP: "172.30.0.2", PrivateCIDR: "172.30.0.0/24",
		Gateway: "172.30.0.1", NetNSPath: filepath.Join(t.TempDir(), "netns", "fsb-test"),
	}
	require.NoError(t, driver.Destroy(context.Background(), slot))

	joined := strings.Join(runner.commands, "\n")
	// The bridge neighbour entry for the slot IP must not outlive the veth:
	// a stale entry keeps forwarding to the dead MAC and drops traffic for
	// the next owner of the reused private address.
	require.Contains(t, joined, "ip link delete fh123")
	require.Contains(t, joined, "ip neigh del 172.30.0.2 dev fsb0")
	require.Contains(t, joined, "ip netns delete fsb-test")
}

func TestDefaultRouteDevice(t *testing.T) {
	require.Equal(t, "ens5", defaultRouteDevice("default via 192.0.2.1 dev ens5 proto dhcp"))
	require.Empty(t, defaultRouteDevice("unreachable default"))
}

func TestMissingNetworkResourceRecognizesIdempotentIPLinkDelete(t *testing.T) {
	require.True(t, isMissingNetworkResource(errors.New("ip link delete fh123: exit status 2: RTNETLINK answers: No such device")))
	require.False(t, isMissingNetworkResource(errors.New("ip link delete fh123: permission denied")))
	require.False(t, isMissingNetworkResource(nil))
}

type scriptedNetNSDeleteRunner struct {
	mu     sync.Mutex
	calls  int
	errors []error
}

func (r *scriptedNetNSDeleteRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.calls
	r.calls++
	if idx < len(r.errors) {
		return nil, r.errors[idx]
	}
	return nil, nil
}

func TestDeleteNetNSWithRetryFastFailsOnMissingAndRetriesBusy(t *testing.T) {
	missing := &scriptedNetNSDeleteRunner{
		errors: []error{errors.New(`ip netns delete fsb-missing: exit status 1: Cannot open network namespace "fsb-missing": No such file or directory`)},
	}
	started := time.Now()
	require.NoError(t, deleteNetNSWithRetry(context.Background(), missing, "ip", "fsb-missing"))
	require.Equal(t, 1, missing.calls, "missing netns must not retry")
	require.Less(t, time.Since(started), 100*time.Millisecond)

	busyThenOK := &scriptedNetNSDeleteRunner{
		errors: []error{
			errors.New(`Cannot remove namespace file "/run/netns/fsb-busy": Device or resource busy`),
			errors.New(`Cannot remove namespace file "/run/netns/fsb-busy": Device or resource busy`),
			nil,
		},
	}
	require.NoError(t, deleteNetNSWithRetry(context.Background(), busyThenOK, "ip", "fsb-busy"))
	require.Equal(t, 3, busyThenOK.calls)
}

func TestLinuxNetNSDriverConcurrentPrepareInitializesHostOnce(t *testing.T) {
	root := t.TempDir()
	resolver := filepath.Join(root, "resolv.conf")
	require.NoError(t, os.WriteFile(resolver, []byte("nameserver 10.96.0.10\n"), 0o644))

	runner := &recordingRunner{}
	driver := NewLinuxNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: resolver})

	const workers = 8
	slots := make([]*Slot, workers)
	var wg sync.WaitGroup
	for i := range workers {
		slot := &Slot{
			ID: fmt.Sprintf("slot-%d", i), NetNSName: fmt.Sprintf("fsb-%d", i),
			NetNSPath:     filepath.Join(root, "netns", fmt.Sprintf("fsb-%d", i)),
			HostNetNSPath: fmt.Sprintf("/run/fast-sandbox/netns/fsb-%d", i),
			HostVeth:      fmt.Sprintf("fh%d", i), PeerVeth: fmt.Sprintf("fp%d", i),
			Bridge: "fsb0", Address: fmt.Sprintf("172.30.0.%d/24", i+2),
			IP: fmt.Sprintf("172.30.0.%d", i+2), Gateway: "172.30.0.1",
			PrivateCIDR: "172.30.0.0/24",
			DNSPath:     filepath.Join(root, "state", fmt.Sprintf("slot-%d.resolv.conf", i)),
			MTU:         1500,
		}
		slots[i] = slot
		wg.Add(1)
		go func(target *Slot) {
			defer wg.Done()
			require.NoError(t, driver.Prepare(context.Background(), target))
		}(slot)
	}
	wg.Wait()

	for _, slot := range slots {
		require.Equal(t, "eth0", slot.EgressDevice)
	}
	runner.mu.Lock()
	bridgeAdds := 0
	masqueradeAdds := 0
	for _, cmd := range runner.commands {
		if cmd == "ip link add fsb0 type bridge" {
			bridgeAdds++
		}
		if strings.Contains(cmd, "iptables -t nat -A POSTROUTING") {
			masqueradeAdds++
		}
	}
	runner.mu.Unlock()
	require.Equal(t, 1, bridgeAdds)
	require.Equal(t, 1, masqueradeAdds)
}

type missingNetNSRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *missingNetNSRunner) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	line := command + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.commands = append(r.commands, line)
	r.mu.Unlock()
	if strings.Contains(line, "netns") || strings.Contains(line, "link delete") {
		return nil, errors.New(`Cannot open network namespace "ns-missing": No such file or directory`)
	}
	return nil, nil
}

func TestGuestVMNetNSDriverDestroyShortCircuitsMissingNetNS(t *testing.T) {
	runner := &missingNetNSRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner})
	slot := &Slot{
		NetNSName: "ns-missing", HostVeth: "fh-missing", Bridge: "fsb0",
		Address: "10.17.0.2/16", IP: "10.17.0.2", PrivateCIDR: "10.17.0.0/16",
		Gateway: "10.17.0.1", GuestTap: "vmtap0", GuestIP: "10.17.0.3",
	}
	started := time.Now()
	require.NoError(t, driver.Destroy(context.Background(), slot))
	require.Less(t, time.Since(started), 100*time.Millisecond)

	iptablesExecs := 0
	netnsDeletes := 0
	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "ip netns exec ns-missing iptables") {
			iptablesExecs++
		}
		if strings.Contains(cmd, "ip netns delete ns-missing") {
			netnsDeletes++
		}
	}
	require.Equal(t, 1, iptablesExecs, "must stop rule cleanup once netns is confirmed absent")
	require.Equal(t, 1, netnsDeletes, "must not retry deleting an absent netns")
}
