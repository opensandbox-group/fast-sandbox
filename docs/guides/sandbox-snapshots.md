# Using Sandbox Snapshots

A SandboxSnapshot takes a live checkpoint of a **running** Sandbox and
publishes it as a bootable image. After the snapshot succeeds, starting a
new Sandbox from it is an ordinary create whose `image` is the snapshot's
template name for native + S3, or the returned immutable OCI `manifest_ref` for OverlayBD + OCI. See [OCI live snapshots](overlaybd-oci-sandboxes.md#live-snapshot-api) for the UUID-based two-artifact flow.

```text
your Sandbox (running)
        │  CreateSandboxSnapshot (one call, returns immediately)
        ▼
  SandboxSnapshot CR  ──  kubectl get -w : Pending → Creating → Publishing → Succeeded
        │
        ▼
  CreateSandbox(image=<templateName>)  ──  new Sandbox boots from the checkpoint
```

During the snapshot the source Sandbox keeps running: the only interruption
is a sub-second pause while the memory is dumped (zero observable `/ping`
failures with a spill area configured).

The source remains running for both publication paths. OCI snapshots use a random UUID `snapshot_id`; `template_name` is optional and no image-name index is published. Native snapshots retain the named-index behavior described below.

## Prerequisites

- The target Sandbox is `Ready` on a Firecracker pool.
- For native + S3, the node runtime-agent has a **write credential** for the artifact store
  (a `writeSecretRef` on the registry rule; without it snapshots fail with
  a Forbidden-family error and pulls keep working). Your platform operator
  provides this — on the integration environment `verify-snapshot` wires it
  automatically.

## Take a snapshot

**Imperative (gRPC, recommended)** — idempotent, with client-side
pre-checks:

```proto
CreateSandboxSnapshot(
  request_id    = "ckpt-2026-09-11-01",   // becomes the CR name; replay-safe
  sandbox       = { namespaced_name = { namespace: "default", name: "my-sandbox" },
                    expected_uid    = "<uid>" },          // optional, fences renames
  template_name = "my-app-v2"                             // the image name this
)                                                         // snapshot will get
```

- `request_id` is the idempotency key: resending the same request returns
  the current state; changing the intent under the same ID is a conflict.
- `template_name` is **global and permanent for the store**: after
  publishing, `my-app-v2` resolves to this artifact set. Pick it like an
  image tag (`my-app`, version or timestamp suffixes recommended).
- The call returns as soon as the intent is persisted (phase `Pending`);
  it does not wait for the dump.

**Declarative (kubectl)** — the controller drives the same flow:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxSnapshot
metadata:
  name: ckpt-2026-09-11-01        # same name == same idempotent intent
  namespace: default
spec:
  sandboxRef:
    name: my-sandbox
    namespace: default
  templateName: my-app-v2
```

## Watch it converge

```bash
kubectl get sandboxsnapshot ckpt-2026-09-11-01 -w
```

| Phase | Meaning |
| --- | --- |
| `Pending` | Intent persisted, not yet admitted by a fastlet (placement catch-up, retry window) |
| `Creating` | The pause window: VM paused, memory + disk dumped, VM resumed. Business-visible interruption lives here (sub-second with a spill area) |
| `Publishing` | Dump complete, VM already resumed; the artifact set is uploading |
| `Succeeded` | `status.manifestRef` / `artifactDigest` / `sizeBytes` describe the published set |
| `Failed` | Terminal. `status.conditions[Completed].reason` explains; retry with a **new** `request_id` |

gRPC polling: `GetSandboxSnapshot` (supports `expected_uid` fencing).

## Boot from the snapshot

Nothing snapshot-specific — the template name is just an image reference:

```bash
fastctl run my-sandbox-v2 --image my-app-v2 --pool firecracker-pool
# or: CreateSandbox(image="my-app-v2", pool_ref="firecracker-pool", ...)
```

The first create on a node pulls and verifies the artifact set into the
node cache (multi-GiB sets take seconds to minutes over the store/P2P
path); later creates on that node restore in milliseconds.

## Network policy on restore

The published manifest carries no policy record. What a Sandbox created
from a snapshot image enforces is decided by two channels, in a fixed
priority:

1. **Template state carry (capability-gated).** When the egress handler
   reports `templateStateExport`, the template bundle includes the source's
   opaque, sealed `policy.json`. The node lands it for the clone's first
   registration — strictly before the data plane becomes ready — and the
   landed record is authoritative: its revision is the source's, not the
   seed's. This is how a clone continues the source's converged policy.
2. **The create-request `actionBindings` (the seed).** A clone is a new
   policy subject; the bindings you pass are validated as on any create and
   seed its first registration. They decide the effective policy when the
   template bundle carries no state — the egress lacks the capability, or
   the landing degraded (logged, never fails the create).

`verify-policy` discriminates the channels by cloning with the *opposite*
binding (deny) while the template record is allow: a capability-carrying
egress must enforce the template's allow (source revision preserved); a
seed-only egress must enforce the deny.

Pause the source Sandbox first if you need to replay its bindings — they
are readable from the Sandbox spec until it is gone.

A resume of the *same* Sandbox (pause checkpoint) is different: its live,
relay-mutated policy rides the checkpoint bundle as the opaque
`policy.json` and is re-imported node-side before the data plane becomes
ready. That path never involves the control plane or the manifest.

## Rules of thumb

- **Same Sandbox, back-to-back snapshots**: allowed once the previous one
  reaches `Publishing` — no need to wait for the upload to finish.
  Attempts while it is still `Pending`/`Creating` are rejected
  (`FailedPrecondition`, no object created).
- **Same template name**: exclusive until the holder terminates. Reusing a
  name after `Succeeded` republishes under it (the previous set remains in
  the store but becomes unaddressable); prefer a fresh name per checkpoint.
- **After `Failed`**: issue a new snapshot with a new `request_id` (the
  object is one-shot). Nothing was made addressable by the failed attempt.
- **Deleting the snapshot** (`DeleteSandboxSnapshot` / `kubectl delete`)
  deletes the published rootfs and snapfiles manifests for OCI snapshots,
  then removes the CR and node-local staging. Failures keep the CR terminating
  for retry. Shared blobs are reclaimed by registry GC. Native + S3 retains
  its existing out-of-band artifact lifecycle.
- Template names are **global across namespaces**: two teams snapshotting to
  the same name overwrite each other's index (last writer wins).

## Troubleshooting

| Symptom | Cause / action |
| --- | --- |
| `Failed` with `Completed.reason=InsufficientStorage` | Node-local staging space cannot hold the artifact set; the snapshot was **rejected before the VM was ever paused** (a best-effort image-cache GC already ran). Free node space and re-issue with a new `request_id` — distinct from a genuine failure, nothing was interrupted or staged |
| `Failed: ... Forbidden`-family at publish | Agent has no write credential for the store; contact the platform operator |
| `Failed: SandboxNotFound` / `SandboxUIDMismatch` | Target was deleted or recreated under the same name; re-issue against the current Sandbox |
| `Failed: SnapshotLost` | The fastlet that owned the task restarted mid-flight; re-issue with a new `request_id` |
| `Failed: SnapshotDeadlineExceeded` | Wedged unadmitted or unobservable task timed out; fences released, retry |
| `FailedPrecondition ... retry once it reaches Publishing` | The Sandbox still has a snapshot in its pause window; retry after `Publishing` |
| Restore pulls are slow on the snapshotting node | Expected in v1: the snapshot deletes its local copies after publishing (same-node prewarm is issue #57) |

## Try it end to end

The integration environment exercises the entire flow, including the
fencing rules and artifact validation:

```bash
./scripts/integration-env.sh up
./scripts/integration-env.sh verify-snapshot
```

For the CRD field reference and gRPC details see
[SandboxSnapshot in the API reference](../reference/api.md); for the
internals (spill area, fence implementation, artifact layout, write
credentials) see the sections below.

---

## Appendix: how it works

- **Chain**: fastpath persists the CR and triggers the assigned fastlet
  directly; the controller converges (trigger/observe to terminal), pins
  observations to the trigger-time placement, and enforces terminal-phase
  monotonicity plus deadlines (10 min unadmitted, 45 min unobservable).
- **Pause window**: pause → rootfs reflink → vmstate/memory dump → resume.
  With `FAST_SANDBOX_SNAPSHOT_SPILL_DIR` pointing at a tmpfs/emptyDir
  (Memory)/local NVMe, the dump lands there and moves to staging after the
  resume — measured 7.2 s → 258 ms for a 512 Mi Sandbox on a network-backed
  StateRoot. Jailed VMs get the spill bind-mounted into the jail root at
  instance creation.
- **Artifacts**: byte-identical to SandboxTemplate builds
  (`<store>/<sha256(manifest)[:16]>/{rootfs.ext4,vmstate.snap,memory.snap,
  SHA256SUMS,manifest.json}` + `index/<sha256(templateName)>.json` written
  last). The manifest copies `machine`/`guestNetwork`/`kernel`/`platform.envs`
  verbatim from the source image (they describe the vmstate lineage that
  restore validates). Only the template-name index is written — never a
  default `sha256(image)` key, which would pollute the golden image.
- **Transfer mode** (`transfer.format` in the runtime-agent config, the
  Helm value `runtime.transfer.format`): one install-time switch.
  - `legacy` (default): the whole-file layout above and the original cache
    behaviour (every create/resume re-hashes the cached set).
  - `chunked`: each artifact is split into 4 MiB chunks; all-zero chunks
    are skipped and the rest are stored once under content-addressed keys
    (`<store>/chunks/sha256/<aa>/<sha256>`), so a 3.5 GiB snapshot that is
    ~93% zeros uploads ~240 MiB, and a repeat pause only its changed
    chunks. The set namespace then holds `chunks.json` (the chunk list)
    instead of the artifacts, bound into the manifest by a `chunks` field:
    index digest → manifest → `chunks.json` → per-chunk sha256 forms an
    unbroken verification chain. The mode also turns on parallel chunk
    transfers, a verified-cache marker (`user.fsb.verified` xattr) that
    replaces the per-lease re-hash, sparse fsynced cache writes, parallel
    manifest hashing in the fastlet (#61), and the same-node cache of
    template snapshots (#57).
  - Pulls read both layouts in either mode, so switching back to `legacy`
    keeps every chunked snapshot restorable. An agent image that predates
    the setting cannot read chunked sets: roll the new image to every node
    before switching any node to `chunked`.
  - Store requirements are unchanged: path-style SigV4 `GET`/`PUT`/`HEAD`
    (Ceph RGW, MinIO, AWS S3, R2, ...). Chunk `PUT`s are signed with the
    payload digest, so the store rejects a corrupted body. Chunk objects
    are shared between sets and are not garbage-collected yet.
- **Follow-ups**: [#56](https://github.com/opensandbox-group/fast-sandbox/issues/56)
  diff snapshots (pause window proportional to dirty pages),
  [#57](https://github.com/opensandbox-group/fast-sandbox/issues/57)
  same-node prewarm of a freshly published set,
  [#61](https://github.com/opensandbox-group/fast-sandbox/issues/61) overlap
  the manifest hashing with the dump window (per-file SHA256 currently
  serializes ~15.5 s before the upload starts).
