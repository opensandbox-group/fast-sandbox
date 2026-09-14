# Firecracker full-chain E2E (builder → S3 → runtime-agent → driver restore)

Reference environment and results for `scripts/firecracker-chain-e2e.sh` —
the stage-1 closeout of the
[Firecracker on-demand loading](../design/firecracker-on-demand-loading.md)
design. Every component runs for real: the builder publishes through MinIO,
the runtime-agent pulls over the wire (SigV4 + credential mapping), and the
driver restores from the pulled artifacts.

```text
alpine:3.19 ──builder──▶ s3://sandbox-images/publish (index + digest16 build + SHA256SUMS)
                              │  PinImage (UDS, SigV4 over MinIO)
                              ▼
                       runtime-agent ──pull──▶ <state-root>/images/<sha256(image)>/
                              │  restore (LoadSnapshot, network_overrides)
                              ▼
                       firecracker driver E2E ──▶ guest reachable + execd ready
```

Host requirements match [firecracker-runtime-e2e.md](firecracker-runtime-e2e.md)
(xfs reflink StateRoot, `/dev/kvm`, `/dev/net/tun`) plus `docker`, `jq`,
`go`.

## Verification points

| # | Point | Assertion |
|---|-------|-----------|
| 1 | Builder publish layout | `index/<sha256(image)>.json` + `digest16/{rootfs.ext4,vmstate.snap,memory.snap,SHA256SUMS,manifest.json}`; `index.artifactDigest` == sha256(manifest); every `files[]` digest matches the uploaded object |
| 2 | SigV4 against MinIO | Agent pull succeeds over the wire (digest-verified commit proves signature correctness) |
| 3 | Credential mapping | Read-only AK/SK + endpoint resolved from the artifact-store mount (`store`/`endpoint` files); agent connects to MinIO |
| 4 | Builder snapshot ↔ driver restore | Driver E2E restores from the pulled artifacts (`FC_SKIP_PREP`, no self-bootstrap); single + parallel + serial batches, **execd `/ping` ready on every instance** |
| 5 | Idempotency + cleanup | Re-PinImage makes zero re-pull; driver delete unpins through the UDS API; lease lifecycle returns state to zero |

## Script flow

1. Download firecracker release, prep kernel/rootfs (presence checks only in
   `FC_SKIP_PREP` mode);
2. Start MinIO (AK/SK `chain-test`/`chain-test-secret`, bucket
   `sandbox-images`);
3. Export `alpine:3.19`, build the builder image, run the pipeline with
   `publish: s3://sandbox-images/publish`, machine 1 vCPU / 512 MiB, execd
   baked (`opensandbox/execd:1.1.0`);
4. Assert the published layout (point 1);
5. Build and start `firecracker-runtime-agent`; pull twice via `PinImage`
   (points 2/3 + idempotency);
6. Run the driver E2E cases (`TestFirecrackerDriverE2E`, `NoInfra`,
   `Concurrent`, `ConcurrentSerial`) with `FC_SKIP_PREP=1` and
   `FC_AGENT_SOCKET` wired (point 4; batches print per-stage load
   breakdown);
7. Exercise the lease lifecycle (`LeaseDevices`/`ListLeases`/
   `ReleaseDevices`/`UnpinImage`) and assert state returns to zero (point 5);
8. Tear everything down (agent, MinIO container, bridge/netns/rules).

## Results (reference host, 2026-08-29)

**Builder pipeline** (~73 s per run):

| Phase | Time |
|-------|------|
| pull / convert | 6.5 s / 0.6 s |
| boot → SANDBOX_READY | 2.1 s |
| snapshot create / restore verify | 5.0 s / 5.1 s |
| manifest / publish | 12.4 s / 42.3 s |

`bootToReady` dropped from 16.1 s to 2.1 s once readiness is the execd
`/ping` probe instead of the 15 s warmup fallback.

**Scenario A — cold pull**: first `PinImage` over MinIO (3.6 GiB cache)
~39 s; committed-cache re-pin makes zero requests.

**Scenario B — driver restore** (12 creates aggregated):

| Stage | min | avg | max |
|-------|-----|-----|-----|
| total | 39.5 ms | 100.6 ms | 720.9 ms (Infra case) |
| launch (jailer spawn) | 20.4 ms | 20.6 ms | 21.2 ms |
| configure (LoadSnapshot) | 2.4 ms | 2.6 ms | 3.1 ms |
| boot (resume) | 0.24 ms | 0.28 ms | 0.31 ms |
| execd /ping delta | 4.4 ms | 4.7 ms | 5.1 ms |

Batch comparison: **parallel 5 VMs ≈ 50 ms** vs **serial ≈ 204 ms**
(~4× from launch overlap; slot acquire is ~23 µs after the guest data plane
moved outside the manager lock). End-to-end create → business-ready is
~45 ms on the driver layer (VM ~40 ms + execd ~5 ms).

## Known gaps

- **OSS vs MinIO**: SigV4 is verified against MinIO; Aliyun OSS region
  normalization and endpoint-style differences are covered in production
  validation (design §6.8)
- **Builder NIC dependency**: restore relies on the snapshot carrying the
  baked NIC; without it `network_overrides` has no iface to replace and the
  guest is unreachable
- **execd readiness is driver-E2E-verified only**: the builder's restore
  validation waits for the guest heartbeat, so execd survival after restore
  is asserted by the driver E2E (the sandbox-usable SLO), not by the builder

## Overrides

`CHAIN_SOURCE_IMAGE` (alpine:3.19), `CHAIN_IMAGE` (chain-test:v1),
`CHAIN_MACHINE_VCPU`/`CHAIN_MACHINE_MEM` (1/512Mi), `CHAIN_ROOTFS_SIZE`,
`MINIO_*`, `WORK`, `SANDBOX_TEMPLATE_BUILDER_IMAGE`, plus the driver E2E
overrides `FC_VERSION/FC_BINARY/FC_JAILER/FC_KERNEL/FC_ROOTFS/FC_STATE_ROOT`.
`--cleanup` cleans a crashed run.
