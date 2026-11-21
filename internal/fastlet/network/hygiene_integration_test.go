//go:build linux

package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests create only private, uniquely named netns devices. No host
// routes, bridge identities outside the fsbtest0 test bridge, or host
// firewall rules are changed beyond the shared MASQUERADE idempotency the
// regular privileged driver test already installs.

// TestHygieneListenerHelper binds a port inside the slot namespace so the
// audit's listener scan has something to find. It runs only when the test
// harness spawns it explicitly (FSB_RESIDUE_HELPER=listener).
func TestHygieneListenerHelper(t *testing.T) {
	if os.Getenv("FSB_RESIDUE_HELPER") != "listener" {
		t.Skip("residue helper; run through the privileged injection test")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:47231")
	require.NoError(t, err)
	defer listener.Close()
	time.Sleep(30 * time.Second)
}

// TestHygieneConntrackHelper sends one UDP datagram from inside the slot
// namespace so the namespace conntrack table holds an entry the deep reset
// must flush (FSB_RESIDUE_HELPER=conntrack).
func TestHygieneConntrackHelper(t *testing.T) {
	if os.Getenv("FSB_RESIDUE_HELPER") != "conntrack" {
		t.Skip("residue helper; run through the privileged injection test")
	}
	connection, err := net.DialTimeout("udp", "10.99.0.5:9999", time.Second)
	require.NoError(t, err)
	_, err = connection.Write([]byte("residue"))
	require.NoError(t, err)
	require.NoError(t, connection.Close())
}

// startDetached starts a process in the slot namespace without holding the
// capture pipes open, so a long-lived resident does not block the test.
func startDetached(t *testing.T, name string, arguments ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command
}

func spawnInNamespace(t *testing.T, nsName, helper string) *exec.Cmd {
	t.Helper()
	helperTest := map[string]string{"listener": "TestHygieneListenerHelper", "conntrack": "TestHygieneConntrackHelper"}[helper]
	require.NotEmpty(t, helperTest)
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.Command("ip", "netns", "exec", nsName, "env", "FSB_RESIDUE_HELPER="+helper,
		executable, "-test.run", "^"+helperTest+"$")
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command
}

// TestPooledSlotResidueHygienePrivileged runs the injection-audit method of
// the pooling proposal against a real slot namespace: residue classes are
// deliberately injected, the audit must catch every one of them, the deep
// reset must return the namespace to the golden state, and the kernel
// conntrack table must be flushed by the reset.
func TestPooledSlotResidueHygienePrivileged(t *testing.T) {
	if os.Getenv("FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST") != "1" {
		t.Skip("set FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST=1 in a disposable privileged Linux environment")
	}
	require.Zero(t, os.Geteuid())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runner := ExecRunner{}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	netnsName := "fsbhyg-" + suffix
	stateRoot := t.TempDir()
	slot := &Slot{
		Version: 1, ID: "slot-" + suffix, OwnerPodUID: "integration", Phase: SlotPhaseClean,
		NetNSName: netnsName, NetNSPath: "/run/netns/" + netnsName,
		HostNetNSPath: "/run/netns/" + netnsName,
		HostVeth:      "fh" + suffix, PeerVeth: "fp" + suffix, Bridge: "fsbtest0",
		Address: "172.30.254.2/29", IP: "172.30.254.2", Gateway: "172.30.254.1",
		PrivateCIDR: "172.30.254.0/29", DNSPath: stateRoot + "/slot.resolv.conf", MTU: 1400,
	}
	driver := NewLinuxNetNSDriver(LinuxDriverConfig{})
	require.NoError(t, driver.Prepare(ctx, slot))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		require.NoError(t, driver.Destroy(cleanupCtx, slot))
	})
	golden, err := driver.CaptureGolden(ctx, slot)
	require.NoError(t, err)

	cleanAudit := func(context string) {
		violations, err := driver.Audit(ctx, slot, golden)
		require.NoError(t, err, context)
		require.Emptyf(t, violations, "%s: %v", context, violations)
	}
	cleanAudit("freshly prepared slot")

	// One representative of every injectable residue class at once. The
	// admin-down injection comes last because routes and shaping qdiscs need
	// the device up.
	injections := [][]string{
		{"-n", netnsName, "addr", "del", slot.IP + "/29", "dev", "eth0"},
		{"-n", netnsName, "addr", "add", "10.66.6.6/32", "dev", "eth0"},
		{"-n", netnsName, "route", "add", "blackhole", "10.88.0.0/16"},
		{"-n", netnsName, "route", "add", "10.77.0.0/16", "dev", "eth0"},
		{"-n", netnsName, "link", "set", "dev", "eth0", "address", "aa:bb:cc:dd:ee:02"},
		{"netns", "exec", netnsName, "tc", "qdisc", "add", "dev", "eth0", "root", "netem", "delay", "100ms"},
		{"netns", "exec", netnsName, "sysctl", "-q", "-w", "net.ipv4.ip_forward=0"},
		// Taking the link down flushes its neighbour entries, so the stray
		// permanent entry is injected afterwards.
		{"-n", netnsName, "link", "set", "dev", "eth0", "down"},
		{"-n", netnsName, "neigh", "replace", "172.30.254.9", "lladdr", "aa:bb:cc:dd:ee:01", "dev", "eth0", "nud", "permanent"},
	}
	for _, injection := range injections {
		_, err := runner.Run(ctx, "ip", injection...)
		require.NoErrorf(t, err, "inject %v", injection)
	}
	// Give the tenant-style MAC drift time to also surface as derived
	// link-local state before auditing.
	time.Sleep(200 * time.Millisecond)

	violations, err := driver.Audit(ctx, slot, golden)
	require.NoError(t, err)
	domains := map[string]bool{}
	for _, violation := range violations {
		domains[violation.Domain] = true
	}
	for _, domain := range []string{"link", "address", "route", "neighbour", "qdisc", "sysctl"} {
		require.Truef(t, domains[domain], "audit must report the %s residue domain: %v", domain, violations)
	}

	require.NoError(t, driver.DeepReset(ctx, slot, golden))
	cleanAudit("after deep reset of the structural residue")

	// Firewall rules live inside the slot namespace: tampering is detected
	// by the audit. The reset never rewrites firewall policy, so a rule
	// violation quarantines the slot at the manager level; the test restores
	// the prepare-time rule set by hand to continue the clean path.
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-D", "OUTPUT", "-d", slot.Gateway+"/32", "-j", "ACCEPT")
	require.NoError(t, err)
	violations, err = driver.Audit(ctx, slot, golden)
	require.NoError(t, err)
	domains = map[string]bool{}
	for _, violation := range violations {
		domains[violation.Domain] = true
	}
	require.Truef(t, domains["firewall"], "audit must report removed firewall rules: %v", violations)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-F", "OUTPUT")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-A", "OUTPUT", "-d", slot.Gateway+"/32", "-j", "ACCEPT")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-A", "OUTPUT", "-d", slot.PrivateCIDR, "-j", "REJECT")
	require.NoError(t, err)
	cleanAudit("after manual firewall restore")

	// Foreign members: a leftover listener socket and a leftover resident
	// process are both caught by the member scan.
	listenerHelper := spawnInNamespace(t, netnsName, "listener")
	// The resident process is started detached: a background job under a
	// piped shell would keep the pipe (and the Run call) open for its whole
	// lifetime.
	resident := startDetached(t, "ip", "netns", "exec", netnsName, "sleep", "600")
	_ = resident
	time.Sleep(300 * time.Millisecond)
	violations, err = driver.Audit(ctx, slot, golden)
	require.NoError(t, err)
	domains = map[string]bool{}
	for _, violation := range violations {
		domains[violation.Domain] = true
	}
	require.True(t, domains["socket"], "audit must report the leftover listener: %v", violations)
	require.True(t, domains["foreign"], "audit must report the resident process: %v", violations)
	// Cleaning the members by hand is what the quarantine path would do via
	// destroy; the reset itself never kills processes. Zombies keep their
	// namespace reference until reaped, so the listener helper is waited for.
	require.NoError(t, listenerHelper.Process.Kill())
	_, _ = listenerHelper.Process.Wait()
	killMembers := func() ([]int, error) {
		members, err := foreignMembers("/proc", slot.NetNSPath)
		if err != nil {
			return nil, err
		}
		for _, pid := range members {
			if process, findErr := os.FindProcess(pid); findErr == nil {
				_ = process.Kill()
			}
		}
		return members, nil
	}
	_, err = killMembers()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		violations, auditErr := driver.Audit(ctx, slot, golden)
		return auditErr == nil && len(violations) == 0
	}, 5*time.Second, 50*time.Millisecond, "member-free slot must audit clean")

	// Kernel-internal residue: entries the userspace audit cannot see are
	// flushed by the reset.
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-A", "OUTPUT", "-m", "conntrack", "--ctstate", "NEW", "-j", "ACCEPT")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "-n", netnsName, "route", "add", "10.99.0.0/16", "dev", "eth0")
	require.NoError(t, err)
	conntrackHelper := spawnInNamespace(t, netnsName, "conntrack")
	require.NoError(t, conntrackHelper.Wait())
	// The conntrack match rule is itself firewall residue: restore the
	// prepare-time OUTPUT chain before the reset so only the kernel-internal
	// domain is under test.
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-F", "OUTPUT")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-A", "OUTPUT", "-d", slot.Gateway+"/32", "-j", "ACCEPT")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-A", "OUTPUT", "-d", slot.PrivateCIDR, "-j", "REJECT")
	require.NoError(t, err)
	count := func() string {
		output, err := runner.Run(ctx, "ip", "netns", "exec", netnsName, "cat", "/proc/sys/net/netfilter/nf_conntrack_count")
		require.NoError(t, err)
		return strings.TrimSpace(string(output))
	}
	require.NotEqual(t, "0", count(), "the helper must have created a conntrack entry")
	require.NoError(t, driver.DeepReset(ctx, slot, golden))
	cleanAudit("after conntrack flush")
	require.Equal(t, "0", count(), "the deep reset must flush the conntrack table")
}

