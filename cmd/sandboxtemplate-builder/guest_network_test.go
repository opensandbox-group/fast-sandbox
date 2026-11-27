package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveBakedGuestNetworkFromCIDR(t *testing.T) {
	t.Run("default empty uses /24", func(t *testing.T) {
		got, err := resolveBakedGuestNetworkFromCIDR("")
		require.NoError(t, err)
		require.Equal(t, bakedGuestIP, got.IP)
		require.Equal(t, bakedGuestGateway, got.Gateway)
		require.Equal(t, "255.255.255.0", got.Netmask)
		require.Equal(t, bakedGuestMAC, got.MAC)
		require.Equal(t, bakedGuestMTU, got.MTU)
	})

	t.Run("custom /20 derives matching netmask for 500+ slot fastlets", func(t *testing.T) {
		got, err := resolveBakedGuestNetworkFromCIDR("172.30.0.0/20")
		require.NoError(t, err)
		require.Equal(t, "172.30.0.3", got.IP)
		require.Equal(t, "172.30.0.1", got.Gateway)
		require.Equal(t, "255.255.240.0", got.Netmask)
	})

	t.Run("custom /16 subnet derives gateway and guest IP from network address", func(t *testing.T) {
		got, err := resolveBakedGuestNetworkFromCIDR("10.17.0.5/16")
		require.NoError(t, err)
		require.Equal(t, "10.17.0.3", got.IP)
		require.Equal(t, "10.17.0.1", got.Gateway)
		require.Equal(t, "255.255.0.0", got.Netmask)
	})

	t.Run("rejects invalid, IPv6, and too-small CIDRs", func(t *testing.T) {
		for _, bad := range []string{"not-a-cidr", "fd00::/64", "172.30.0.0/30", "10.0.0.0/7"} {
			_, err := resolveBakedGuestNetworkFromCIDR(bad)
			require.Error(t, err, "expected error for %q", bad)
		}
	})
}

func TestBakedNetworkBootArgsUsesConfiguredCIDR(t *testing.T) {
	t.Setenv(networkCIDREnv, "172.30.0.0/20")
	got, err := bakedNetworkBootArgs("console=ttyS0 root=/dev/vda rw")
	require.NoError(t, err)
	require.Contains(t, got, "ip=172.30.0.3::172.30.0.1:255.255.240.0::eth0:off")
}

func TestBakedNetworkBootArgsRejectsInvalidCIDR(t *testing.T) {
	t.Setenv(networkCIDREnv, "172.30.0.0/30")
	_, err := bakedNetworkBootArgs("console=ttyS0 root=/dev/vda rw")
	require.Error(t, err)
}
