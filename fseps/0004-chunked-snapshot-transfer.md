---
title: "Chunked Snapshot Transfer"
authors:
  - "@kaustubh-765"
creation-date: 2026-11-17
last-updated: 2026-11-24
status: draft
---

# FSEP-0004: Chunked Snapshot Transfer

## Problem and goals

Pause, snapshot and restore of a Firecracker Sandbox publish or pull a whole
native artifact set: rootfs, guest memory, VM state and small state bundles.
The runtime-agent uploads every file in full, and it re-hashes the full
cached set on every lease. The Fastlet hashes each file again to build
`manifest.json`.

For a 3.5 GiB set (3 GiB rootfs + 512 MiB RAM) on a 3-node cluster with a
Ceph RGW store over a ~2 Gbit/s link (details in [#146](https://github.com/opensandbox-group/fast-sandbox/issues/146)):

| Operation | Time | VM actually paused |
|---|---|---|
| Pause | 25.2 s | ~0.1 s |
| Snapshot | 24.2 s | ~0.1 s |
| Restore, other node | 25.1 s | – |

Almost all of this time goes to moving bytes that are zero or already in the
store:
- ~93% of the rootfs and ~81% of guest memory are all-zero 4 MiB blocks.
- Consecutive snapshots of one Sandbox share most of their non-zero blocks.

Goals:

- Upload and download only the non-zero data the store does not already
  have.
- Keep end-to-end integrity: every restored byte is verified against a
  digest bound to the published artifact digest.
- Opt-in at install time; today's layout stays the default.
- Defined behaviour for mixed versions during a rolling upgrade, and a
  stated rollback path with its limits.

### Non-goals

- No public API, CRD, SDK, CLI or FastPath changes. The internal agent
  protocol gains optional response fields (see Compatibility).
- No new store requirements beyond today's path-style SigV4 `GET`, `PUT` and
  `HEAD`.
- The OverlayBD/OCI path (FSEP-0001) is unchanged; only native Firecracker
  sets are chunked. That includes native RootfsOnly sets (#181), whose chunk
  index holds only `rootfs.ext4`.
- The template builder keeps publishing whole-file sets (possible
  follow-up).
- Local optimizations that change no protocol are not part of this
  proposal:
  - template snapshots kept in the node cache (#57) and a verified-digest
    memo keyed by file identity: done in #244, which also removed the
    post-pull re-hash;
  - fsynced cache writes and single-flighted cache ensures: a separate
    small PR before PR 1;
  - parallel and overlapped hashing (#61): a separate PR.

  #213's xattr verified-cache marker is dropped in favor of #244's memo:
  the chunked pull records each file it committed and verified into that
  memo, as a whole-file pull does. Nothing local depends on
  `transfer.format`, which selects only the store format. Sparse cache
  files stay a property of the chunked pull, which skips zero chunks.

  The standalone fixes from #213 are already merged in #214.

## Proposal and tradeoffs

### Storage layout

```text
<store root>/
  <build ns>/                      # first 16 hex of sha256(manifest.json), unchanged
    manifest.json                  # gains an optional "chunks" ref
    chunks.json                    # chunk index of this set (new)
    SHA256SUMS, vault/policy ...   # small files stay whole-file objects
                                   # (SHA256SUMS still lists every file, also
                                   # those stored only as chunks; informational)
  chunks/sha256/<aa>/<sha256>      # raw chunk, shared by all sets (new)
  chunks/sha256/<aa>/<sha256>.zst  # zstd-encoded chunk (compression: zstd)
```

- **Chunk keys:** a chunk object's key is the SHA-256 of its *decoded*
  bytes, so identical content is stored once across all sets. The `.zst`
  suffix keeps raw and compressed copies of the same content apart.
- **Chunk size:** fixed at 4 MiB (the RGW stripe size). It is recorded per
  index, so it can change later without a format change.

### Chunk index (`chunks.json`)

```json
{
  "version": 1,
  "chunkSize": 4194304,
  "files": {
    "memory.snap": {
      "sizeBytes": 536870912,
      "chunks": [ { "off": 0, "len": 4194304, "sha256": "…", "enc": "zstd", "storedSize": 81233 } ]
    }
  }
}
```

- An offset missing from `chunks` is an all-zero range, restored as a hole.
- Serialization is deterministic, so a retried publish produces the same
  manifest digest.
- The parser rejects the index before any write if a chunk is misaligned,
  out of bounds, has a malformed digest, an unknown encoding, or an invalid
  stored size.

### Manifest binding

`manifest.json` gains two optional fields:

- **Top-level `chunks`:** `{version, ref, sha256, sizeBytes}` binds the index
  by digest. `ref` must be `chunks.json` in the set's own namespace.
- **Per native file entry, `chunkList`:** `{version, chunkSize, sha256}`.
  This is a domain-separated SHA-256 over the chunk size, the file size and,
  in offset order, each non-zero chunk's `(offset, length, sha256)`.
  - It can be computed from a verified index without reading any data.
  - It can be computed from a local file by hashing only its non-zero
    chunks, in parallel.
  - It lets the Fastlet skip the whole-file hash, which takes ~10 s for a
    4 GiB dump on CPUs without SHA extensions. Whether a manifest may omit
    the whole-file `sha256` is a rollout decision; see "Rollout and
    rollback".

Trust chain: `index artifactDigest → manifest.json → chunks.json → per-chunk
sha256`. A set without `chunks` is a whole-file set and is read exactly as
today.

### Data path

**Publish (runtime-agent):**
- A staged entry and a published entry are checked separately:
  - **Acceptance, before any upload:** the staged manifest parses; every
    native file the set needs exists, and its `sizeBytes` equals the file
    size; a digest that is present (`sha256`, `chunkList`) is well-formed. A
    native entry with only `sizeBytes` is accepted in every mode, since the
    Fastlet stages that way when it skips whole-file hashes. An inherited
    `chunks` binding is dropped, not rejected (see Compatibility).
  - **Completion:** in `chunked`, the agent derives each native file's
    `chunkList` from the chunks it wrote. Unless `format: chunked` and
    `chunkListOnly: true` are both set, it computes the whole-file `sha256`
    of every entry staged without one and rewrites `SHA256SUMS` from the
    final manifest.
  - **Pre-manifest check, right before `manifest.json` is written:** every
    native entry has `sizeBytes` > 0 and the digests its mode requires
    (`legacy`: `sha256`; `chunked`: `chunkList` bound to the uploaded
    `chunks.json`, plus `sha256` unless `chunkListOnly: true`). State files
    (vault, policy) always carry `sha256`, and `SHA256SUMS` is never empty,
    which readers reject. A failed check fails the publish before any
    manifest or index object exists; its chunks stay unreferenced.
  - A Fastlet acting on a stale capability, or an agent restarted into
    another mode between staging and publish, therefore costs a hash, not
    a rejected or unrestorable set.
- Chunk the native files the staged manifest lists (rootfs, memory and VM
  state for a full set; only the rootfs for a RootfsOnly set), not a fixed
  list.
- Split files into chunks, skipping holes (`SEEK_DATA`/`SEEK_HOLE`) and
  all-zero chunks.
- Skip chunks the store already has, using an in-memory set of known keys
  plus `HEAD`. A 403 answer (AWS without `s3:ListBucket`) falls back to
  uploading.
- Collapse concurrent uploads of one key into one (singleflight).
- Upload order: chunks → `chunks.json` → small bundles and `SHA256SUMS` →
  `manifest.json` → index object (none for checkpoints). A reader never sees
  a manifest whose chunks are missing.
- Raw chunk `PUT`s are SigV4 payload-signed with the chunk's own digest,
  which the store verifies.

**Pull (runtime-agent):**
- Verify `chunks.json` against the manifest.
- Fetch chunks in parallel (`transfer.concurrency`), verify each chunk's
  SHA-256 before writing it, and leave zero ranges as holes.
- Commit a file only after every one of its chunks was fetched and
  verified; a cancelled or failed pull commits nothing. Each committed file
  is recorded in #244's verified-digest memo.
- Chunk requests use a 10 s per-attempt timeout with retries.
- In chunked mode, store traffic uses a pooled transport: 64 idle
  connections per host, 30 s idle timeout.

**Fastlet:**
- Learns the agent's mode from optional `transferFormat` and
  `chunkListDigest` fields in `HealthResponse` and `PublishImageResponse`.
  The agent reports `chunkListDigest` only when `format: chunked` and
  `chunkListOnly: true`.
- Older agents omit the fields, which means legacy with whole-file hashes.
- Stages native files with `sizeBytes` only when the agent reported
  `chunkListDigest`; otherwise it hashes them as today.
- Calls `Health` right before it decides whether to skip whole-file hashes.
  A `Health` error clears the cached capability, so the Fastlet falls back
  to hashing.
- Drops an inherited `chunks` binding in both manifest assemblers: the full
  snapshot manifest and the RootfsOnly manifest (#181).

### Shared-chunk retention and garbage collection

**Today:**
- fast-sandbox never deletes store objects. Build namespaces accumulate and
  are cleaned outside the system.
- Chunking changes one fact: a chunk can belong to many sets. Deleting a
  namespace no longer frees its data, and a lifecycle rule on `chunks/`
  could delete data that live sets still need.

**Proposal:**
1. **First release: no store-side deletion**, the same as today. Turning
   off the built-in GC does not protect `chunks/` from other deleters, so
   enabling `chunked` has a rollout gate: no store expiration rule
   (including bucket-wide rules) and no external cleanup job may delete
   objects under `chunks/` (see "Rollout and rollback").
2. **Then a separate, explicit mark-and-sweep tool,** `fsb-store gc`. It
   runs as a CronJob with its own store credentials (list, get, delete
   object versions) and read-only Kubernetes RBAC; the agent's credentials
   stay as they are. It ships **dry-run only** and reports what it would
   delete.
   - **Roots,** read from the Kubernetes API, plus the store's own recent
     writes:
     - `SandboxTemplate` `status.manifestRef` and `spec.indexKey`;
     - each Sandbox's `status.runtime.checkpoint.manifestRef` and
       `spec.image`. A paused Sandbox's checkpoint is private: no index
       entry names it, so this field is its only root;
     - `SandboxSnapshot` `status.manifestRef` and `spec.templateName`;
     - `SandboxPool` `spec.warmImages`;
     - every `index/*.json` entry (see "Index report");
     - every set namespace younger than `G`, which covers a set published
       before its owner's status records it (for example a pause whose
       checkpoint is not persisted yet, or a retried pause).
   - **Fail closed:** any list, read or parse error, or an unreadable root
     manifest or `chunks.json`, aborts the run without deleting anything.
   - **Mark:** root → `manifest.json` → `chunks.json` → chunk keys. A root
     whose chunk has no current version is reported.
   - **Repair (optional, `--repair`):** for such a chunk, if a noncurrent
     version still exists (removed by a plain `DELETE` or a lifecycle rule,
     not yet expired), the run restores it: it removes the delete marker, or
     copies the latest noncurrent version back as current. A repaired chunk
     is verified against its digest from `chunks.json`. Without a surviving
     version the root stays reported, and the set can only be re-created
     from a node cache that still holds it.
   - **Sweep (chunks):** a chunk is a candidate only when it was unmarked in
     **two** marks and its `LastModified` was already older than `G` when
     mark 1 started. Right before deleting, the sweep re-reads each
     candidate's versions and deletes exactly the versions it judged, by
     version ID.
   - **Sets phase:** a set namespace (whole-file or chunked) that no root
     references and that is older than `G` is deleted the same way, version
     by version. This replaces today's out-of-band namespace cleanup for
     both formats; its chunks then become unmarked for a later sweep.
   - **Index report (`--report-index`):** lists index entries older than
     `G` that no object references (a `SandboxTemplate` `spec.image` or
     `spec.indexKey`, a `SandboxSnapshot` `spec.templateName`, a Sandbox
     `spec.image`, a `SandboxPool` `warmImages` entry). GC never deletes an
     index entry. An entry is found by name only
     (`index/<sha256(image)>.json`), and creating a Sandbox that names image
     K writes nothing to the store, so a reference created between the
     root scan and a delete would be invisible to GC, even when every
     client goes through Kubernetes. Retiring an image is a coordinated
     action by the operator or an API layer in front of the cluster:
     1. mark the image retired and refuse new creates that name it;
     2. confirm that no Kubernetes object references it;
     3. delete the index entry.

     The set then stops being a root, and the sets phase collects it once
     it is older than `G`.
3. **Publisher refresh (part of PR 4, released before any deletion is
   enabled):**
   - A publisher reuses an existing chunk only if it is younger than a
     refresh age `R`, measured in server time: `Date` minus `Last-Modified`
     from the same `HEAD` response, so client clock skew does not matter.
   - An older chunk is refreshed by a server-side copy onto itself
     (`x-amz-copy-source` = its own key, `MetadataDirective: REPLACE`). The
     bytes, and so every existing set's `storedSize`, stay identical; a
     re-`PUT` of re-encoded zstd data could change the length. Where the
     copy is refused, a raw chunk is re-`PUT` with the same bytes, and a
     zstd chunk whose re-encoding has a different length is published under
     its raw key instead of overwriting.
   - The in-memory known-key set follows the same rule: an entry is trusted
     only until `R` has passed since the chunk's server-side write.
   - A publish must write `chunks.json` within `P_max` of its first reuse
     decision, or fail.
   - An agent whose code implements the refresh reports the `chunkRefresh`
     capability in `HealthResponse` and `PublishImageResponse`, in either
     `transfer.format`: a `legacy` agent reuses no chunks, and an agent
     switched back to `chunked` refreshes from its first publish.
   - Defaults: `R` = 1 day, `P_max` = 24 h, `G` = 7 days.

**Why the refresh is needed** (review on #216): marks alone do not protect
a publish that starts after mark 1 and reuses an old, unmarked chunk C by
`HEAD` or from its known-key set. Its manifest can land after the sweep, and
C, unmarked twice and old, would be deleted. The grace period does not help
either, since reuse does not change C's `LastModified`.

**Invariant:** every chunk a publish binds was written or refreshed within
`R` (server time) before the publish used it, and the publish writes
`chunks.json` within `P_max`. So the chunk is younger than `R` + `P_max` < `G`
when its set becomes a root (a young set is a root by itself). No mark that
could make the chunk a sweep candidate can start before that set is
visible.

**Stale sweep decisions (closed by versioning):** the sweep decides from a
`HEAD`, then deletes. A publisher can refresh the chunk in between: GC does
its final `HEAD` (version v1); the publisher refreshes the chunk (v2),
publishes its manifest and completes a post-publish `HEAD`; then GC
deletes. With an unconditional `DELETE` the set would lose the chunk. With
bucket versioning, the GC deletes v1 by its version ID, and the refreshed
v2 stays current. An `ETag` condition cannot do this: a refresh by copy, or
a re-`PUT` of identical bytes, keeps the same `ETag`. Older versions are
removed by a `NoncurrentVersionExpiration` lifecycle rule, which never
touches a current version and so is compatible with rollout gate 0.

**Gate for destructive mode** (all required; otherwise the run stays
dry-run):
- bucket versioning is `Enabled`;
- every registered Fastlet has advertised the `chunkRefresh` capability
  (its agent implements the publisher refresh) continuously for longer than
  `R` + `P_max`. Agents write nothing to Kubernetes:
  - each Fastlet takes the capability from its node's agent and sends it in
    its heartbeat `Capabilities`, next to the existing snapshot
    capabilities. It reads the agent's `Health` on every heartbeat and
    before every publish, sends a heartbeat at once when the capability
    changes, and reports it missing when `Health` fails;
  - the leader-elected controller aggregates its Fastlet registry and keeps
    **one** ConfigMap for the cluster with `chunkRefreshSince` and
    `observedAt`. It resets `chunkRefreshSince` to now as soon as any
    registered Fastlet lacks the capability, and renews `observedAt` every
    minute;
  - after a controller restart its registry starts empty, so it may reset
    `chunkRefreshSince` but not keep or advance it until every Fastlet has
    re-registered;
  - `fsb-store gc` deletes chunks only while `observedAt` is younger than
    `T_stale` (default 5 minutes) and `chunkRefreshSince` is older than
    `R` + `P_max`. It checks both at the start of the sweep and again
    before every delete batch (at most 1,000 objects or 1 minute). A failed
    check ends the run, and the remaining candidates wait for the next
    run. A stopped controller or a lost leader therefore stops deletion
    within `T_stale` instead of leaving an old observation in force;
- the store prefix is owned by this cluster (see "Store prefix
  ownership");
- the store passes a conformance test: responses carry `Date`, a version-ID
  delete removes only that version (including the `null` version of
  objects written before versioning), a self-copy with `REPLACE` creates a
  new current version with the same bytes and `ETag`,
  `ListObjectVersions` pages and reports each version's ID and
  `LastModified`, and whether a `PUT` with `If-None-Match: *` refuses to
  overwrite (used by `claim`).

**Writer downgrades and running sweeps:** a destructive run holds the
Kubernetes Lease `fsb-store-gc` and renews it while it runs. Before an
agent is rolled back to a release without the publisher refresh (below
PR 4 step 1), the operator suspends the CronJob and waits until the Lease
is released or has expired; only then are agents rolled back. The
rolled-back agents' Fastlets reset `chunkRefreshSince`, so later runs stay
in dry-run for at least `R` + `P_max`. Without this step the per-batch
check bounds the exposure to about one heartbeat plus one batch, but does
not remove it, so it is a downgrade limit (see "Rollout and rollback"). A
snapshot admitted before the rollback ends within the Fastlet's 30-minute
snapshot timeout, well inside `P_max`.

**Store prefix ownership:** GC sees only its own cluster's roots, so a
destructive run needs one cluster that owns the store prefix and is its
only publisher. `fsb-store gc claim` writes `gc/owner.json` with the
cluster ID (the UID of the `kube-system` namespace), only if the object is
absent: a create-only `PUT` (`If-None-Match: *`) where the store supports
it, which the conformance test checks, otherwise a write followed by a
read-back and compare. A missing marker, or one that names another
cluster, keeps the run in dry-run. Clusters that share a bucket use
separate prefixes; roots from several clusters on one prefix are not
supported.

**Store notes** (checked on Ceph RGW 20.2.4 with a throwaway bucket: the
race above survives a version-ID delete, and the unversioned negative
control loses the chunk):
- `HEAD` omits `x-amz-version-id` for an object written before versioning
  was enabled, and the copy response omits the new version ID. The sweep
  therefore takes version IDs from `ListObjectVersions`, where such an
  object is listed as `null`.
- A self-copy shares an object's data beyond the first 4 MiB but copies the
  head, so a refreshed chunk (at most 4 MiB) is stored twice until its
  noncurrent version expires. Bucket usage counts every version at full
  size, which matters for bucket quotas.
- Lifecycle rules run once a day in RGW's configured window, so a
  noncurrent version is removed one to two days after it stops being
  current.

A store without versioning keeps working for publish and restore; only
destructive GC is unavailable there.

### Alternatives considered

| Alternative | Why not |
|---|---|
| Whole-file compression only (zstd per file) | Still moves and hashes every byte on each pause. No dedup between consecutive snapshots. |
| Content-defined chunking (rolling hash) | Better dedup for shifted data, but guest memory and block images change in place at fixed offsets. Fixed 4 MiB alignment matches holes and the store's stripe size, and keeps the index simple to validate. |
| Store-side multipart copy of unchanged ranges | Depends on store features (`UploadPartCopy`) not available everywhere, and cannot drop zeros. |
| OverlayBD layers for memory | OverlayBD covers block devices (rootfs). Guest memory and VM state stay native files. |

### Configuration

Agent `agent.yaml`:

```yaml
transfer:
  format: legacy          # legacy (default) | chunked
  compression: none       # none | zstd (chunked only)
  concurrency: 8
  chunkSizeMiB: 4
  chunkListOnly: false    # chunked only; gates the chunkListDigest capability
```

- An unknown value, or `zstd` or `chunkListOnly: true` together with
  `legacy`, fails agent startup.
- `chunkListOnly: true` lets manifests omit the whole-file `sha256` of
  native files (see "Rollout and rollback", step 4). With the default
  `false`, chunked sets keep `sha256` next to `chunkList`, and older chunk
  readers can still restore them.
- In the OpenSandbox chart (OpenSandbox#2422) this is
  `runtime.transfer.*`. The default renders exactly as today, and a hash of
  the effective settings is a pod annotation, so any change rolls the
  DaemonSet.

### Security

- Every restored byte is verified: each chunk against its SHA-256 from a
  digest-bound index, before it is written.
- The index ref is restricted to the set's own namespace and fully
  validated before use.
- Chunk `PUT`s are payload-signed.
- The agent gets no new credentials, endpoints or permissions; the
  publisher refresh is a copy within the bucket it already writes to.
- `fsb-store gc` uses separate store credentials (list, get, delete object
  versions, and put on `gc/owner.json` only) and a read-only ClusterRole on
  the four CRDs, plus read access to the gate ConfigMap and the
  `kube-system` namespace (cluster ID) and get, create and update on its
  own Lease. A missing permission aborts the run, which then deletes
  nothing.

### Implementation plan

| PR | Content | Depends on |
|---|---|---|
| #214 | Standalone snapshot fixes split out of #213 | merged |
| #244 | Verified-digest memo, same-node cache commit (#57) | merged |
| cache | Fsynced cache writes, single-flighted cache ensures, independent of `transfer.format` | – |
| #61 | Parallel and overlapped snapshot hashing, independent of `transfer.format` | – |
| 1 | Chunk format, `chunks.json`, chunked publish and pull (recording into the #244 memo), always-on chunk reader, `transfer.format` and `transfer.compression`, staged-manifest acceptance and the pre-manifest digest check, inherited `chunks` dropped by the publisher and both Fastlet assemblers, a chunk transfer cancelled between jobs fails instead of committing, a coalesced chunk upload counted once, docs | this FSEP accepted, cache |
| 2 | `chunkList` digest, digest-signed `PUT`s, `chunkListOnly` and the capability rule, completion of size-only entries (agent `sha256` fill-in and `SHA256SUMS` rewrite), Fastlet `Health` refresh | 1, same release |
| 3 | Pooled store transport; P2P gateway pooling per open question 3 | 1 |
| 4 | Step 1: publisher refresh (server-time `R`, self-copy, known-key expiry, `P_max`), `chunkRefresh` capability with an immediate Fastlet heartbeat on change, controller gate ConfigMap with `observedAt`. Step 2: `fsb-store gc` (dry-run by default, Kubernetes roots, two marks, version-ID sweep with a gate check before every batch, Lease, sets phase, index report, opt-in repair, `claim` and the owner marker, store conformance test) | 1; step 1 released for longer than `R` + `P_max` before destructive mode |
| – | OpenSandbox chart value and pin bump (OpenSandbox#2422) | 1 merged |

[#213](https://github.com/opensandbox-group/fast-sandbox/pull/213) is the complete reference implementation and spans PRs 1–3,
including the fixes from its review. Each PR is built fresh on `main`
(after #181 and #244), so PR 1 is not an existing subset of #213. #213 stays
a draft and is closed once PR 1 is open.

### Open questions

1. **GC roots and safety (resolved):** `fsb-store gc` reads the roots from
   the Kubernetes API and treats young sets and every index entry as roots;
   a version-ID delete on a versioned bucket closes the sweep window; the
   gate expires with the controller's observation and is re-checked before
   every batch; the Lease orders writer downgrades after running sweeps; a
   claimed prefix keeps other clusters' roots out of scope (see
   "Shared-chunk retention and garbage collection"). Still open: run `fsb-store gc` as a CronJob, as
   proposed, or inside the leader-elected controller?
2. **Rollback (resolved):** the `chunkListOnly` finalize step is kept. It
   is safe to defer because `chunkListDigest` is reported only with
   `chunkListOnly: true`, the agent fills a missing `sha256` otherwise, and
   the Fastlet refreshes the capability before it skips hashes (see "Data
   path").
3. **P2P peer gateway:** chunk `GET`s go through the gateway when it is
   configured. Its client uses the default HTTP transport (2 idle
   connections per host), so parallel pulls churn connections. Should it
   get the pooled transport, or should chunk `GET`s bypass the gateway,
   since dedup already removes most of the traffic?

## Compatibility

- **APIs, CRDs, SDKs, CLI:** no change.
- **Configuration:** one new optional `transfer` section, defaulting to
  today's behaviour. No existing field changes meaning.
- **Store format:** additive.
  - Whole-file sets keep their exact layout and bytes. In `legacy` the
    published manifest is byte-identical to today's.
  - New readers read both layouts in both modes.
  - **Inherited `chunks` binding:** the Fastlet builds a snapshot manifest
    from the manifest the Sandbox booted from and keeps its other fields;
    both the full and the RootfsOnly (#181) assembler do this. A Sandbox
    restored from a chunked set and paused again with the agent in `legacy`
    would publish a `chunks` ref to a `chunks.json` that was never uploaded.
    The `legacy` publisher drops the binding (#213 does this:
    `bindChunkIndex` with a nil ref), and from PR 1 both Fastlet assemblers
    drop it too.
    An agent older than PR 1 can still restore a chunked set from its
    node cache and republish such a manifest with an old Fastlet. This is one
    more reason not to downgrade below PR 1 (see "Downgrade limits").
- **Existing templates, checkpoints and snapshots:** restore unchanged.

**Version combinations** ("new" = a release containing PR 1/2):

| Agent | Fastlet | Result |
|---|---|---|
| old | old | Today. |
| new, `legacy` | old | Same as today. The old Fastlet ignores the new response fields. |
| new, `chunked` | old | Chunked sets are published. The old Fastlet still writes whole-file `sha256`, which is allowed. |
| old | new | The Fastlet sees no capability and behaves as today. |
| new, `chunked` | new | Chunked sets. With the default `chunkListOnly: false`, whole-file `sha256` is kept, and the agent fills it in if a stale Fastlet staged without it. |
| new, `chunked`, `chunkListOnly: true` | new | Whole-file `sha256` of native files is omitted. Only agents and Fastlets with PR 2 can read these manifests. |
| new, `chunked`, `chunkListOnly: true` | old | **Not allowed.** The old Fastlet's readiness check (`ImageReady`) requires a whole-file `sha256` and reports such a set, including a RootfsOnly set, as `ErrImageNotReady`. |

Once every agent and Fastlet runs PR 2 or later, configuration switches
(`legacy` ↔ `chunked` ↔ `chunkListOnly`) are safe in any direction: the
reader is chosen by each set's manifest, not by the agent's settings, so
every set published earlier stays restorable.

**Restore by an old agent:**
- An agent older than PR 1 cannot read a chunked set: it looks for
  whole-file objects that were never uploaded.
- An agent with PR 1 but not PR 2 would reject a manifest entry that has
  `chunkList` but no `sha256`. **PR 1 and PR 2 ship in the same release**, so
  this agent never exists in a fleet. The E2E matrix covers only agents
  from before PR 1 and from after PR 2. If the two PRs end up in different
  releases, the matrix adds a PR 1-only build.
- The first case is handled by the rollout order below.

## Test plan

**Unit** (runtime-agent, artifacts, Fastlet):
- `legacy` publish key sequence and a byte-identical manifest;
- chunked round trip with and without zstd;
- sparse holes;
- dedup by `HEAD` and by the in-memory set;
- corrupt-chunk and corrupt-index rejection;
- stalled `PUT`/`GET` retries, `HEAD` 403 fallback;
- config validation;
- chunk-list digest from an index equals the digest from a file, and a
  mismatch is rejected;
- `chunkListOnly` off keeps `sha256`;
- `chunkListDigest` is reported only for `chunked` with `chunkListOnly`;
- the agent fills a missing `sha256` and rewrites `SHA256SUMS`, and never
  writes an empty `SHA256SUMS`;
- a malformed digest or a size mismatch fails acceptance before any object
  is uploaded;
- a size-only staged entry is completed in `legacy`, `chunked` and
  `chunkListOnly`;
- a missing required digest fails the pre-manifest check, and no
  `manifest.json` or index object is written;
- a `Health` error clears the Fastlet's cached capability;
- a chunk transfer cancelled between jobs fails and commits nothing;
- a coalesced chunk upload is counted once in the publish stats.

**Integration:**
- Publish and pull a 1.5 GiB set in both modes against a second S3
  implementation (SeaweedFS).
- Restore chunked sets with the agent in `legacy`.

**E2E** (multi-node cluster):
- Create, pause, resume, snapshot, and restore on the same node and on
  another node, in both modes. Verify data inside the Sandbox with
  checksums of files written before the pause.
- **Failure recovery:**
  - kill the agent mid-publish, then confirm the retry publishes the same
    digest and no manifest exists without its chunks;
  - a corrupted chunk object fails the restore with a digest error;
  - store unavailable during a pull, then a retry.
- **Compatibility:**
  - every row of the version table above;
  - switch `chunked`+`zstd` → `legacy`+`none` in one change, confirm the
    agent starts, and restore the earlier chunked checkpoints and
    snapshots;
  - restore a chunked snapshot, switch to `legacy`, pause again, and verify
    that the new manifest has no `chunks` ref and restores on an agent from
    before PR 1;
  - switch the agent's mode between staging and publish (Fastlet not
    restarted), and verify the published set restores cold on another
    node;
  - publish and restore a native RootfsOnly set in `chunked`;
  - stage size-only with `chunkListOnly: true`, switch the agent to
    `legacy` before publish, and verify the set restores cold on another
    node;
  - a `chunkListOnly` RootfsOnly set fails readiness on a Fastlet from
    before PR 2 (the downgrade limit); after `chunkListOnly: false` and a
    new snapshot, it boots there.
- **GC acceptance (PR 4, before destructive mode is enabled):**
  - dry-run lists candidates and deletes nothing;
  - a publish that reuses an old chunk after mark 1 refreshes and keeps it;
  - the interleaving from "Stale sweep decisions": final `HEAD` (v1), the
    publisher's refresh (v2), manifest and post-publish `HEAD`, then the GC
    deletes v1. The published set must stay restorable;
  - negative control: the same sequence with an unversioned `DELETE` loses
    the chunk, which shows the test can detect the race;
  - known-key expiry: with a fake server clock past `R`, a remembered chunk
    is checked again and refreshed;
  - fail closed: one unreadable root means no deletes;
  - paused checkpoint: a Sandbox paused for longer than `G` (shortened for
    the test), with destructive runs in between, resumes cold on another
    node;
  - index report: an unreferenced index entry older than `G` is reported
    and never deleted, and its set stays; after a coordinated retirement
    deletes the entry, the set is collected once older than `G`;
  - sets phase: unrooted namespaces older than `G` are deleted, young or
    rooted ones are kept, for whole-file and chunked sets;
  - the gate refuses destructive mode without versioning or `chunkRefresh`,
    and a restarted controller does not advance `chunkRefreshSince` before
    every Fastlet has re-registered;
  - gate freshness: an `observedAt` older than `T_stale` means no deletes,
    and a gate that fails during a run stops it before the next batch;
  - writer downgrade: with the CronJob suspended, the downgrade waits for
    the Lease, and the following runs stay in dry-run;
  - prefix ownership: a missing or foreign `gc/owner.json` means dry-run,
    and `claim` never overwrites an existing marker;
  - repair: a rooted chunk removed by a plain `DELETE` is restored from its
    noncurrent version and verified by digest.

**Performance:**
- **Boundary:** the client-observed duration of the pause, snapshot and
  restore calls, plus the agent's publish stats log (logical, uploaded and
  reused bytes).
- **Environment:** 3 nodes, the store type and link speed, and the CPU model
  (SHA extensions or not) are recorded with each run.
- **Cache state:**
  - pause: a warm store (template chunks present), measured as the first
    pause and as repeat pauses;
  - same-node restore: a warm node cache;
  - other-node restore: a cold node cache.
- **Acceptance criteria:**
  - chunked pause, snapshot and other-node restore are at least 3× faster
    than `legacy` for the 3.5 GiB set;
  - uploaded bytes per repeat pause are below 10% of the set size;
  - `legacy` shows no regression beyond run-to-run noise.

## Rollout and rollback

**Rollout:**
0. **Gate before enabling `chunked`:** check that no store expiration rule
   (including bucket-wide rules) and no external cleanup job deletes
   objects under the shared `chunks/` prefix. Turning off the built-in GC
   does not protect against those deletions.
1. Upgrade every runtime-agent to the release with PR 1 (and PR 2), still
   in `legacy`. The readers are now everywhere.
2. Upgrade the Fastlets.
3. Switch `transfer.format: chunked`, one pool or node group at a time. The
   OpenSandbox chart rolls the DaemonSet on any transfer setting change.
4. Optionally, once every agent and Fastlet runs PR 2 or later and no
   downgrade of either below PR 2 is planned, set `chunkListOnly: true` to
   remove the whole-file hash from the pause path. This is the
   "finalize" step, similar to Ceph's `require-osd-release`.
5. **GC (after PR 4):** enable bucket versioning and a
   `NoncurrentVersionExpiration` rule, run `fsb-store gc` in dry-run, run
   `fsb-store gc claim` for the cluster's prefix, and enable destructive
   mode only once its gate holds: sets first, then chunks.

**Signals:**
- `runtime-agent chunked publish stats`: `uploadedBytes` vs `logicalBytes`,
  `reusedChunks`;
- `chunk request attempt failed, retrying` frequency;
- digest-mismatch and index-validation errors;
- pause, snapshot and restore latency and failure rate at the API.
- Prometheus counters for these are a possible follow-up; today they are
  structured logs.

**Rollback triggers:**
- any digest mismatch on restore;
- a sustained increase in pause or restore failures;
- a retry rate that does not settle.

**Disable:** set `transfer.format: legacy`. If `transfer.compression` is
`zstd`, set it to `none` in the same configuration change. `legacy` with
`zstd` fails validation, so an agent given only the format change does not
start.
- New publishes return to whole-file sets.
- Chunks already stored as `zstd` stay readable: the reader decodes each
  chunk by the encoding recorded in `chunks.json`, not by the agent's
  `transfer.compression` setting.
- Only the store format changes: the local cache optimizations do not
  depend on `transfer.format` (see Non-goals).
- With `chunkListOnly: true`, set it to `false` in the same change. Sets
  already published without `sha256` stay restorable on PR 2+ agents and
  Fastlets.
- The chunk reader stays on, so every chunked set already published remains
  restorable. This is the supported rollback.

**Downgrade limits:**
- **Agent below PR 1:** cannot read any chunked set from the store, and
  from a node-cached chunked set it can republish a manifest with an
  inherited `chunks` ref (see Compatibility). Do not downgrade below PR 1
  once `chunked` has been enabled anywhere, or recover by rolling forward.
- **Agent below PR 2 after `chunkListOnly: true`:** cannot read those
  manifests. That is why the setting is a separate, explicit last step.
- **Agent below PR 4 step 1 while destructive chunk GC is on:** suspend
  the `fsb-store gc` CronJob and wait until its Lease is released first
  (see "Writer downgrades and running sweeps").
- **Fastlet below PR 2 after `chunkListOnly: true`:** its readiness check
  requires the whole-file `sha256`, so it cannot boot sets published
  without one, RootfsOnly sets included. Same rule as for the agent.
- **Fastlet below PR 2 otherwise:** safe. Older Fastlets ignore the new
  fields and write whole-file hashes.
- **Recovery plan for a forced downgrade:** keep one pool on the new
  release and restore the affected Sandboxes there. To go below PR 2 after
  `chunkListOnly: true`: set `chunkListOnly: false`, snapshot again on a
  PR 2+ node, or delete, every set published without `sha256`, then
  downgrade. To go below PR 1: snapshot them again with `legacy`.

## Related links (optional)

- [#146: Snapshot transfer spends most of its time moving and re-hashing zeros](https://github.com/opensandbox-group/fast-sandbox/issues/146) (problem and measurements).
- [#213: chunked, zero-elided snapshot transfer](https://github.com/opensandbox-group/fast-sandbox/pull/213) (reference implementation, draft; supersedes [#147](https://github.com/opensandbox-group/fast-sandbox/pull/147)).
- [#214: standalone snapshot fixes](https://github.com/opensandbox-group/fast-sandbox/pull/214) (merged, split out of #213).
- [#244](https://github.com/opensandbox-group/fast-sandbox/pull/244): verified-digest memo and same-node cache commit (merged; closes [#57](https://github.com/opensandbox-group/fast-sandbox/issues/57)).
- [#61](https://github.com/opensandbox-group/fast-sandbox/issues/61): parallel and overlapped snapshot hashing, a separate PR.
- [#181](https://github.com/opensandbox-group/fast-sandbox/pull/181): native RootfsOnly S3 snapshots, whose sets are chunked too.
- [OpenSandbox#2422](https://github.com/opensandbox-group/OpenSandbox/pull/2422): chart value `runtime.transfer.*`.
- FSEP-0001 (rootfs-only snapshots on the OverlayBD path).
