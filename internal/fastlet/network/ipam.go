package network

import (
	"fmt"
	"net/netip"
)

type IPv4IPAM struct {
	prefix  netip.Prefix
	gateway netip.Addr
}

func NewIPv4IPAM(cidr string) (*IPv4IPAM, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse private CIDR: %w", err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("private CIDR %q is not IPv4", cidr)
	}
	if prefix.Bits() > 29 {
		return nil, fmt.Errorf("private CIDR %q has no usable sandbox addresses", cidr)
	}
	gateway := prefix.Addr().Next()
	return &IPv4IPAM{prefix: prefix, gateway: gateway}, nil
}

func (i *IPv4IPAM) CIDR() string { return i.prefix.String() }

func (i *IPv4IPAM) Gateway() string { return i.gateway.String() }

func (i *IPv4IPAM) GatewayPrefix() string {
	return netip.PrefixFrom(i.gateway, i.prefix.Bits()).String()
}

func (i *IPv4IPAM) Allocate(used map[string]struct{}) (string, string, error) {
	for address := i.gateway.Next(); i.prefix.Contains(address); address = address.Next() {
		// The final address in an IPv4 prefix is the broadcast address.
		if !i.prefix.Contains(address.Next()) {
			break
		}
		if _, exists := used[address.String()]; exists {
			continue
		}
		return address.String(), netip.PrefixFrom(address, i.prefix.Bits()).String(), nil
	}
	return "", "", ErrNoCleanSlot
}
