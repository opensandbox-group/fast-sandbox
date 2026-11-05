---
title: "Rootfs-Only Snapshots on the OverlayBD Path"
authors:
  - "@Pangjiping"
creation-date: 2026-11-05
last-updated: 2026-11-05
status: draft
---

# FSEP-0001: Rootfs-Only Snapshots on the OverlayBD Path

## Problem and goals

Today every snapshot on the Firecracker runtime is a full snapshot: the produced OCI pair carries the guest memory (`memfile`) and device state (`vmstate.bin`) next to the committed writable layer (`internal/runtime/firecracker/oci_upper.go`, `cmd/fsb-vm/pack.go`). Full snapshots buy millisecond in-place resume, but they cost three things:

1. **Artifact weight.** The snapfiles layer always contains a memory-sized file. The OCI staging footprint formula is `3*(upper + memory) + 512MiB` (`snapshot_driver.go:403-420`), and the pause window contains a memory dump even with the spill-directory optimization (7.2s → 258ms at 512MiB, `docs/guides/sandbox-snapshots.md`).
2. **CPU portability.** Restore compatibility is pinned to the snapshot's CPU family/model/stepping, CPU template, and an exact Firecracker version (`internal/artifacts/manifest.go:522-558`, `docs/guides/snapshot-cpu-compatibility.md`). A full snapshot taken on one node generation cannot resume on another.
3. **Resume semantics that many use cases do not need.** Persisting a workspace between sessions, checkpointing build or scratch state, or producing a template that many sandboxes start from only need the disk. For those cases the memory image is pure overhead, and the restraints it imposes (CPU pinning, memory-sized uploads) are pure loss.

This proposal introduces **rootfs-only snapshots** on the OverlayBD (OCI) path: a snapshot that persists only the writable layer and whose consumers **boot** rather than restore-in-place. The design follows the model of E2B's filesystem-only snapshots: give up process continuity explicitly, gain a smaller, faster, CPU-portable artifact.

### Goals

- Add a `mode` option to live sandbox snapshots (`CreateSandboxSnapshot` / `SandboxSnapshot` CRD): `Full` (default, unchanged) or `RootfsOnly`, **OverlayBD path only** in this proposal.
- Produce a rootfs-only artifact as the existing OCI pair shape with a degenerate snapfiles layer, so resolution, digest addressing, immutable-tag publishing, and owner fencing are unchanged.
- Make restore **data-driven**: the artifact's manifest declares the mode; the runtime branches on it. `CreateSandboxRequest`, `PauseSandboxRequest`, `ResumeSandboxRequest`, and `RestoreSpec` do not change.
- Add the missing production cold-boot branch (today the driver is restore-only, `restore.go:3-12`) by reusing the template builder's boot recipe, including activation of the ADR-004 guest-kernel hook on nodes.
- Keep the snapshot pause window at least as good as today's spill-optimized window, and remove the memory image from staging and publish entirely.

### Non-goals

