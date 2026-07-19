//go:build linux

package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLinuxNetNSDriverPrivileged(t *testing.T) {
	if os.Getenv("FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST") != "1" {
		t.Skip("set FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST=1 in a disposable privileged Linux environment")
	}
	if os.Geteuid() != 0 {
		t.Fatal("privileged network test must run as root")
	}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	netnsName := "fsbit-" + suffix
	stateRoot := t.TempDir()
	slot := &Slot{
		Version: 1, ID: "slot-" + suffix, OwnerPodUID: "integration", Phase: SlotPhaseClean,
		NetNSName: netnsName, NetNSPath: filepath.Join("/run/netns", netnsName),
		HostNetNSPath: filepath.Join("/run/netns", netnsName),
		HostVeth:      "fh" + suffix, PeerVeth: "fp" + suffix, Bridge: "fsbtest0",
		Address: "172.30.253.2/29", IP: "172.30.253.2", Gateway: "172.30.253.1",
		PrivateCIDR: "172.30.253.0/29", DNSPath: filepath.Join(stateRoot, "resolv.conf"), MTU: 1400,
	}
	driver := NewLinuxNetNSDriver(LinuxDriverConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, driver.Prepare(ctx, slot))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		require.NoError(t, driver.Destroy(cleanupCtx, slot))
	})
	require.NoError(t, driver.Validate(ctx, slot))

	runner := ExecRunner{}
	route, err := runner.Run(ctx, "ip", "-n", netnsName, "route", "show", "default")
	require.NoError(t, err)
	require.Contains(t, string(route), "via 172.30.253.1")
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-C", "OUTPUT",
		"-d", slot.Gateway+"/32", "-j", "ACCEPT")
	require.NoError(t, err)
	_, err = runner.Run(ctx, "ip", "netns", "exec", netnsName, "iptables", "-C", "OUTPUT",
		"-d", slot.PrivateCIDR, "-j", "REJECT")
	require.NoError(t, err)
}
