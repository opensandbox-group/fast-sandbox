# Using Sandbox Snapshots

A SandboxSnapshot takes a live checkpoint of a **running** Sandbox and
publishes it as a bootable image. After the snapshot succeeds, starting a
new Sandbox from it is an ordinary create whose `image` is the snapshot's
template name — same as any golden image.

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

## Prerequisites

- The target Sandbox is `Ready` on a Firecracker pool.
- The node runtime-agent has a **write credential** for the artifact store
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

The snapshot CR records the source Sandbox's **action bindings** verbatim
(`sandbox.fast.io/source-action-bindings` annotation) — including egress
network policy applied through the egress handler. Creating a Sandbox whose
`image` equals a snapshot's `templateName` re-applies them automatically:

- bindings you pass **explicitly** on the create win per handler (override
  one policy, keep the rest);
- recorded handlers the target Pool does not declare are dropped (they
  could not take effect there);
- in-guest network state (routes, connections, in-guest firewall) comes
  along in the memory image itself.

The bindings are also recorded **in the published manifest**
(`actionBindings`, an optional field existing consumers ignore): the
artifact set outlives the SandboxSnapshot CR — deleting the CR keeps the
artifacts — so the manifest is the durable record. A restore after the CR
is gone (or in another cluster) reads the policy from the store and
constructs the create with those bindings; the fastpath auto-apply only
consults the CR because the control plane cannot read the object store at
binding time (the image has not been placed on a node yet).

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
  removes the CR and node-local staging, but **never deletes the published
  artifacts** — store lifecycle is managed out of band.
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
  last). The manifest copies `machine`/`guestNetwork`/`kernel`/`envs`
  verbatim from the source image (they describe the vmstate lineage that
  restore validates). Only the template-name index is written — never a
  default `sha256(sourceImage)` key, which would pollute the golden image.
- **Follow-ups**: [#56](https://github.com/opensandbox-group/fast-sandbox/issues/56)
  diff snapshots (pause window proportional to dirty pages),
  [#57](https://github.com/opensandbox-group/fast-sandbox/issues/57)
  same-node prewarm of a freshly published set.
