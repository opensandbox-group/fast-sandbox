package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIPv4IPAMAllocatesUsableAddresses(t *testing.T) {
	ipam, err := NewIPv4IPAM("172.30.0.0/29")
	require.NoError(t, err)
	require.Equal(t, "172.30.0.1", ipam.Gateway())

	used := map[string]struct{}{"172.30.0.2": {}}
	ip, address, err := ipam.Allocate(used)
	require.NoError(t, err)
	require.Equal(t, "172.30.0.3", ip)
	require.Equal(t, "172.30.0.3/29", address)
}

func TestIPv4IPAMRejectsTooSmallPrefix(t *testing.T) {
	_, err := NewIPv4IPAM("172.30.0.0/30")
	require.Error(t, err)
}
