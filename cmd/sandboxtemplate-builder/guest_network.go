package main

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
)

const networkCIDREnv = "FAST_SANDBOX_NETWORK_CIDR"

type bakedGuestNetwork struct {
	MAC     string
	IP      string
	Gateway string
	Netmask string
	MTU     int
}

func resolveBakedGuestNetwork() (bakedGuestNetwork, error) {
	return resolveBakedGuestNetworkFromCIDR(os.Getenv(networkCIDREnv))
}

func resolveBakedGuestNetworkFromCIDR(raw string) (bakedGuestNetwork, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bakedGuestNetwork{
			MAC:     bakedGuestMAC,
			IP:      bakedGuestIP,
			Gateway: bakedGuestGateway,
			Netmask: bakedGuestNetmask,
			MTU:     bakedGuestMTU,
		}, nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return bakedGuestNetwork{}, fmt.Errorf("parse %s %q: %w", networkCIDREnv, raw, err)
	}
	if !prefix.Addr().Is4() {
		return bakedGuestNetwork{}, fmt.Errorf("%s %q must be an IPv4 CIDR", networkCIDREnv, raw)
	}
	bits := prefix.Bits()
	if bits < 8 || bits > 29 {
		return bakedGuestNetwork{}, fmt.Errorf("%s %q prefix length /%d must be between /8 and /29", networkCIDREnv, raw, bits)
	}
	base := prefix.Masked().Addr().As4()
	gw := base
	gw[3]++
	guest := base
	guest[3] += 3
	mask := net.CIDRMask(bits, 32)
	return bakedGuestNetwork{
		MAC:     bakedGuestMAC,
		IP:      netip.AddrFrom4(guest).String(),
		Gateway: netip.AddrFrom4(gw).String(),
		Netmask: fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3]),
		MTU:     bakedGuestMTU,
	}, nil
}
