---
title: "Bounded Runtime Cgroup Hierarchy with Explicit Per-Sandbox Host Overhead"
authors:
  - "高然"
creation-date: 2026-11-01
last-updated: 2026-11-01
status: draft
---

# FSEP-0001: Bounded Runtime Cgroup Hierarchy with Explicit Per-Sandbox Host Overhead

## Problem and goals

ResourceAware pools compile three vectors: `commitmentCapacity`, `sandboxRuntimeBudget`, and `runtimeEnvelope = sandboxRuntimeBudget + fastletOverhead`. The Controller applies `runtimeEnvelope` as the runtime-owner container request and limit, but nothing at runtime enforces `sandboxRuntimeBudget` as an aggregate cgroup boundary below that envelope. Two consequences, described in [#156](https://github.com/opensandbox-group/fast-sandbox/issues/156):

- Direct Firecracker: the Fastlet launches VMMs with `exec.Command`, so every VMM inherits the Fastlet container cgroup. Aggregate Sandbox memory pressure can reach the container `memory.max`, and the kernel OOM victim set then contains the Fastlet itself. Killing the Fastlet turns a local resource-pressure event into a control-plane failure for every Sandbox on that Fastlet.
- containerd/Kata: per-Sandbox OCI cgroups already sit below a shared `fast-sandbox` parent, but that parent carries no compiled aggregate limit, so the design statement that fixed Fastlet overhead cannot be consumed by Sandboxes is not enforced.

The issue also requires one contract be decided before any host limit is applied: today the public Sandbox memory limit maps one-to-one to guest-visible RAM (`resolveMachineConfig`), while host-side VMM consumption is guest RAM plus VMM metadata, stage-2/page tables, device emulation, and process overhead. Copying the same number into a per-Sandbox host leaf `memory.max` makes the leaf structurally too small and cgroup-local OOM kills healthy Sandboxes.

Goals: enforce the invariant that Sandbox overcommit may throttle, reclaim, or OOM inside the Sandbox workload subtree but must not consume the Fastlet headroom or select the Fastlet as an OOM victim; preserve overcommit (`sum(per-Sandbox host leaves) > sandboxRuntimeBudget`); and keep the public resource semantics unchanged. Non-goals: node-level global OOM handling, cgroup v1 support, LegacySlot mode, and changes to dynamic admission, which stays an early-warning and load-shedding mechanism rather than a correctness boundary.

## Proposal and tradeoffs

### Memory-limit contract: the public limit stays guest RAM; VMM host overhead is explicit and platform-owned

The declared `limits.memory` of a Sandbox keeps its current meaning: guest-visible RAM. Host-side consumption is bounded by an explicit, per-runtime-profile overhead instead:

```
per-Sandbox host leaf   memory.max = limits.memory + sandboxHostOverhead
sandboxRuntimeBudget               = commitmentCapacity + maxSandboxes * sandboxHostOverhead
runtimeEnvelope                    = sandboxRuntimeBudget + fastletOverhead     (unchanged formula)

sandboxHostOverhead (direct Firecracker) = 8MiB + ceil(limits.memory / 512)
```

For direct Firecracker the overhead is a formula — an 8MiB process baseline plus the worst-case stage-2 page-table term (`RAM/512` at 4K granule) — rather than a flat constant, because the page-table term is the only component that scales with guest size. Every Sandbox in a ResourceAware pool shares the same profile limit, so the Controller resolves the formula into one deterministic byte value per pool; the Fastlet consumes that projected value and never evaluates a formula itself. For high-density pools this is materially cheaper than a flat constant sized for the largest guest: a 512MiB-limit pool pays 9MiB per Sandbox, 1GiB pays 10MiB, 2GiB pays 12MiB, and an 8GiB pool still stays safe at 24MiB even with transparent huge pages fully disabled.

`sandboxHostOverhead` is a new `RuntimeDefinition` field owned by the runtime catalog, carrying a memory quantity only in the first iteration: the direct Firecracker profile resolves the formula above, Kata profiles carry a fixed value set from measurement, and runc and gVisor carry zero. Host CPU consumption of a VMM is dominated by guest vCPU execution, which the Sandbox CPU limit already bounds, so a CPU overhead term is deferred until measurement shows the VMM's own threads matter; the same field mechanism supports adding one later without a contract change. Because the leaf always exceeds guest RAM plus VMM overhead, a Sandbox that fully uses its declared memory never trips its own leaf: the sharing boundary is deliberately the aggregate `fast-sandbox` parent, and leaves exist for blast-radius isolation between Sandboxes.

For the direct Firecracker runtime the overhead covers every host charge a Sandbox leaf accumulates beyond guest RAM: the Firecracker process itself (Rust runtime, stacks, heap; the Firecracker paper reports a VMM memory overhead below 5 MiB per microVM [[1]](https://www.usenix.org/conference/nsdi20/presentation/agache), so the 8MiB baseline term carries margin over the published figure), KVM kernel metadata charged to the process (vCPU and memslot structures, plus stage-2 page tables that grow with touched guest memory — worst case roughly `RAM/512` at 4K granule and far less when transparent huge pages back the guest memory, which is the `limit/512` term), and virtio device emulation plus vhost threads (sub-MiB, absorbed by the baseline margin). Reclaimable page cache from snapshot reads may transiently reach the size of `memory.snap` inside the leaf but is reclaimed first under `memory.max` pressure and therefore earns no permanent headroom. The overhead does not cover guest RAM (the declared limit), the Fastlet process (the existing `fastletOverhead`), or the runtime-agent DaemonSet and sidecars, all of which stay outside the hierarchy. The Kata value covers the fatter QEMU/cloud-hypervisor process baseline and is set from measurement.

This was chosen over deriving guest RAM inside the declared budget (a ratio such as 95%) after weighing where the unavoidable per-VM overhead tax is charged. Both designs carry the same constant; the alternative charges it to the tenant as reduced guest RAM, changes the public meaning of `limits.memory`, and requires every existing template snapshot to be rebuilt because baked `mem_size_mib` equals the full declared value. The chosen contract keeps user semantics and all existing snapshots valid — restore admission is already a `>=` check against the baked machine tuple — and charges the tax to pool capacity, which the platform already owns through `fastletOverhead` for the Fastlet process. The accepted cost is that control-plane compilation, settings validation, and leaf writes must consume the new constant consistently, and that aggregate budget numbers increase by `maxSandboxes * sandboxHostOverhead`.

### Runtime cgroup hierarchy (cgroup v2 only)

For runtimes whose processes the Fastlet launches, the manager builds the hierarchy from [#156](https://github.com/opensandbox-group/fast-sandbox/issues/156) before the Fastlet reports Ready:

```
runtime-owner container cgroup        (cpu.max / memory.max = runtimeEnvelope, kubelet-owned)
├── fastlet-self                      (Fastlet process only; no hard limit of its own)
└── fast-sandbox                      (cpu.max / memory.max = sandboxRuntimeBudget)
    ├── fsb-<hash-a>                  (per-Sandbox leaf: memory.max = limit + sandboxHostOverhead)
    ├── fsb-<hash-b>
    └── fsb-<hash-c>
```

On cgroup v2 the manager creates `fastlet-self` first, moves the container's processes into it, and only then enables `+cpu +memory +pids` in `cgroup.subtree_control` (no-internal-process rule). Per-Sandbox leaf limits may sum above the parent budget; overcommit continues to work. The manager reconciles membership after Fastlet restart (hook: `RecoverRuntimeResources`) and removes empty leaves on Sandbox deletion. Direct Firecracker VMMs are launched into their leaf, preferably via `clone3(CLONE_INTO_CGROUP)` (kernel 5.7+), with a post-exec PID move as the documented-race fallback; the choice is an implementation detail validated by the test plan. The containerd/Kata path keeps its existing OCI `cgroupsPath` behavior and additionally receives the compiled `sandboxRuntimeBudget` written on the `fast-sandbox` parent at driver initialization; Kata hypervisor processes land in the Sandbox OCI cgroup, so the same per-Sandbox overhead applies through the leaf the shim creates.

Scope gates: only ResourceAware pools; cgroup v1 hosts report the runtime capability as unavailable (mirroring the aggregate memory observer, which already requires v2); LegacySlot behavior is unchanged; the node runtime-agent DaemonSet resources stay independent of every pool ledger and hierarchy.

### Data flow of the resolved overhead

The runtime catalog gains `sandboxHostOverhead` on `RuntimeDefinition` (profile hash changes, so the builtin profile version bumps and each pool rolls its Fastlet Pods as its runtime plan revision changes). The Controller resolves the direct Firecracker formula into a single per-pool value, consumes it in `compileFastletResources` when deriving `sandboxRuntimeBudget`, and projects it into the Fastlet settings ConfigMap as a new `ResourceAdmissionSettings` field so the Fastlet can compute leaf limits without hardcoding. `ResourceAdmissionSettings.Validate` extends its containment inequalities from "budget contains one Sandbox limit" to "budget contains one Sandbox limit plus its host overhead".

Main risks: the deployment assumes privileged Fastlet containers (all builtin profiles are privileged today) and tolerates systemd-managed hosts, where foreign subgroups below the kubelet-owned pod cgroup must persist (precedent: the existing `fast-sandbox-shims` group). Enabling subtree_control and moving the container init process must not disturb kubelet liveness or cadvisor aggregation; both are verified in E2E. An undersized overhead reintroduces leaf pressure; the formula tracks the 4K page-table worst case so the remaining risk is confined to the 8MiB process baseline, which the published below-5MiB figure and the THP-disabled stress test cover. An oversized value only wastes budgeted capacity.

## Compatibility

- **Public semantics**: unchanged. `limits.memory` still means guest RAM; no CRD, SDK, CLI, or template change is required, and existing snapshots restore unchanged.
- **Platform-internal changes**: the runtime catalog field changes profile hashes and runtime plan revisions, which rolls Fastlet Pods per pool through the existing revision mechanism; the settings ConfigMap gains a field, which changes the settings revision and the pod template hash the same way. Older Fastlet images ignore the new settings field (JSON decoding is tolerant), so mixed Controller/Fastlet versions during rollout simply run without leaf enforcement until the Fastlet upgrades.
- **Capacity**: `sandboxRuntimeBudget` and therefore the runtime-owner container request/limit increase by `maxSandboxes * sandboxHostOverhead` (for a pool of 20 Sandboxes at a 1GiB limit, the formula yields 10MiB each and about `200Mi` more request per Fastlet Pod; a 2GiB pool pays 12MiB each). Nodes need correspondingly more allocatable capacity; pools that no longer fit schedule fewer Fastlet Pods, which is visible in Pool status before rollout completes.
- **Existing resources and snapshots**: no migration, no manifest marker, and no per-image contract versioning are needed; rollback leaves every template and running Sandbox valid.

## Test plan

- Unit: cgroup manager against a fake cgroupfs root (extending the `podcgroup` test pattern): creation order (`fastlet-self` before `subtree_control`), limit writes including the overhead term, idempotency, refusal to remove populated leaves, path containment below the pod cgroup, and cgroup v1 rejection.
- Unit: `compileFastletResources` derives `sandboxHostOverhead` from the profile formula (including the `ceil(limit/512)` rounding), `sandboxRuntimeBudget = commitmentCapacity + N * overhead`, and the envelope on top; `ResourceAdmissionSettings.Validate` accepts exactly one limit-plus-overhead and rejects a budget that cannot contain it; leaf computation consumes the projected settings field rather than a hardcoded constant.
- Integration/E2E (Linux, root, KVM; see the testing guide for host prerequisites): the acceptance criteria from [#156](https://github.com/opensandbox-group/fast-sandbox/issues/156) — VMM PIDs verified inside their leaves; aggregate CPU stress throttles the subtree without consuming the Fastlet CPU reserve; aggregate memory stress reclaims or OOMs inside the subtree while the Fastlet stays Ready; one Sandbox crossing its leaf limit cannot kill a sibling or the Fastlet; a Sandbox fully using its declared guest RAM does not trip its own leaf, repeated on a host with transparent huge pages disabled to exercise the 4K page-table worst case the `limit/512` term budgets for; Fastlet restart reconstructs membership; Sandbox deletion removes the empty leaf. Tests must distinguish cgroup-local OOM from node-global OOM.
- Performance: create-path latency delta measured on the already-instrumented `fastlet.firecracker.launch` span (leaf creation and process placement), expected sub-millisecond per create, reported from a warm cache run.

## Rollout and rollback

Staged per pool behind the ResourceAware mode plus a settings gate for the bounded hierarchy (initially off). The catalog and settings revisions roll Fastlet Pods per pool independently of the gate, which only controls whether the manager enforces boundaries. Monitor Fastlet OOMKills and restarts, Sandbox create failures mentioning cgroups, memcg OOM events inside the `fast-sandbox` subtree, subtree throttling, and per-leaf OOM events (which should stay at zero for healthy Sandboxes; a non-zero rate means the overhead is undersized and is a rollback trigger). Rollback disables the gate: new Fastlet Pods start without the hierarchy and VMMs return to the container cgroup; running Pods are left alone, since the budget never exceeds the envelope and the raised requests are merely conservative. Rolling back the Controller removes the overhead term from the budget as well, and because no public semantics, template, or snapshot ever changed, the rollback is fully reversible with no one-way door.

## Related links

- Issue [#156](https://github.com/opensandbox-group/fast-sandbox/issues/156) (problem statement and acceptance criteria), which references [#115](https://github.com/opensandbox-group/fast-sandbox/issues/115).
- [1] Agache et al., [Firecracker: Lightweight Virtualization for Serverless Applications](https://www.usenix.org/conference/nsdi20/presentation/agache), NSDI 2020 — the below-5MiB VMM memory overhead figure used to anchor the process term of the formula.
- Implementation PRs: to be linked. Docs to update during implementation: sandbox resource semantics in `docs/`, the architecture page, and the testing guide E2E matrix.

Open questions for review: the direct Firecracker formula parameters (8MiB process baseline anchored to the published below-5MiB figure, and the `limit/512` worst-case page-table ratio), the measured Kata fixed value, and the launch primitive (clone3 versus post-exec move) once the Linux spike in [#156](https://github.com/opensandbox-group/fast-sandbox/issues/156) reports back. A CPU overhead term is deliberately out of scope for the first iteration.
