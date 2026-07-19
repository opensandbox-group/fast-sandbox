package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
}

func TestDefaultRouteDevice(t *testing.T) {
	require.Equal(t, "ens5", defaultRouteDevice("default via 192.0.2.1 dev ens5 proto dhcp"))
	require.Empty(t, defaultRouteDevice("unreachable default"))
}
