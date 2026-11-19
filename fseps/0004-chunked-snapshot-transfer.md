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
- Validate the staged manifest before uploading anything: every native file
  entry needs a well-formed digest, and a `legacy` publish may not carry a
  `chunks` binding. Unless `format: chunked` and `chunkListOnly: true` are
  both set, the agent fills in any missing whole-file `sha256` and rewrites
  `SHA256SUMS` from the final manifest. It never writes an empty
  `SHA256SUMS`, which readers reject. A Fastlet acting on a stale
  capability, or an agent restarted into another mode between staging and
  publish, therefore costs a hash, not an unrestorable set.
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
2. **Then a separate, explicit mark-and-sweep tool** (`fsb-store gc`, run as
   a CronJob, with its own credentials that can list and delete; the agent's
   credentials stay as they are). It ships **dry-run only**. Destructive mode
   stays off until both gates below hold:
   - every live root can be enumerated (open question 1);
   - a stale sweep decision provably cannot delete a newly reused chunk
     (see "Remaining window"), with that interleaving in the GC acceptance
     tests.
   - **Roots:** every live reference (SandboxTemplate image index keys,
     snapshot index keys, checkpoint references of existing Sandboxes).
   - **Mark:** read `manifest.json` → `chunks.json` of every root and
     collect the chunk keys.
   - **Sweep:** delete a chunk only when it was unmarked in **two** marks
     and its `LastModified` was already older than the grace period `G` when
     mark 1 started. Right before each `DELETE`, the sweep `HEAD`s the chunk
     again and skips it if `LastModified` has changed.
3. **Publisher refresh (part of PR 4, before any deletion is enabled):** a
   publisher reuses an existing chunk only if it is younger than a refresh
   age `R` (`R` < `G`). A `HEAD` that finds an older chunk is followed by a
   re-`PUT`, which refreshes its `LastModified`. The in-memory known-key set
   follows the same rule: an entry older than `R` is checked again.

**Why the refresh is needed** (review on this PR): marks alone do not
protect a publish that starts after mark 1 and reuses an old, unmarked
chunk C by `HEAD`. Its manifest can land after the sweep, and C, unmarked
twice and old, would be deleted. The grace period does not help either,
since reuse does not change C's `LastModified`.

**Invariant with the refresh:** every chunk a publish binds was uploaded or
refreshed by a publish within the last `R`. So its `LastModified` stays
younger than `G` for at least `G` − `R` after the publish uses it. A mark
that starts after that has to find the publish's manifest, as long as the
manifest lands and becomes a root within `G` − `R`. Marks that start earlier
cannot make the chunk a sweep candidate, because the chunk is still younger
than `G`. Typical values: `R` = 1 day, `G` = 7 days. Re-`PUT`s fall only on
chunks that no publish has used for `R`, which are rare.

**Remaining window (not closed by this design):** the sweep decides from a
`HEAD`, then issues an unconditional `DELETE`. A publisher can act in
between. For example: GC does its final `HEAD`; the publisher refreshes the
chunk, publishes its manifest and completes a post-publish `HEAD`; only then
does GC issue the `DELETE`. Both checks succeeded, but the published set has
lost a chunk. Checks on either side only shrink this window. Closing it
needs a delete that fails when the chunk has been refreshed since the
sweep's decision, or another protocol with the same guarantee. Which
mechanism, and which stores support it, is open question 1. Until it is
answered, destructive GC stays off.

**Namespace cleanup** stays where it is today. The chunk GC only assumes
that a deleted namespace is no longer a root.

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
- The agent gets no new credentials, endpoints or permissions. GC uses
  separate credentials.

### Implementation plan

