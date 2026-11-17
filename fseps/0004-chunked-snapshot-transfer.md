---
title: "Chunked Snapshot Transfer"
authors:
  - "@kaustubh-765"
creation-date: 2026-11-17
last-updated: 2026-11-17
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
  sets are chunked.
- The template builder keeps publishing whole-file sets (possible
  follow-up).
- Local optimizations that change no protocol go in separate small PRs and
  are not part of this proposal:
  - the verified-cache marker, with fsynced cache writes and single-flighted
    cache ensures;
  - template snapshots kept in the node cache (#57);
  - parallel and overlapped hashing (#61).

  In #213 the marker and the fsynced cache writes are still switched on by
  `transfer.format: chunked`. The split removes that coupling: these PRs land
  before PR 1, independent of `transfer.format`, and #213 is rebased onto
  them. After that, `transfer.format` selects only the store format. Sparse
  cache files stay a property of the chunked pull, which skips zero chunks.

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
- Chunk requests use a 10 s per-attempt timeout with retries.
- In chunked mode, store traffic uses a pooled transport: 64 idle
  connections per host, 30 s idle timeout.

**Fastlet:**
- Learns the agent's mode from optional `transferFormat` and
  `chunkListDigest` fields in `HealthResponse` and `PublishImageResponse`.
- Older agents omit the fields, which means legacy with whole-file hashes.

### Shared-chunk retention and garbage collection

**Today:**
- fast-sandbox never deletes store objects. Build namespaces accumulate and
  are cleaned outside the system.
- Chunking changes one fact: a chunk can belong to many sets. Deleting a
  namespace no longer frees its data, and a lifecycle rule on `chunks/`
  could delete data that live sets still need.

**Proposal:**
1. **First release: no store-side deletion**, the same as today. Document
   that lifecycle rules must not target `chunks/`.
2. **Then a separate, explicit mark-and-sweep tool** (`fsb-store gc`, run as
   a CronJob, dry-run by default, with its own credentials that can list and
   delete; the agent's credentials stay as they are):
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

**Remaining window:** a re-`PUT` that lands between the sweep's last `HEAD`
and its `DELETE` is lost. The final `HEAD` shrinks this to the time of one
request. To close it, the publisher `HEAD`s the chunks it refreshed once
more after its manifest has landed, and uploads any that are missing. Where
the store supports conditional deletes, the sweep can use them instead.
Which of the two to require is part of open question 1.

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
  chunkListOnly: false    # see "Rollout and rollback"
```

- An unknown value, or `zstd` together with `legacy`, fails agent startup.
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
| 1 | Chunk format, `chunks.json`, chunked publish and pull, always-on chunk reader, `transfer.format` config, docs | this FSEP accepted |
| 2 | `chunkList` digest, digest-signed `PUT`s, `chunkListOnly` | 1 |
| 3 | Pooled store transport; P2P gateway pooling per open question 3 | 1 |
| 4 | `fsb-store gc` | 1 |
| – | OpenSandbox chart value and pin bump (OpenSandbox#2422) | 1 merged |

[#213](https://github.com/opensandbox-group/fast-sandbox/pull/213) is the complete reference implementation and already spans PRs 1–3
(the `chunkList` digest, digest-signed `PUT`s, the pooled transport, the 10 s
attempt timeout and the `HEAD` 403 fallback are all in it). Landing the split
means carving up that branch. PR 1 is not an existing subset of it. #213 stays
a draft until this proposal is decided.

PR 1 also covers the inherited `chunks` binding (see Compatibility): the
publisher drops it in `legacy`, and the Fastlet drops it when it assembles a
snapshot manifest.

### Open questions

1. **GC roots and safety:** which component can list every live reference
   (templates, snapshots, Sandbox checkpoints)? Should the controller own
   `fsb-store gc`? And for the remaining sweep window: does the publisher's
   post-publish check suffice, or should GC require stores with
   conditional deletes?
2. **Rollback:** is the `chunkListOnly` finalize step acceptable (see
   "Rollout and rollback"), or should manifests always keep the whole-file
   `sha256` and accept the hash cost?
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
    from the manifest the Sandbox booted from and keeps its other fields. A
    Sandbox restored from a chunked set and paused again with the agent in
    `legacy` would publish a `chunks` ref to a `chunks.json` that was never
    uploaded. The `legacy` publisher drops the binding (#213 does this:
    `bindChunkIndex` with a nil ref), and from PR 1 the Fastlet drops it too.
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
| new, `chunked` | new | Chunked sets; whole-file `sha256` is omitted only with `chunkListOnly: true`. |

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
- `chunkListOnly` off keeps `sha256`.

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
  - switch `chunked` → `legacy` and restore the earlier chunked checkpoints
    and snapshots;
  - restore a chunked snapshot, switch to `legacy`, pause again, and verify
    that the new manifest has no `chunks` ref and restores on an agent from
    before PR 1.

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

**Disable:** set `transfer.format: legacy`.
- New publishes return to whole-file sets.
- Only the store format changes: the local cache optimizations no longer
  depend on `transfer.format` (see Non-goals). In #213 as it stands today,
  this switch also turns off the verified-cache marker.
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
- [#57](https://github.com/opensandbox-group/fast-sandbox/issues/57) and [#61](https://github.com/opensandbox-group/fast-sandbox/issues/61): local optimizations, handled in separate PRs.
- [OpenSandbox#2422](https://github.com/opensandbox-group/OpenSandbox/pull/2422): chart value `runtime.transfer.*`.
- FSEP-0001 (rootfs-only snapshots on the OverlayBD path).