// TestPooledGuestSlotResidueHygienePrivileged covers the guest-VM data
// plane: per-restore guest NAT rules of a previous tenant must be replaced
// by the prepare-time chains before the slot is reused.
func TestPooledGuestSlotResidueHygienePrivileged(t *testing.T) {
	if os.Getenv("FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST") != "1" {
		t.Skip("set FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST=1 in a disposable privileged Linux environment")
	}
	require.Zero(t, os.Geteuid())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runner := ExecRunner{}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	netnsName := "fsbhygg-" + suffix
	stateRoot := t.TempDir()
	slot := &Slot{
		Version: 1, ID: "gslot-" + suffix, OwnerPodUID: "integration", Phase: SlotPhaseClean,
		NetNSName: netnsName, NetNSPath: "/run/netns/" + netnsName,
		HostNetNSPath: "/run/netns/" + netnsName,
		HostVeth:      "fh" + suffix, PeerVeth: "fp" + suffix, Bridge: "fsbtest0",
		Address: "172.30.255.2/29", IP: "172.30.255.2", Gateway: "172.30.255.1",
		PrivateCIDR: "172.30.255.0/29", DNSPath: stateRoot + "/gslot.resolv.conf", MTU: 1400,
		GuestTap: guestVMDefaultTapName,
	}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{})
	require.NoError(t, driver.Prepare(ctx, slot))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		require.NoError(t, driver.Destroy(cleanupCtx, slot))
	})
	golden, err := driver.CaptureGolden(ctx, slot)
	require.NoError(t, err)
	violations, err := driver.Audit(ctx, slot, golden)
	require.NoError(t, err)
	require.Emptyf(t, violations, "freshly prepared guest slot: %v", violations)

	// Bind-time tenant state: guest delivery route and per-tenant NAT rules.
	require.NoError(t, driver.ApplyGuest(ctx, slot, "172.30.255.3"))
	violations, err = driver.Audit(ctx, slot, golden)
	require.NoError(t, err)
	require.NotEmptyf(t, violations, "a bound slot must not audit clean against the golden state: %v", violations)

	// Extra residue on top of the binding: shaping qdisc, sysctl drift, and
	// a tenant DNAT rule inside the dedicated chain.
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "tc", "qdisc", "add", "dev", guestVMDefaultTapName, "root", "netem", "delay", "100ms")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "sysctl", "-q", "-w", "net.ipv4.conf."+guestVMDefaultTapName+".proxy_arp=0")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-t", "nat", "-A", guestDNATChain, "-d", slot.IP+"/32", "-j", "DNAT", "--to-destination", "10.9.9.9")
	require.NoError(t, err)

	require.NoError(t, driver.DeepReset(ctx, slot, golden))
	violations, err = driver.Audit(ctx, slot, golden)
	require.NoError(t, err)
	require.Emptyf(t, violations, "deep reset must return the guest slot to the golden state: %v", violations)

	// The dedicated chains are back to prepare-time state.
	output, err := runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-t", "nat", "-S", guestDNATChain)
	require.NoError(t, err)
	require.Equal(t, "-N "+guestDNATChain, strings.TrimSpace(string(output)))
	output, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-t", "nat", "-C", "PREROUTING", "-j", guestDNATChain)
	require.NoError(t, err, "the DNAT hook must survive the reset")
}
