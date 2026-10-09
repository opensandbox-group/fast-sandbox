package network

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/guestnetwork"
)

func TestGuestVMIP(t *testing.T) {
	slot := &Slot{IP: "10.17.0.2", PrivateCIDR: "10.17.0.0/16"}
	guest, err := GuestVMIP(slot)
	require.NoError(t, err)
	require.Equal(t, "10.17.0.3", guest)

	_, err = GuestVMIP(&Slot{IP: "not-an-ip", PrivateCIDR: "10.17.0.0/16"})
	require.Error(t, err)
	_, err = GuestVMIP(&Slot{IP: "192.168.1.5", PrivateCIDR: "10.17.0.0/16"})
	require.Error(t, err)
	_, err = GuestVMIP(&Slot{IP: "10.17.0.7", PrivateCIDR: "10.17.0.0/29"})
	require.Error(t, err)
	_, err = GuestVMIP(nil)
	require.Error(t, err)
}

// guestVMSlotForTest returns a slot exercising the guest-VM netns data plane.
func guestVMSlotForTest(root string) *Slot {
	return &Slot{
		NetNSName: "ns-1", NetNSPath: root + "/netns/ns-1", HostNetNSPath: root + "/host-netns/ns-1",
		HostVeth: "fh-1", PeerVeth: "fp-1", Bridge: "fsb0",
		Address: "10.17.0.2/16", IP: "10.17.0.2", Gateway: "10.17.0.1",
		PrivateCIDR: "10.17.0.0/16", DNSPath: root + "/dns", MTU: 1400,
		GuestTap: "fc-abc123",
	}
}

// failCheckRunner records commands, fails existence checks (bridge probe and
// iptables -C), so every rule is installed with its -A variant and the full
// command sequence is assertable.
type failCheckRunner struct {
	commands []string
	tapJSON  string
}

func (r *failCheckRunner) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	line := command + " " + strings.Join(args, " ")
	r.commands = append(r.commands, line)
	if strings.Contains(line, "-j -n ns-1 link show dev vmtap0") {
		if r.tapJSON != "" {
			return []byte(r.tapJSON), nil
		}
		return []byte(`[{"address":"` + guestnetwork.GatewayMAC + `"}]`), nil
	}
	if strings.Contains(line, "link show dev fsb0") || strings.Contains(line, " -C ") {
		return nil, errors.New("not found")
	}
	if strings.Contains(line, "route show default") {
		return []byte("default via 10.0.0.1 dev eth0\n"), nil
	}
	return nil, nil
}

func TestGuestVMNetNSDriverPrepare(t *testing.T) {
	root := t.TempDir()
	runner := &failCheckRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := guestVMSlotForTest(root)
	require.NoError(t, driver.Prepare(context.Background(), slot))

	joined := strings.Join(runner.commands, "\n")
	// The tap is created INSIDE the slot netns with the fixed name, no
	// bridge membership.
	require.Contains(t, joined, "ip netns exec ns-1 ip tuntap add dev vmtap0 mode tap")
	require.Contains(t, joined, "ip netns exec ns-1 ip link set vmtap0 address "+guestnetwork.GatewayMAC)
	require.Less(t, strings.Index(joined, "address "+guestnetwork.GatewayMAC), strings.Index(joined, "link set vmtap0 up"))
	require.Contains(t, joined, "ip netns exec ns-1 ip link set vmtap0 mtu 1400")
	require.Contains(t, joined, "ip netns exec ns-1 ip link set vmtap0 up")
	require.NotContains(t, joined, "link set vmtap0 master")
	// Proxy ARP on the tap only (NOT conf.all: the all knob would make
	// eth0 proxy-answer the whole private CIDR and poison the host
	// neighbour cache), so the guest resolves its baked gateway address
	// and its replies and egress flow through the namespace.
	require.Contains(t, joined, "ip netns exec ns-1 sysctl -w net.ipv4.conf.vmtap0.proxy_arp=1")
	require.NotContains(t, joined, "net.ipv4.conf.all.proxy_arp=1")
	// Proxy ARP replies must not pay the default 800ms proxy_delay: the
	// guest's first egress packet after restore blocks on the gateway reply.
	require.Contains(t, joined, "ip netns exec ns-1 sysctl -w net.ipv4.neigh.vmtap0.proxy_delay=0")
	require.Contains(t, joined, "ip netns exec ns-1 sysctl -w net.ipv4.ip_forward=1")
	// Faster ARP re-resolution on both namespace devices (the guest resolves
	// its baked gateway through the proxy-ARP tap; egress resolves the host
	// gateway through eth0).
	require.Contains(t, joined, "ip netns exec ns-1 sysctl -w net.ipv4.neigh.vmtap0.retrans_time_ms=100")
	require.Contains(t, joined, "ip netns exec ns-1 sysctl -w net.ipv4.neigh.eth0.retrans_time_ms=100")
	// The forward rules accept the gateway, guest egress and ingress,
	// reject siblings (only traffic originating from the tap to the private
	// CIDR), and accept established connections.
	require.Contains(t, joined, "ip netns exec ns-1 iptables -A FORWARD -d 10.17.0.1/32 -j ACCEPT")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -A FORWARD -i vmtap0 -o eth0 -d 10.17.0.0/16 -j REJECT")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -A FORWARD -i vmtap0 -o eth0 -j ACCEPT")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -A FORWARD -i eth0 -o vmtap0 -j ACCEPT")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -A FORWARD -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT")
	// The guest-specific NAT rules are applied per restore, not at slot
	// preparation (the baked guest address is unknown when the slot is
	// prepared): no in-namespace DNAT/SNAT/route yet (the host MASQUERADE
	// from the Linux base is unrelated).
	require.NotContains(t, joined, "ip netns exec ns-1 iptables -t nat -A PREROUTING")
	require.NotContains(t, joined, "ip netns exec ns-1 iptables -t nat -A POSTROUTING")
	require.NotContains(t, joined, "ip netns exec ns-1 ip route add")
	// An empty slot tap gets the fixed name for consumers.
	slot.GuestTap = ""
	require.NoError(t, driver.Prepare(context.Background(), slot))
	require.Equal(t, guestVMDefaultTapName, slot.GuestTap)
}

