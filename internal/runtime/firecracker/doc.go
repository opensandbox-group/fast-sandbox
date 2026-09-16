// Package firecracker implements the direct Firecracker runtime driver for
// Fastlet: one microVM restored from a golden snapshot per Sandbox create
// request (restore is the only startup path; an instance checkpoint resume
// shares the same machinery), launched as a child process (jailer-chrooted
// when configured) with an immutable per-Sandbox identity (matching the
// NodeJanitor residual-process contract: binary "firecracker" + "--id").
//
// Networking comes from the pre-provisioned Fastlet slot: the slot carries
// the pod-side IP and the guest tap prepared by GuestVMNetNSDriver, so a
// create performs no ip/iptables commands. Restored clones share the static
// guest address baked into the golden snapshot, DNATed per slot at the
// host. Rootfs comes from the content-addressed cache (reflink-copied per
// instance), and durable state in StateRoot/sandboxes/<id>/meta.json anchors
// idempotent create, delete, and Fastlet-restart recovery.
//
// The driver optionally delegates image pulls and device leases to the
// node-level firecracker runtime-agent over a UDS socket (agent_wiring.go);
// without a socket it runs in local mode. The HostCapabilityProber gates the
// runtime on the node assets (KVM, /dev/net/tun, firecracker binary,
// kernel, rootfs) before any driver is built.
package firecracker
