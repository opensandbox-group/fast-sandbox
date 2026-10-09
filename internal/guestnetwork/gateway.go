// Package guestnetwork defines network identities shared by snapshot producers
// and the guest-VM runtime.
package guestnetwork

// GatewayMAC is the locally administered unicast MAC of the guest-facing TAP.
// Each TAP is isolated in its own network namespace. Keeping this identity
// stable across template builds and restores preserves the gateway neighbour
// entries saved in guest memory. It must differ from the baked guest NIC MAC.
const GatewayMAC = "02:00:00:00:00:02"