- The S3/native artifact path. It can adopt the same mode later; this proposal rejects `RootfsOnly` there.
- `PauseSandbox` (checkpoint kind) with rootfs-only semantics. Changing what "Paused and resumed" means for a live sandbox is a separate decision; `SnapshotKindCheckpoint + RootfsOnly` is rejected by validation in this proposal.
- Diff or incremental memory snapshots (issue #56), same-node prewarming (#57), and manifest hashing overlap (#61).
- Per-instance entrypoint or env overrides at boot time. Cold boot makes this possible later; it is not specified here.

## Proposal and tradeoffs

### Artifact contract: a degenerate pair, self-describing

A rootfs-only snapshot keeps the exact shape of today's OCI checkpoint publish (`oci_checkpoint.go:100-158`, `pack.go:52-127`):

```
<id>_rootfs:     layers = [...inherited parent layers (remote refs), frozen upper committed as one new zfile]
<id>_snapfiles:  single ext4 layer containing manifest.json (+ optional vault/policy state files) — no vmstate.bin, no memfile
```

The rootfs manifest is unchanged in shape; this is the key observation that makes the proposal cheap: **the rootfs half of the pair already is the disk-only snapshot**. Only the snapfiles half degenerates from a memory-sized layer to a kilobyte-scale one. `manifest.json` stays exactly where it is today — a plain file on the snapfiles ext4 layer (`cmd/fsb-vm/build.go:719`) — so it remains the pair's resolution anchor and the first artifact mounted at create (`cmd/fsb-vm/create.go:186-196`, where the ordering exists precisely because manifest.json supplies the rootfs size; it now also carries `snapshotMode` and `kernel.cmdline`). The degenerate snapfiles disk is no longer sized from `memoryMiB`; it becomes a small fixed ext4 holding manifest.json plus optional state files. `resolvePair` digest addressing, `pushImmutableManifest` one-shot tags, `layoutIndex` two-image validation, and checkpoint owner annotations all continue to work unmodified.

The shared `VMManifest` (`internal/artifacts/manifest.go`) gains two additive, optional fields:

- `platform.snapshotMode`: `"full"` (absent = full, for all existing manifests) or `"rootfsOnly"`.
- `kernel.cmdline`: the complete kernel boot arguments needed to cold-boot the disk (captured from the source template's build; see the restore section). `kernel.version` already exists and becomes the node-side kernel asset key.

Why not a single image without the pair? Every consumer (`ocitemplate.ResolveFrom`, `resolvePair`, `ValidateDigestReference`, resume references recorded in `status.runtime.checkpoint` and `SandboxSnapshotStatus`) addresses the pair by the snapfiles manifest digest. Dropping the pair would churn the entire resolution contract for no functional gain. The degenerate snapfiles layer costs kilobytes.

Why data-driven restore instead of a request field? The artifact knows what it is; the requesters should not have to say so, and pause/resume flows that carry only `{ManifestRef, ArtifactDigest}` keep working without protocol changes. This also matches the "snapshot content vs restore policy are orthogonal" split: a future `resumeWithoutMemory` option would be a request-side choice over a full artifact, the mirror image of this proposal.

### RPC and contract changes

Four layers carry the mode, all aligned by `make generate`. Only the snapshot-creation side changes; the restore side reads the artifact.

**FastPath gRPC** (`api/proto/v2/fastpath.proto`):

```proto
enum SnapshotMode {
  SNAPSHOT_MODE_UNSPECIFIED = 0; // resolves to FULL for compatibility
  SNAPSHOT_MODE_FULL = 1;
  SNAPSHOT_MODE_ROOTFS_ONLY = 2;
}

message CreateSandboxSnapshotRequest {
  // ... existing fields 1-4 unchanged ...
  // mode selects what the snapshot persists. FULL (default) captures memory
  // and device state; ROOTFS_ONLY persists only the writable rootfs layer and
  // consumers boot from disk. ROOTFS_ONLY requires the OverlayBD artifact path.
  SnapshotMode mode = 5;
}

message SandboxSnapshotInfo {
  // ... existing fields 1-13 unchanged ...
  SnapshotMode mode = 14;
}
```

`CreateSandboxSnapshot` rejects `ROOTFS_ONLY` with `INVALID_ARGUMENT` before persisting the intent when the sandbox is not on the OCI path — the sandbox is not touched, matching the fail-fast style of the existing reentrancy checks (`fastpath/snapshot.go:117-131`). `SandboxSnapshotInfo.mode` reports the mode of an existing snapshot so clients can tell artifact classes apart.

**SandboxSnapshot CRD** (`api/v1alpha2/sandboxsnapshot_types.go`):

```go
// +kubebuilder:validation:Enum=Full;RootfsOnly
// +kubebuilder:default=Full
type SandboxSnapshotMode string

type SandboxSnapshotSpec struct {
    SandboxRef  SandboxRef          `json:"sandboxRef"`
    TemplateName string             `json:"templateName,omitempty"`
    // Mode selects what the snapshot persists. RootfsOnly requires the
    // OverlayBD artifact path and is rejected for checkpoint-style use.
    Mode SandboxSnapshotMode        `json:"mode,omitempty"`
}
```

The spec stays immutable by convention (one-shot intent), and `SnapshotSpecHash` automatically covers the new field, so request_id replays with a different mode are rejected as spec drift.

**Fastlet protocol** (`internal/protocol/fastlet/types.go`): `SnapshotSpec.Mode` (`full` default / `rootfsOnly`). `validateSnapshotRequest` (`internal/fastlet/sandbox/snapshot.go:176-193`) rejects `rootfsOnly` for non-OCI sandboxes and for `SnapshotKindCheckpoint`.

**Runtime contract** (`internal/runtime/contract/runtime.go`): `SnapshotInput.Mode` with the same two values. The contract comment extends: a rootfs-only capture must still quiesce the drive before freezing the upper layer and must resume the VM on both success and failure paths.

SDKs regenerate stubs only; `pkg/sandboxclient` and the Python SDK add optional convenience later. `fastctl` is unchanged.

### Snapshot production: the fastest rootfs export

The window in `dumpRunningSandbox` (`snapshot_driver.go:518-714`) for OCI + rootfs-only becomes:

1. `PATCH /vm {"state":"Paused"}`.
2. **Drain step (v1): keep `PUT /snapshot/create` with both output paths pointed at the spill directory, and discard the files after resume.** The existing safety argument for `captureOCIUpper` is that it runs "only after snapshot creation has drained/flushed the drive" (`oci_upper.go:22-24`): overlaybd-ublkd is a userspace block device daemon, so a pre-pause reflink of the upper files cannot claim consistency the way the native path does. Reusing the snapshot-create drain keeps the consistency argument byte-identical to today's while the dump cost collapses to a spill write (258ms-class at 512MiB) instead of entering staging.
3. `captureOCIUpper` unchanged: reflink, else `SEEK_DATA/SEEK_HOLE` extent copy, else zero-block scan; writes `rootfs.upper.data/index` + `rootfs-upper.json`.
4. Resume. Delete the discarded drain outputs. Everything expensive that remains (pack, commit-to-zfile, squash, push) already runs asynchronously after `OnPublishing` (`oci_checkpoint.go:98-158`).

Net effects versus a full OCI snapshot: the pause window is no worse than today's spill-optimized window; staging drops from `3*(upper+memory)+512MiB` to roughly `2*upper + slack` (`dumpFootprintBytes` OCI branch updated); the published snapfiles layer shrinks from memory-sized to kilobytes; no vmstate/memfile ever reaches the bundle or the registry.

`fsb-vm pack` gains `--rootfs-only`: `validatePackBundle` (`pack.go:143-185`) stops requiring `vmstate.snap`/`memory.snap` and drops the `memory.snap == memoryMiB` check; `captureSnapfiles` writes manifest.json (plus extras) only. Everything else in pack (zfile commit, `remote-sources.json` inheritance, squash, `writeTemplatePair`) is untouched.

A pre-pause reflink of the upper files is explicitly rejected: ublkd's userspace write path makes "disk-behind-memory" reasoning invalid on this path, and unlike the native path there is no memory image whose consistency we would be protecting.

**Follow-up optimization (out of scope):** replace the drain-by-snapshot-create with a bounded ublkd quiesce wait (settle polling on the upper files), removing the memory-sized spill write from the window entirely. Gated on a stress-validated consistency argument; see the test plan.

### Restore: booting a new sandbox from a rootfs-only artifact

`EnsureSandbox`'s OCI branch learns one branch point. `readPreparedOCIMetadata` (`oci.go:218-262`) reads the manifest from the (tiny, shared-cached) degenerate snapfiles mount; when `platform.snapshotMode == "rootfsOnly"` the driver takes the new **boot path** instead of `LoadSnapshot`:

1. `fsb-vm create --prepare-only` is unchanged: resolve the pair, attach the rootfs device (all layers as lowers, lazy range-read from the registry, fresh blank hybrid upper), acquire the ublk lease. The acquire order is untouched — the degenerate snapfiles still attaches and mounts **first**, because manifest.json on it supplies the rootfs virtual size and now the mode and boot cmdline; degenerate snapfiles ride the existing shared-snapfiles cache, whose key dedupes by layers and credential scope as today, so many sandboxes from the same artifact share one KB-scale mount. Chain snapshots also keep working unchanged: `captureOCIUpper` reads the source manifest from the current sandbox's snapfiles mount, degenerate or not.
2. `prepareOCIInstance` skips the vmstate/memfile bind mounts and instead bind-mounts the **guest kernel** into the jail root.
3. The driver issues the boot sequence, reusing the builder's proven recipe (`cmd/sandboxtemplate-builder/snapshot_stage.go:335-393` and `cmd/fsb-vm/build_firecracker.go:135-216` are the references; the runtime `Client` already exposes the calls):
   - `PUT /machine-config` from `manifest.machine` (`smt=false`; CPU template chosen by host vendor as at build time — **not** required to match the source snapshot's template).
   - `PUT /balloon` (free-page-reporting) and `PUT /entropy` (virtio-rng; together with `random.trust_cpu=on` this is required or execd's RNG blocks forever at session-id generation).
   - `PUT /boot-source` with the node kernel asset and `manifest.kernel.cmdline`. The cmdline is captured at template build and carries the baked network (`ip=172.30.0.3::172.30.0.1:...::eth0:off`), `init=<guest-init path>`, `root=/dev/vda rw`, and console arguments — identical strings to `snapshot_stage.go:124-126`.
   - `PUT /drives/rootfs` with the relative `rootfs.dev` path (the existing drive-naming contract).
   - `PUT /network-interfaces/eth0` with the slot tap and the manifest's baked guest MAC.
   - `InstanceStart`.
4. Admission deltas versus restore: `requireStaticGuestNetwork` stays (the boot cmdline needs the baked network); `checkRestoreCompatibility` is **exempted** — CPU family/model/stepping, CPU template, and exact-Firecracker-version matching are vmstate constraints and do not apply to a boot. Only `cpuArch` remains. This is the portability dividend: a rootfs-only artifact boots across node generations and Firecracker upgrades.

**Guest kernel assets (activating the ADR-004 hook).** `resolveKernel` (`create.go:353-367`) already defines `<kernel-dir>/vmlinux-<manifest.kernel.version>` but only as an existence check on the standalone path. This proposal activates it: the runtime-agent installs kernel bundles next to the OCI tool bundle using the same immutable release-dir channel (`/opt/fast-sandbox/oci/releases/<digest>` style); `hostready.CheckOCI` verifies `vmlinux-<version>` presence and exports a node label; `fsb-vm create` records the resolved `kernel_path` in `state.json` (also under `--prepare-only`, where the check currently returns early); the driver reads it for the boot-source call. Nodes without the labeled kernel fail sandbox admission **before** any snapshot side effect, and snapshot creation itself remains possible everywhere (kernels are needed at boot, not at capture). Kernel verification today is filename-convention only; recording the kernel blob digest in the manifest for verification at boot is a hardening follow-up listed under open questions.

**How the sandbox becomes usable: guest-init, execd, entrypoint.** Nothing new is invented here — the builder's bootstrap contract is simply allowed to run again:

- `guest-init` (the injected PID 1, `convert.go:338-415`) is already on the disk. On every boot it mounts `/proc`, `/sys`, `/dev` behind `mountpoint` guards, mounts **fresh tmpfs on `/run` and `/tmp`** (masking stale runtime state), brings up loopback, sources `/etc/sandbox-init.env`, and runs the readiness loop. It was audited for repeat runs during template builds; repeated cold boots exercise the same code path.
- **execd registration requires no guest-side work.** execd never registers anywhere today: routing is host-side discovery — the slot netns DNATs `slotIP:44772` to the guest's baked IP, fastlet-proxy publishes the route from placement, and `X-Fast-Sandbox-Route-Credential` is an Ed25519 token issued and verified entirely host-side (`internal/dataplane/`). After a cold boot the only thing that must happen is execd listening on 44772 again: guest-init launches `/opt/opensandbox/bootstrap.sh`, which starts execd; fastlet's existing `InitializeInstance` HTTP/TCP probe (`infra_lifecycle.go:218-267`) re-probes until it answers and turns the sandbox `DataPlaneReady`. The egress MITM CA was installed into the guest trust store at the original snapshot boot and lives on the disk, so cold-booted sandboxes inherit it exactly like restored ones ("restored sandboxes trust the CA from first boot").
- **The user entrypoint re-executes on every boot.** `/etc/sandbox-init.env` carries `ENTRYPOINT`; guest-init passes it as `BOOTSTRAP_CMD` to `bootstrap.sh` (or `setsid sh -c` directly). This is a semantic change worth stating loudly: a full restore never re-runs the entrypoint (the process is in the memory image), a rootfs-only boot always does. User processes running at snapshot time are not resurrected; anything that must survive must live on disk, and anything that must run must be restartable (or re-run via ENTRYPOINT).

**Readiness signaling.** Production does not depend on the serial console. The VM is `RuntimeReady` when Firecracker reports Running; guest-level readiness is the existing re-probe: the readiness contract inside the guest (ReadinessSpec probe → execd ping → warmup+healthcheck, in that priority) gates `SANDBOX_READY`, and host-side the fastlet infra probe supplies the externally visible signal. The boot timeout budget uses the builder's calibration (boot-to-ready measured 2.1s on the reference builder for an execd-ping template; budget `warmupSeconds + 60s`, floor 300s, `snapshot_stage.go:596-602`). Serial console capture stays available for diagnostics, as in the builder.

`fast-sandbox.json` (VM discovery metadata) is boot-mode agnostic and continues to be written best-effort.

### Runtime guarantees and constraints

| Aspect | Full snapshot (today) | Rootfs-only snapshot |
|---|---|---|
| Writable layer (disk) | Preserved | Preserved |
| Guest RAM | Preserved | Discarded |
| Processes at snapshot time | Restored in place | Terminated; boot path re-runs |
| execd | Resumes with memory | Restarted by guest-init, re-probed by fastlet |
| User entrypoint | Never re-runs | Re-runs on every boot |
| `/run`, `/tmp` | Carried in memory image | Fresh tmpfs per boot (stale state masked) |
| Route/credentials | Host-side, unchanged | Host-side, unchanged |
| Resume latency | Milliseconds (restore) | Seconds (boot + readiness re-probe) |
| CPU compatibility | FMS + CPU template + exact Firecracker version | Architecture only |
| Artifacts | Pair + memory-sized snapfiles | Pair + kilobyte snapfiles |
| First-request latency | Guest network stack warm | ARP/neighbor caches cold; first request slower (observed pattern: sub-second settle) |

Explicit constraints:

- **OverlayBD path only.** `RootfsOnly` with the S3/native artifact format is rejected by validation on every layer.
- **Template kind only.** `SnapshotKindCheckpoint + RootfsOnly` is rejected; `PauseSandbox` is untouched and keeps producing full checkpoints.
- **Kernel-present nodes.** Booting a rootfs-only artifact requires the node kernel asset matching `manifest.kernel.version`; admission fails fast when the hostready label is absent.
- **Guest contract.** The injected guest-init is idempotent (verified by code audit: mount guards, tmpfs masking, stateless readiness loops). `bootstrap.sh` ships in the execd image outside this repository; its re-run behavior — notably the MITM CA trust-store installation — is an upstream contract item this proposal adds to the execd image's verification checklist. Application-level restart idempotency (locks, one-time migrations) is the user's responsibility and is documented as such.
- **Layer chain growth.** Each rootfs-only snapshot commits one new zfile layer over the inherited lowers; the 24-layer/16-layer squash with the never-merged `BaseLayers` boundary (`pack.go:228-257`) applies unchanged. Cheaper snapshots invite more frequent checkpointing, which accelerates chain growth and squash-driven blob pulls (`ensureLocalBlob`); per-mode squash thresholds are an open question below.
- **One-directional artifact class.** A rootfs-only pair cannot later be "upgraded" to a full pair: the memory of that moment is gone. Semantically this is the same as any missed snapshot; stating it prevents surprises.

### Tradeoffs, risks, and open questions

The main tradeoff is resume latency and process continuity in exchange for artifact weight, export speed, and CPU portability — accepted because the targeted use cases (workspace persistence, build-state checkpoints, template seeding) do not need running processes back. The main risks:

1. **Cold boot is a new production path.** The driver has never booted a VM outside the builder. Mitigation: the recipe is copied from the builder's proven sequence, and the boot branch is inert until a rootfs-only artifact exists.
2. **Drain semantics.** v1 reuses snapshot-create's drain, keeping today's consistency argument; the quiesce-only optimization is deferred until stress-validated.
3. **bootstrap.sh re-run semantics** is owned outside this repository; treated as a contract item with an E2E guard (repeated boots from the same artifact).

Open questions for review: per-mode squash thresholds; recording the kernel blob digest in the manifest and verifying at boot; whether `templateName`-addressed creation should require the label-based scheduling hint or rely on fastlet-side failure; the pause-kind (checkpoint) rootfs-only variant and the S3-path variant as separate follow-up proposals.

## Compatibility

- **Defaults preserve behavior.** `mode` absent or `UNSPECIFIED` means `Full` on every layer; all existing snapshots, manifests, and flows are byte-compatible. `platform.snapshotMode` absent means `full`, so every existing `VMManifest` decodes as before.
- **Existing resources.** Existing full-snapshot artifacts are unaffected and remain resumable by old and new drivers. New rootfs-only artifacts are only bootable by drivers that implement this proposal; an older driver fails closed with an explicit restore validation error (the degenerate snapfiles layer has no `vmstate.bin`/`memfile`), never a silent full-restore attempt.
- **Version skew.** New controlplane with old fastlet: the fastlet rejects the unknown `SnapshotSpec.Mode` value at admission and the failure surfaces in `SandboxSnapshotStatus` — fail closed. Old controlplane with new fastlet: no mode field, full snapshots only. Mixed-version fleets therefore never produce half-understood artifacts.
- **Public interfaces.** Additive only: proto enum plus one optional field on two messages; one optional CRD field with a default; internal protocol and runtime-contract structs gain one field. SDKs regenerate stubs (`make generate`); no SDK or CLI breaking change. `PauseSandbox`/`ResumeSandbox`/`CreateSandbox` requests are deliberately untouched (data-driven restore).
- **CRD validation.** `SandboxSnapshotSpec.Mode` uses an enum with `Full` default; immutability follows the existing one-shot spec convention rather than new CEL rules.

## Test plan

Unit (run anywhere, `make test SCOPE=unit`):

- Mode plumbing through all four contract layers, including `SnapshotSpecHash` covering the new field (request_id replay with a changed mode must be rejected).
- Validation matrix: `rootfsOnly` accepted for OCI template-kind; rejected for the S3 path, for `SnapshotKindCheckpoint`, and by an old-style fastlet stub (unknown value).
- `validatePackBundle` rootfs-only branch (no vmstate/memfile requirement); `assembleSnapshotManifest` writes `snapshotMode` and `kernel.cmdline`; `dumpFootprintBytes` OCI branch without the memory term.
- Admission branch: `readPreparedOCIMetadata` dispatches on `platform.snapshotMode`; compatibility exemption applies only to `rootfsOnly`.

E2E and integration (Linux, root, KVM, ublkd, XFS StateRoot; skipped platforms reported):

- **Lifecycle:** create a sandbox from an OCI template → write a file and start a background process → `CreateSandboxSnapshot(mode=ROOTFS_ONLY)` → source keeps running (ping gap within the spill-optimized budget) → create a new sandbox from the snapshot → the file is readable, the old process is gone, execd answers `/ping`, entrypoint re-ran (boot-marker file with a boot count), sandbox reaches `DataPlaneReady` via the re-probe path.
- **Repeat boots:** boot the same rootfs-only artifact twice (chain: snapshot the booted sandbox again) — guest-init and bootstrap re-runs stay healthy; layer chain stays ≤ 24 with squash firing and `BaseLayers` preserved (`oci_chain_e2e_test.go` pattern extended).
- **Consistency:** run an fsync-heavy workload inside the guest during capture; the frozen upper image passes `e2fsck -fn` (mounting the assembled device read-only).
- **Portability:** boot a rootfs-only artifact on a node whose CPU template differs from the capture node (the headline capability); a full snapshot from the same source still restores only on the compatible node.
- **Failure recovery:** kill the fastlet mid-publish and retry (idempotent one-shot tag semantics); boot failure (missing kernel asset) fails admission before side effects; crash during the drain window leaves the source resumed (existing recovery path).

Performance (state environment and cache state; reuse `scripts/integration-env.sh` instrumentation `vm_pause_window_ms`, `ping_max_gap_ms`, `checkpoint_dump_window_ms`): pause window for rootfs-only vs full at 512MiB and 2GiB memory sizes; boot-to-first-request vs restore-to-first-request; artifact and staging sizes (no memory term); registry push bytes (one new layer vs layer + snapfiles).

Acceptance criteria: rootfs-only pause window no worse than the full-snapshot spill window; boot-to-DataPlaneReady within the builder-calibrated budget; zero vmstate/memfile bytes in the bundle, staging, and registry; version-skew matrix behaves fail-closed.

## Rollout and rollback

Rollout is per-request opt-in with no cluster flag; the pieces land inert-first. (1) Manifest fields, kernel asset channel, hostready label, and the driver boot branch (inert: no rootfs-only artifacts exist yet, and boot requires one). (2) Mode plumbing across proto, CRD, fastlet, runtime contract, with validation and `make generate` outputs. (3) Docs updates (`sandbox-snapshots.md`, `overlaybd-oci-sandboxes.md`, `artifact-manifest-reference.md`, `pause-resume-snapshot-integration.md`) and the execd-image contract note.

Signals to monitor: snapshot success/failure rates by mode, boot failure and readiness-timeout rates for rootfs-only images, pause-window and boot-latency histograms, layer counts and squash frequency on chains, node kernel-asset label coverage.

Rollback triggers: boot failures or readiness timeouts above budget, any consistency finding on frozen images, or version-skew confusion. Rollback = stop issuing `mode=ROOTFS_ONLY` requests and stop creating from rootfs-only templates; no component downgrade is required because full snapshots are the untouched default. Limits: rootfs-only artifacts already in the registry cannot be booted by rolled-back drivers (degenerate pair), and their disk data is only reachable by booting a new-mode driver from them or by re-snapshotting a sandbox that is still running from one. Kernel bundles left on nodes are harmless.

## Related links (optional)

- Reference model: E2B "Filesystem-only snapshots" (docs.e2b.dev/sandbox/filesystem-only-snapshots).
- Implementation anchors: `internal/runtime/firecracker/{snapshot_driver,oci,oci_upper,oci_checkpoint,restore,driver,client}.go`, `cmd/fsb-vm/{create,pack,overlaybd,build_firecracker}.go`, `internal/ocitemplate/{registry,upper}.go`, `internal/artifacts/manifest.go`, `internal/fastlet/sandbox/snapshot.go`, `internal/controlplane/fastpath/snapshot.go`.
- Related guides: `docs/guides/sandbox-snapshots.md`, `docs/guides/overlaybd-oci-sandboxes.md`, `docs/guides/snapshot-cpu-compatibility.md`, `docs/design/adr-fsb-vm-cli.en.md` (ADR-004 kernel hook).
- Follow-ups: pause-kind rootfs-only variant; S3/native path parity; ublkd quiesce drain; per-mode squash thresholds; kernel digest pinning; per-instance entrypoint override at boot.
