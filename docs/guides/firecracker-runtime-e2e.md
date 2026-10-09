# Firecracker runtime driver E2E

Reference environment and results for `scripts/firecracker-e2e.sh`, which
drives the Firecracker driver directly through `go test -tags firecracker`.
Use it to reproduce the suite on a fresh host or to triage failures.

## Host requirements

| Item | Value |
|------|-------|
| Machine | Bare-metal with `/dev/kvm` and `/dev/net/tun` (VT-x); root runner |
| Reference host | `agent-sandbox033067064046.sg52` — 96 logical CPUs, 504 GiB, Alibaba Cloud Linux 3 (kernel 5.10.134) |
| Tools | `ip`, `iptables`, `sysctl`, `ping`, `tcpdump`, `tar`, `curl`; docker not required |
| StateRoot | **XFS with `reflink=1` required** — provision with `scripts/firecracker-xfs-stateroot.sh --loop`; Fastlet startup rejects non-XFS and failed `cp --reflink=always` probes instead of accepting a slow rootfs copy |
| Disk headroom | Keep ≥ 20% free; high occupancy measurably inflates GuestCopy/rootfs timing (see Results) |

## Runtime assets

Restore is the only startup path: the runtime asset is the **golden
snapshot set**; the kernel and rootfs are **snapshot-prep inputs** (one
preparation VM produces the set; restored Sandboxes never boot a kernel).

| Item | Value |
|------|-------|
| Firecracker / jailer | v1.16.1 upstream release (both binaries ship in the same tarball) |
| Snapshot-prep kernel / rootfs | `vmlinux.bin` 4.14.174, `bionic.rootfs.ext4` (~300 MiB) |
| Golden snapshot set | `<StateRoot>/images/<sha256(image)>/{rootfs.img, vmstate.snap, memory.snap, manifest.json}` |
| Guest spec | 1 vCPU / 512 MiB (manifest machine tuple; restore machine config is baked in the vmstate) |

The set is self-bootstrapped once per StateRoot (prep VM → Pause →
`PUT /snapshot/create`) and cached; a `.prep-version` marker rejects stale
recipes. Preparation uses an isolated network namespace with an UP TAP
and gateway `172.30.0.1`, and waits for a guest echo reply before pausing.
A permanent host-side guest neighbor prevents host ARP requests from seeding
the guest cache; the guest must resolve the gateway itself to return the reply.
Artifacts download on first run.

## Restore startup (v1.16 semantics)

`PUT /snapshot/load` must be the **first** configuration call (any
machine/drive/network call before it is rejected). The driver:

1. prepares the jail root `<StateRoot>/jails/firecracker/<id[:32]>/root/`
   (instance rootfs reflink copy + hard-linked `vmstate.snap`/`memory.snap`
   under `snapshots/`) and launches
   `jailer --id <id> --netns <slot netns> --uid 0 --gid 0 --exec-file <firecracker>
   --chroot-base-dir <StateRoot>/jails -- --api-sock /api.sock`; the VMM
   enters the per-clone slot netns and its chroot (jailer execs firecracker,
   PID unchanged);
2. `PUT /snapshot/load` with chroot-relative `/snapshots/*` paths, file-backed
   memory, and the in-netns tap `vmtap0` via `network_overrides`;
3. `PATCH /vm {"state":"Resumed"}` (InstanceStart is rejected after load),
   then polls until `Running`.

## Network topology (per-clone netns)

Each slot owns a netns (`fsb<64 hex>`); the VMM and its tap `vmtap0`
(fixed name) live inside it — no host-side tap, no shared-bridge ARP domain.
The baked guest MAC/IP (172.30.0.3 / 02:00:00:00:00:01) is shared by all
clones and made safe by namespace isolation (upstream clone model).

- Bridge `fsb0` (host): `172.30.0.1/24`; slot `eth0` = `172.30.0.2+`,
  **skipping the baked guest address** (a slot owning it would shadow the
  guest)
- Static in-netns rules (slot preparation): `FORWARD` gateway ACCEPT,
  sibling REJECT (`-i vmtap0 -o eth0 -d <CIDR>`), egress/ingress ACCEPT,
  conntrack; proxy ARP on the **tap only**; `ip_forward=1`
