# Snapshot CPU compatibility — scheduling by T2 / T2A / none

Golden snapshots embed the build host's CPU state (CPUID mask, MSRs,
XSTATE). Restoring one on the wrong CPU is a hard failure at
`snapshot/load` — or worse, undefined behavior — so the runtime enforces
a compatibility check before staging (OSEP-0024 Phase 1), and the agent
labels each node with the snapshot tiers it can restore. This document
defines the tiers, their authoritative basis, and how to schedule
against them.

## The compatibility contract

Firecracker's snapshot docs state the invariant principle
([docs/snapshotting/versioning.md](https://github.com/firecracker-microvm/firecracker/blob/v1.16.1/docs/snapshotting/versioning.md),
"Snapshot compatibility → CPU model"):

> Snapshots are not compatible across CPU architectures and even across
> CPU models of the same architecture. They are only compatible if the
> CPU features exposed to the guest are an invariant when saving and
> restoring the snapshot. The trivial scenario is creating and restoring
> snapshots on hosts that have the same CPU model.
>
> Restoring from an Intel snapshot on AMD (or vice-versa) is not
> supported.

A CPU template makes the exposed feature set an invariant across the
hosts the template covers ([docs/cpu_templates/cpu-templates.md](https://github.com/firecracker-microvm/firecracker/blob/v1.16.1/docs/cpu_templates/cpu-templates.md)):

> A real world use case for this is representing a heterogeneous fleet
> (a fleet consisting of multiple CPU models) as a homogeneous fleet, so
> the guests will experience a consistent feature set supported by the
> host.

## The three tiers

The builder pins the vendor-native template and falls back to the raw
host CPUID when the host refuses it
(`cmd/sandboxtemplate-builder/snapshot_stage.go`); the manifest
`compatibility.cpuTemplate` records which of the three tiers produced
the snapshot, and restore admission enforces the matching rule.

| Tier | Snapshot built on | Restores on | Allowlist (Firecracker v1.16.1, exact FMS incl. stepping) |
|---|---|---|---|
| `T2` | Intel host that accepts T2 | Any CPU in the T2 allowlist | Skylake-SP `6/85/4`, Cascade Lake-SP `6/85/7`, Ice Lake-SP `6/106/6` |
| `T2A` | AMD EPYC Milan (stepping 1) | Any CPU in the T2A allowlist | Milan `25/1/1` (CPUID EAX `0x00a00f11`) |
| `none` | Host that refused the pinned template (e.g. Sapphire Rapids, Emerald Rapids, EPYC Genoa/Turin) | Only the identical CPUID identity: vendor/family/model/**stepping** | — |

The `none` tier compares **exact FMS including stepping**: steppings within
one model can differ in CPUID features — Skylake-SP (8163, stepping 4) lacks
`AVX512_VNNI` while Cascade Lake-SP (8269CY, stepping 7) has it — so a raw
snapshot restoring across that skew would fault guest code at execution
time, exactly the post-load failure class admission exists to eliminate.

Rules that apply to every tier:

- **Firecracker version equality.** The node's VMM version must equal the
  version recorded in the manifest. Cross-version vmstate restores are
  unsupported (see the microVM-state section of
  [versioning.md](https://github.com/firecracker-microvm/firecracker/blob/v1.16.1/docs/snapshotting/versioning.md)),
  and the allowlist table is per version anyway.
- **`hostKernel` is recorded but not enforced** in this phase.
- **Legacy manifests** without the structured compatibility fields (or
  with the pre-structured string `cpuModel`) are admitted with a warning.
  The warning is sticky for a checkpoint chain: a checkpoint inherits its
  compatibility block from its source, so a legacy ancestor keeps the
  chain on the legacy path until the image is rebuilt. An
  agent-delivered artifact set without *any* committed manifest is not
  treated as legacy — the pull commits the manifest last, so a missing
  manifest during a create fails with `ErrImageNotReady` and retries
  until the commit point lands (the legacy fallback applies only to
  local-mode, hand-seeded caches).

### Authoritative basis for the allowlists

1. The static-template support matrix, per release, in Firecracker
   source ([`static_cpu_templates/mod.rs`](https://github.com/firecracker-microvm/firecracker/blob/v1.16.1/src/vmm/src/cpu_config/x86_64/static_cpu_templates/mod.rs)):
   `T2/T2A` vendor and model rows match the table above; the official
   docs table (v1.16.1 `docs/cpu_templates/cpu-templates.md`) lists the
   same pairs — "T2 | Intel | Skylake, Cascade Lake, Ice Lake" and
   "T2A | AMD | Milan" — plus the note
   *"The only AMD template is T2A. It is considered safe to be used with
   AMD Milan."*
2. The comparison is **exact family/model/stepping equality**
   ([`arch/x86_64/cpu_model.rs`](https://github.com/firecracker-microvm/firecracker/blob/v1.16.1/src/vmm/src/arch/x86_64/cpu_model.rs):
   `CpuModel` derives `Eq` over all FMS fields, MILAN_FMS = EAX
   `0x00a00f11`). A Milan with stepping ≠ 1 fails the check the same way
   a Turin does.
3. Vendor crossing is rejected upstream before the model check:
   *"Representing one CPU vendor as another CPU vendor is not
   supported"* (`cpu-templates.md` note); the runtime error is
   `CpuVendorMismatched`. This is why the builder pins per vendor
   instead of always pinning T2.
4. **Empirical confirmation** (EPYC 9T95 / Turin, family 26 model 17
   stepping 0): `PUT /machine-config` with `cpu_template=T2A` is
   accepted, `InstanceStart` returns 400 *"The current CPU model is not
   permitted to apply the CPU template"*. Upstream has no Turin/Genoa
   static template in v1.16.1 or v1.17.0 (latest at the time of
   writing): `cpu_model.rs` defines no FMS for them.

## Builder behavior per host vendor

| Host | Pinned template | Manifest tier |
|---|---|---|
| GenuineIntel | `T2` | `T2` on allowlisted models; `none` when refused (SPR/EMR, non-allowlisted stepping) |
| AuthenticAMD | `T2A` | `T2A` on Milan stepping 1; `none` otherwise (Genoa, Turin) |
| unknown | none | `none` |

T2A only applies to EPYC Milan — it is the only AMD static template
upstream. Turin/Genoa golden images are therefore always tier `none`
today; supporting them portably needs an upstream static template or a
custom template (OSEP-0024 later phase).

## Restore admission

`internal/artifacts.MatchRestoreCompatibility` is the tiered matcher;
`validateRestoreCompatibility` (firecracker driver) runs it against the
cached manifest before any snapshot file is staged. A rejection is
`ErrIncompatibleArtifact` with the failing dimension in the chain, maps
to the `ProfileMismatch` fastlet error (deterministic, not retryable on
the same node), and the fastpath orchestrator advances to the next
top-k candidate.

## Node labels

The `firecracker-runtime` DaemonSet agent stamps scheduling labels on
every healthy node (removed while the node is degraded):

```
sandbox.fast.io/cpu-template:  "T2" | "T2A" | "none"
sandbox.fast.io/cpu-identity:  "AuthenticAMD-26-17"   # "none" nodes only
```

`T2`/`T2A` mean "restores template-masked snapshots of this tier" — the
allowlist decides what fits, so no identity label is needed. `none`
means "restores only identity-matched unmasked snapshots", and the
`cpu-identity` label (`vendor-family-model`, matching the admission's
none-tier key) says which ones; nodes whose CPU is unreadable get the
tier but no identity label.

Scheduling examples:

```yaml
# Keep golden-image builds on the portable tier.
nodeSelector:
  sandbox.fast.io/cpu-template: "T2"

# A SandboxPool whose template image is tier T2A.
nodeSelector:
  sandbox.fast.io/cpu-template: "T2A"

# A none-tier image (built on EPYC 9T95) stays on its identity pool.
nodeSelector:
  sandbox.fast.io/cpu-template: "none"
  sandbox.fast.io/cpu-identity: "AuthenticAMD-26-17"
```

Mixed pools work without labels: an incompatible candidate is rejected
at admission and the orchestrator tries the next node.

## Known limits and follow-ups

- **Static templates are deprecated upstream** since Firecracker v1.5.0
  (removal per the deprecation policy; custom templates via
  `cpu-template-helper` are the successor). Migrating the builder to
  custom templates is the long-term shape; the tier labels and admission
  stay, only the allowlist rows change.
- No upstream static template exists for EPYC Genoa/Turin (verified
  through v1.17.0) — those hosts remain tier `none`.
- T2 cross-model restore (e.g. built on Cascade Lake, restored on Ice
  Lake) is officially supported; validate it once per fleet before
  relying on it in production.
- Placement-time filtering (the fastpath top-k hard filter on this
  label) is a follow-up; today the labels are the operator lever and the
  orchestrator retry covers the rest.

## References

- OSEP-0024 (design and later phases):
  [opensandbox-group/OpenSandbox#1990](https://github.com/opensandbox-group/OpenSandbox/pull/1990)
- Tracking issue:
  [opensandbox-group/fast-sandbox#82](https://github.com/opensandbox-group/fast-sandbox/issues/82)
- Implementation: `internal/artifacts/manifest.go` (identity, allowlists,
  `MatchRestoreCompatibility`), `internal/runtime/firecracker/restore.go`
  (admission), `internal/runtime/firecracker/agent/hostready` (labels)
- Manifest schema: [artifact-manifest-reference.md](artifact-manifest-reference.md)