func TestGuestVMNetNSDriverApplyGuest(t *testing.T) {
	root := t.TempDir()
	runner := &failCheckRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := guestVMSlotForTest(root)
	require.NoError(t, driver.Prepare(context.Background(), slot))

	// The baked guest address (manifest guestNetwork) is the shared clone
	// address; it is NOT slot.IP+1 except for the first slot.
	require.NoError(t, driver.ApplyGuest(context.Background(), slot, "10.17.0.9"))
	require.Equal(t, "10.17.0.9", slot.GuestIP)

	joined := strings.Join(runner.commands, "\n")
	// Ingress delivery: /32 route to the baked guest address via the tap
	// (NOT a local address: a local address would shadow the guest).
	require.Contains(t, joined, "ip netns exec ns-1 ip route replace 10.17.0.9/32 dev vmtap0")
	require.NotContains(t, joined, "ip netns exec ns-1 ip addr add 10.17.0.9/32")
	// Ingress DNAT (slot IP -> baked guest IP) and egress source NAT
	// (baked guest IP -> slot IP).
	require.Contains(t, joined, "ip netns exec ns-1 iptables -t nat -A PREROUTING -d 10.17.0.2/32 -j DNAT --to-destination 10.17.0.9")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -t nat -A POSTROUTING -s 10.17.0.9/32 -j SNAT --to-source 10.17.0.2")

	// Invalid baked addresses are rejected and do not mutate the slot.
	require.Error(t, driver.ApplyGuest(context.Background(), slot, "not-an-ip"))
	require.Error(t, driver.ApplyGuest(context.Background(), slot, "fe80::1"))
	require.Equal(t, "10.17.0.9", slot.GuestIP)
}

func TestBakedGuestIP(t *testing.T) {
	ip, err := BakedGuestIP(&Slot{Gateway: "10.17.0.1"})
	require.NoError(t, err)
	require.Equal(t, "10.17.0.3", ip)

	_, err = BakedGuestIP(nil)
	require.Error(t, err)
	_, err = BakedGuestIP(&Slot{Gateway: "not-an-ip"})
	require.Error(t, err)
}

func TestGuestVMNetNSDriverDestroy(t *testing.T) {
	runner := &failCheckRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := &Slot{
		NetNSName: "ns-1", HostVeth: "fh-1",
		Address: "10.17.0.2/16", IP: "10.17.0.2", PrivateCIDR: "10.17.0.0/16",
		Gateway: "10.17.0.1", GuestTap: "fc-abc123", GuestIP: "10.17.0.9",
	}
	require.NoError(t, driver.Destroy(context.Background(), slot))

	joined := strings.Join(runner.commands, "\n")
	// The rules are deleted inside the namespace before the netns itself:
	// the applied guest NAT rules first, then the static forward rules.
	require.Contains(t, joined, "ip netns exec ns-1 iptables -t nat -D PREROUTING -d 10.17.0.2/32 -j DNAT --to-destination 10.17.0.9")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -t nat -D POSTROUTING -s 10.17.0.9/32 -j SNAT --to-source 10.17.0.2")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -D FORWARD -i vmtap0 -o eth0 -j ACCEPT")
	require.Contains(t, joined, "ip netns exec ns-1 iptables -D FORWARD -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT")
	require.Contains(t, joined, "ip netns delete ns-1")
	// The tap lives inside the namespace and vanishes with it; no explicit
	// tap deletion (the host veth deletion is the Linux base destroy).
	require.NotContains(t, joined, "link del vmtap0")
	require.NotContains(t, joined, "link delete vmtap0")
}

func TestGuestVMNetNSDriverValidate(t *testing.T) {
	root := t.TempDir()
	netnsPath := root + "/ns-1"
	dnsPath := root + "/resolv.conf"
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))
	require.NoError(t, os.MkdirAll(netnsPath, 0o755))
	runner := &failCheckRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := guestVMSlotForTest(root)
	slot.NetNSPath = netnsPath
	slot.DNSPath = dnsPath
	require.NoError(t, driver.Validate(context.Background(), slot))

	joined := strings.Join(runner.commands, "\n")
	require.Contains(t, joined, "ip -j -n ns-1 link show dev vmtap0")

	for _, payload := range []string{`[{"address":"d6:1f:c2:4e:4d:6e"}]`, `[]`, `[{"address":""}]`, `invalid json`} {
		runner.tapJSON = payload
		require.Error(t, driver.Validate(context.Background(), slot), payload)
	}
	// Validation rejects stale identities without rewriting active interfaces.
	for _, command := range runner.commands {
		require.NotContains(t, command, "link set")
	}

	slot.GuestTap = ""
	require.Error(t, driver.Validate(context.Background(), slot))
}