- Per-restore data plane (from the manifest `guestNetwork`): ingress
  `DNAT slot IP → baked guest IP`, egress `SNAT guest IP → slot IP`
  (OSEP-0022 source-IP dispatch), delivery via a `/32` route — the guest
  address is deliberately **not** assigned to the tap (a local address
  would shadow the guest: netns answers ICMP locally, refuses TCP)
- Ingress contract: dial the **slot IP** (AccessDescriptor), unique per
  instance, DNATed to the shared guest address
- Cleanup: kill (jailer PID) → remove jail dir → slot Destroy deletes the
  netns (rules and tap vanish with it)

## Test cases

The script runs the following cases. The cached-gateway regression prepares
its own fresh snapshot; other cases reuse the shared golden snapshot set:

| Test | Covers |
|------|--------|
| `TestFirecrackerDriverE2E` | Full lifecycle with Infra Components GuestCopy delivery (debugfs-verified), reachability via slot DNAT |
| `TestFirecrackerDriverE2ENoInfra` | Pure restore baseline without GuestCopy |
| `TestFirecrackerDriverE2EConcurrent` | 5 VMs restored **in parallel**; per-instance reachability + **execd `/ping` ready on every slot** (the sandbox-usable SLO) |
| `TestFirecrackerDriverE2EConcurrentSerial` | Same batch **sequentially** (production default path); both batches print per-stage min/avg/max |
| `TestFirecrackerDriverE2ECachedGatewayMAC` | Fresh snapshot with a resolved gateway neighbor; 5 concurrent clones must return their first ICMP reply within 1 second with the cached gateway MAC. A changed-TAP-MAC negative control must lose that reply. No guest ARP flush, probe retry, or recreate; always performs local prep, including with `FC_SKIP_PREP=1` |
| `TestFirecrackerDriverE2EImageGC` | LFU cache GC: unreferenced images evicted, live Sandbox pins its image |

## Results (reference host, 2026-08-29, xfs StateRoot)

| Case | Wall | Notes |
|------|------|-------|
| Single NoInfra | ~40 ms | launch ~20.4 ms (jailer spawn, largest fixed cost) + configure ~2.6 ms + boot ~0.3 ms + ~17 ms unstaged overhead (ApplyGuest, state persists, idempotency) |
| Single Infra | 75–720 ms | GuestCopy 36–505 ms, highly variable; a high-occupancy disk inflates it |
| 5 VMs parallel | ~50 ms | acquire ~23 µs (guest data plane runs outside the manager lock); launch overlaps |
| 5 VMs serial | ~204 ms | 5 × ~40 ms, linear |
| execd ready | **+4.4–5.1 ms** after VM Running | stable across all cases |

Restore adds no measurable per-create cost beyond the VMM spawn; the
shared `memory.snap` is COW-read by concurrent clones.

## Environment issues and mitigations

| Issue | Symptom | Mitigation |
|-------|---------|------------|
| Non-reflink StateRoot | Startup rejected | XFS with `reflink=1` and GNU coreutils required |
| Stale netns from a failed teardown (`ip netns del` EBUSY racing a dying VMM) | Stale netns still owns the slot IP on the bridge: answers ARP/pings locally, refuses TCP — the live netns's iptables counters stay 0 | Teardown deletes the host veth **before** the netns; delete retry 5×500 ms; every E2E environment purges stale `fsb*` netns and bridge devices first |
| `net.ipv4.conf.all.proxy_arp` | Every netns proxy-answers the whole CIDR; host neighbour cache points slot IPs at random netns | Proxy ARP on the tap interface only |
| Guest address assigned to the tap | Netns shadows the guest (fake ICMP, TCP refused) | `/32` route + IPAM reserves the baked guest address |
| Builder restore validation waits only for the guest heartbeat | execd survival after restore was latent | Driver E2E probes execd `/ping` as the readiness SLO |

## Running

```bash
FC_STATE_ROOT=/var/lib/fast-sandbox/e2e ./scripts/firecracker-e2e.sh
```

Overrides: `FC_VERSION`, `FC_BINARY`, `FC_JAILER`, `FC_KERNEL`, `FC_ROOTFS`,
`FC_STATE_ROOT` (persist it to reuse the prepared golden set), `WORK`;
`--cleanup` for interrupted runs. Host resources created by a run (bridge,
MASQUERADE, `ip_forward`, netns, jail dirs) are restored on exit.
