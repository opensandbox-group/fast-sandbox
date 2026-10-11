//go:build linux

package network

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This test creates only a private, uniquely named netns. No host routes,
// bridge, IPAM range, or host firewall rules are changed.
func TestGuestNATBatchPrivileged(t *testing.T) {
	if os.Getenv("FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux environment")
	}
	require.Zero(t, os.Geteuid())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runner := ExecRunner{}
	name := fmt.Sprintf("fsbnat-%x", time.Now().UnixNano())
	_, err := runner.Run(ctx, "ip", "netns", "add", name)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, err := runner.Run(cleanup, "ip", "netns", "delete", name)
		require.NoError(t, err)
	})
	_, err = runner.Run(ctx, "ip", "-n", name, "link", "add", guestVMDefaultTapName, "type", "dummy")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "-n", name, "link", "set", guestVMDefaultTapName, "up")
	require.NoError(t, err)
	slot := &Slot{NetNSName: name, IP: "172.30.253.2", GuestNATBatch: true}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{})
	require.True(t, driver.batchNAT)
	initial := fmt.Sprintf("*nat\n:%s - [0:0]\n:%s - [0:0]\n:UNRELATED - [0:0]\n-A PREROUTING -j %s\n-A POSTROUTING -j %s\n-A UNRELATED -d 198.51.100.1/32 -j RETURN\nCOMMIT\n", guestDNATChain, guestSNATChain, guestDNATChain, guestSNATChain)
	require.NoError(t, driver.restoreGuestNAT(ctx, slot, initial))
	_, err = runner.Run(ctx, "ip", "netns", "exec", name, "iptables", "-A", "FORWARD", "-d", "198.51.100.1/32", "-j", "REJECT")
	require.NoError(t, err)
	save := func() string {
		t.Helper()
		output, err := runner.Run(ctx, "ip", "netns", "exec", name, "iptables-save")
		require.NoError(t, err)
		// Keep rule/chain lines; timestamps and counters are not semantic state.
		lines := []string{}
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "-A ") || strings.HasPrefix(line, ":FSB_") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	require.NoError(t, driver.ApplyGuest(ctx, slot, "172.30.253.3"))
	before := save()
	require.NoError(t, driver.ApplyGuest(ctx, slot, "172.30.253.3"))
	require.Equal(t, before, save())
	require.Equal(t, 1, strings.Count(before, "-A FSB_GUEST_DNAT "))
	require.Equal(t, 1, strings.Count(before, "-A FSB_GUEST_SNAT "))
	require.Equal(t, 1, strings.Count(before, "-A PREROUTING -j FSB_GUEST_DNAT"))
	require.Contains(t, before, "-A UNRELATED -d 198.51.100.1/32 -j RETURN")
	require.Contains(t, before, "-A FORWARD -d 198.51.100.1/32 -j REJECT")
	// Failed transaction must leave the existing DNAT/SNAT and foreign rules intact.
	invalid := fmt.Sprintf("*nat\n:%s - [0:0]\n:%s - [0:0]\n-A %s -j NONEXISTENT\nCOMMIT\n", guestDNATChain, guestSNATChain, guestDNATChain)
	require.Error(t, driver.restoreGuestNAT(ctx, slot, invalid))
	require.Equal(t, before, save())
	require.NoError(t, driver.ApplyGuest(ctx, slot, "172.30.253.4"))
	after := save()
	require.NotContains(t, after, "172.30.253.3")
	require.Contains(t, after, "--to-destination 172.30.253.4")
	require.Equal(t, 1, strings.Count(after, "-A FSB_GUEST_DNAT "))
}