| PR | Content | Depends on |
|---|---|---|
| #214 | Standalone snapshot fixes split out of #213 | merged |
| #244 | Verified-digest memo, same-node cache commit (#57) | merged |
| cache | Fsynced cache writes, single-flighted cache ensures, independent of `transfer.format` | – |
| #61 | Parallel and overlapped snapshot hashing, independent of `transfer.format` | – |
| 1 | Chunk format, `chunks.json`, chunked publish and pull (recording into the #244 memo), always-on chunk reader, `transfer.format` and `transfer.compression`, manifest validation before upload, inherited `chunks` dropped by the publisher and both Fastlet assemblers, a chunk transfer cancelled between jobs fails instead of committing, a coalesced chunk upload counted once, docs | this FSEP accepted, cache |
| 2 | `chunkList` digest, digest-signed `PUT`s, `chunkListOnly` and the capability rule, agent `sha256` fill-in and `SHA256SUMS` rewrite, Fastlet `Health` refresh | 1, same release |
| 3 | Pooled store transport; P2P gateway pooling per open question 3 | 1 |
| 4 | `fsb-store gc` | 1 |
| – | OpenSandbox chart value and pin bump (OpenSandbox#2422) | 1 merged |

[#213](https://github.com/opensandbox-group/fast-sandbox/pull/213) is the complete reference implementation and spans PRs 1–3,
including the fixes from its review. Each PR is built fresh on `main`
(after #181 and #244), so PR 1 is not an existing subset of #213. #213 stays
a draft and is closed once PR 1 is open.

### Open questions

1. **GC roots and safety (gate for destructive GC):** which component can
   list every live reference (templates, snapshots, Sandbox checkpoints)?
   Should the controller own `fsb-store gc`? And which delete mechanism
   closes the sweep window ("Remaining window"): a delete conditioned on
   the chunk not having been refreshed, where the store supports it, or a
   different protocol?
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
| new, `chunked`, `chunkListOnly: true` | new | Whole-file `sha256` of native files is omitted. Only agents with PR 2 can read these manifests. |

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
- manifest validation fails before any object is uploaded;
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
  - publish and restore a native RootfsOnly set in `chunked`.
- **GC acceptance (PR 4, before destructive mode is enabled):**
  - dry-run lists candidates and deletes nothing;
  - a publish that reuses an old chunk after mark 1 keeps it;
  - the interleaving from "Remaining window": final `HEAD`, then the
    publisher's refresh, manifest and post-publish `HEAD`, then the GC
    `DELETE`. The published set must stay restorable.

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
4. Optionally, once no agent downgrade is planned, set `chunkListOnly: true`
   to remove the whole-file hash from the pause path. This is the
   "finalize" step, similar to Ceph's `require-osd-release`.

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
- With `chunkListOnly: true`, set it to `false` in the same change.
- The chunk reader stays on, so every chunked set already published remains
  restorable. This is the supported rollback.

**Downgrade limits:**
- **Agent below PR 1:** cannot read any chunked set from the store, and
  from a node-cached chunked set it can republish a manifest with an
  inherited `chunks` ref (see Compatibility). Do not downgrade below PR 1
  once `chunked` has been enabled anywhere, or recover by rolling forward.
- **Agent below PR 2 after `chunkListOnly: true`:** cannot read those
  manifests. That is why the setting is a separate, explicit last step.
- **Recovery plan for a forced downgrade:** keep one pool on the new agent,
  restore the affected Sandboxes there, and snapshot them again with
  `legacy`.
- **Fastlet downgrade:** always safe. Older Fastlets ignore the new fields
  and write whole-file hashes.

## Related links (optional)

- [#146: Snapshot transfer spends most of its time moving and re-hashing zeros](https://github.com/opensandbox-group/fast-sandbox/issues/146) (problem and measurements).
- [#213: chunked, zero-elided snapshot transfer](https://github.com/opensandbox-group/fast-sandbox/pull/213) (reference implementation, draft; supersedes [#147](https://github.com/opensandbox-group/fast-sandbox/pull/147)).
- [#214: standalone snapshot fixes](https://github.com/opensandbox-group/fast-sandbox/pull/214) (merged, split out of #213).
- [#244](https://github.com/opensandbox-group/fast-sandbox/pull/244): verified-digest memo and same-node cache commit (merged; closes [#57](https://github.com/opensandbox-group/fast-sandbox/issues/57)).
- [#61](https://github.com/opensandbox-group/fast-sandbox/issues/61): parallel and overlapped snapshot hashing, a separate PR.
- [#181](https://github.com/opensandbox-group/fast-sandbox/pull/181): native RootfsOnly S3 snapshots, whose sets are chunked too.
- [OpenSandbox#2422](https://github.com/opensandbox-group/OpenSandbox/pull/2422): chart value `runtime.transfer.*`.
- FSEP-0001 (rootfs-only snapshots on the OverlayBD path).
