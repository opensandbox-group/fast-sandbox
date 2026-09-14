# SandboxTemplate: Declarative Golden-Image Builds for Firecracker

> 文档类型：设计提案
>
> 日期：2026-08-24
>
> 说明：本提案的权威版本位于 OpenSandbox 仓库的 OSEP 流程；此副本供 fast-sandbox
> 消费侧（driver / runtime-agent）与转换侧（osb template CLI）对齐使用。
>
> **实现状态（2026-08-25）**：本文件的 proposal/schema 段落是设计阶段的参考，
> 与实际实现存在已知差异，以代码为准：
>
> - API 版本：实现为 `sandbox.fast.io/v1alpha2`（非 v1alpha1）
> - 构建执行：实现为 controller 驱动 **Pod**（非 Job），独立于 CLI
> - 格式：实现为 `native` / `overlaybd`（两者都产出完整快照集；设计的
>   `ext4` / `snapshot` 细分已收敛为 `native`）
> - `output.publish` 在实现中可选（默认取平台 artifact-store ConfigMap），
>   `publishSecretRef` 由 controller 以 volume 挂载进 build Pod（builder 读文件）
>
> **使用文档**：字段含义与操作流程以
> [guides/sandboxtemplate-golden-images.md](../guides/sandboxtemplate-golden-images.md)
> 与 [reference/api.md](../reference/api.md) 为准。

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Firecracker does not consume OCI images directly. Today the conversion of an
OCI image into a Firecracker rootfs, the injection of the OpenSandbox runtime
(execd), and — for snapshot and lazy-loading scenarios — the packaging into
OverlayBD layers, are ad-hoc, hand-written pipeline steps with no shared
contract.

This OSEP introduces **SandboxTemplate**, a declarative
`sandbox.opensandbox.io/v1alpha1` resource that describes a golden-image build:
source OCI image, execd injection, guest init selection, readiness semantics,
and output artifacts. A single schema is consumed in two phases:

1. **Phase 1**: an `osb image` subcommand in the OpenSandbox CLI executes the
   template locally (converting, snapshotting, packaging, publishing).
2. **Phase 2**: a Kubernetes controller reconciles `SandboxTemplate` objects
   by driving the same build as a Kubernetes Job, so golden images are built,
   versioned, and published by the cluster.

## Motivation

Sandboxes backed by Firecracker need a bootable rootfs. The OpenSandbox
runtime stack also requires execd and bootstrap files inside the guest, and
fast startup paths want full snapshots or OverlayBD lazy-loading layers.

None of that has a first-class description today:

- The conversion input (which OCI image), the injected runtime bits, the
  guest PID 1, and the readiness definition are scattered across build
  scripts and CI parameters.
- There is no shared, versionable artifact contract: consumers cannot tell
  from a build output which source image, kernel, execd, or init it embeds,
  nor how it was validated.
- Building images inside CI is not reproducible or reviewable the way a
  declarative resource is; there is no way for a cluster to request a
  rebuild when a template changes.

A declarative template fixes all three: the **template** is the contract a
user writes, the **manifest** is the contract a runtime consumes, and the
**executor** (CLI or controller) is interchangeable.

### Goals

- Define a `SandboxTemplate` CRD (spec + status) covering the full golden
  image build: source, injection, init, readiness, output.
- Define a content-addressed artifact **manifest** (digest, machine, files,
  validation record) produced by every build.
- Phase 1: `osb image` CLI subcommands that consume the same schema locally
  — build, template init, validation, publish.
- Phase 2: a controller that reconciles `SandboxTemplate` into Kubernetes
  Jobs, records build results in `status`, and publishes artifacts.
- Keep the two executors behaviorally identical through a shared build
  engine and shared schema validation.

### Non-Goals

- Not a general-purpose OCI-to-VM converter; scope is Firecracker golden
  images for OpenSandbox.
- No runtime behavior change in the drivers that consume the artifacts; this
  OSEP only changes how the artifacts are produced (the existing
  content-addressed rootfs cache contract in the fast-sandbox Firecracker
  driver is the Phase 1 consumer, see [Runtime consumption](#runtime-consumption)).
- Sandbox API surface is unchanged: the per-sandbox init override is carried
  by the existing `CreateSandboxRequest.entrypoint` (argv list) — in free-init
  mode the request entrypoint becomes the guest PID 1, in managed mode it
  replaces the business command executed under the injected guest init.
- Not covering pause/resume or snapshot restore orchestration at runtime.
- Phase 2 does not include multi-tenancy, quotas, or scheduling policy for
  build Jobs beyond basic resource requests.

## Requirements

- The schema must be a Kubernetes-native CRD (`sandbox.opensandbox.io/v1alpha1`)
  following the existing conventions in `kubernetes/apis/sandbox/v1alpha1`.
- The same YAML document must be consumable by the CLI (Phase 1) and by the
  controller (Phase 2) with identical validation.
- Artifacts must be content-addressed (digest over the produced files and
  manifest) so consumers can verify integrity and share cached blobs.
- The build must support three artifact tiers with strict inclusion:
  `ext4` (converted rootfs only) → `snapshot` (+ full snapshot) →
  `overlaybd` (+ LSMT layers).
- The guest init must be selectable per template (injected PID 1, or the
  image's own init) and overridable per sandbox at runtime. The override is
  carried by the existing `CreateSandboxRequest.entrypoint` field — no new
  sandbox API surface.
- Readiness must be defined per template: custom probe first, execd `/ping`
  by default, time-based warmup plus image healthcheck as fallback.
- Every produced format must pass a boot validation gate; for `ext4` this is
  a boot-only validation (start, reach readiness, stop) without retaining
  snapshot artifacts.
- The manifest must record the snapshot compatibility tuple (kernel digest,
  host kernel, CPU model, Firecracker version) so consumers can select a
  compatible restore node.

## Proposal

Introduce a `SandboxTemplate` custom resource:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: ai-office-sandbox
spec:
  image: registry.example.com/sandbox:v1.0.21
  entrypoint:                              # empty: defaults to ["tail","-f","/dev/null"]
    - /opt/gem/run.sh
  execd: registry.example.com/execd:v1.0.21
  kernel: vmlinux-6.1.177
  machine:
    vcpu: "4"                        # Kubernetes resource quantity
    memory: "8Gi"                    # e.g. 512Mi / 2Gi / 8Gi
  init: /usr/local/sbin/guest-init    # empty: no injection
  envs:                             # written verbatim into /etc/sandbox-init.env (literal values only; not merged with the image Config.Env, valueFrom unsupported)
    - name: FOO
      value: bar
  readiness:
    probe: tcp://127.0.0.1:44772             # custom first
    warmupSeconds: 60                        # fallback baseline
    healthCheck: ""                           # empty: image CMD-SHELL
  output:
    rootfsSize: "30Gi"
    format: overlaybd                        # ext4 | snapshot | overlaybd
    publish: s3://bucket/sandbox-images/     # optional, digest-addressed
    prime:                                 # optional seed-node prime
      nodeSelector:                          # best-effort, never blocks the build
        node-role.open-sandbox.io/agent: "true"
```

The build pipeline has three stages, selected by `output.format`:

1. **convert** — materialize the OCI layers directly into a sparse ext4
   image (e.g. with the `oci2rootfs` tool), repair with `e2fsck`, then
   loop-mount briefly to inject execd/bootstrap/prepare/bwrap, the optional
   guest init (at the exact path the spec declares), and
   `/etc/sandbox-init.env` (spec envs and the entrypoint; the OCI image's
   own `Config.Env` is not merged).
2. **validate-boot** — boot the rootfs on a KVM host with the embedded
   kernel and wait for guest readiness. For `ext4` this is the terminal
   validation gate (start, reach readiness, stop; no snapshot artifacts are
   retained). For `snapshot`/`overlaybd` it continues into the snapshot
   stage.
3. **snapshot** — pause the validated guest and create a full snapshot
   (`vmstate.snap` + `memory.snap`); restore once for validation.
4. **package** — convert `rootfs.ext4` and `memory.snap` into independent
   OverlayBD LSMT commit layers (windowed import, zero-run elision, seal,
   byte-for-byte verification).

Every build emits a content-addressed `manifest.json`:

```json
{
  "schemaVersion": 1,
  "sourceImage": "registry.example.com/sandbox:v1.0.21",
  "sourceImageDigest": "sha256:...",
  "execd": "registry.example.com/execd:v1.0.21",
  "kernel": {
    "name": "vmlinux-6.1.177",
    "digest": "sha256:..."
  },
  "machine": { "vcpu": "4", "memory": "8Gi" },          // parsed into vcpuCount/memoryMiB by the engine
  "compatibility": {
    "firecrackerVersion": "v1.16.1",
    "architecture": "x86_64",
    "cpuModel": "Intel(R) Xeon(R) Platinum 8163 CPU @ 2.50GHz",
    "hostKernel": "5.10.134-18.al8.x86_64"
  },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "init": "/usr/local/sbin/guest-init",            // empty: no injection
  "envs": [{"name": "FOO", "value": "bar"}],            // spec envs, written verbatim (no OCI Config.Env merge)
  "files": { "rootfs": {"sha256": "...", "sizeBytes": 32212254720}, ... },
  "validation": { "booted": true, "restored": true, "iterations": 3, "timing": {...} }
}
```

### Notes/Constraints/Caveats

- **Runtime consumer**: the Phase 1 consumer of the `ext4` artifact is the
  Firecracker runtime driver in the fast-sandbox project, which already
  maintains the content-addressed cache
  (`<StateRoot>/images/<sha256(imageRef)>/rootfs.img`) and pulls artifacts
  by digest. The template's `publish` target becomes the pull source; the
  artifact contract (digest + manifest) is defined here and consumed there.
- **Seed prime is best-effort**: `output.prime` optionally selects seed
  nodes (by label selector) whose agent warms the local cache after a
  successful build, so P2P spread and cold-start storms start from warm
  seeds. Prime never blocks or fails the build; unreachable nodes are
  skipped and the object store remains the authoritative source.
- **execd injection boundary**: only the execd binary and fixed bootstrap
  skeleton are build-time injected. Per-sandbox configuration (infra.json,
  identity, component env) remains runtime-generated, written into the
  instance layer — the build cannot know the sandbox identity.
- **init modes**: injected guest init (managed mode, execd started by PID 1),
  or no injection (image's own init; execd startup is then the image's
  responsibility). A per-sandbox override takes precedence over the template
  value and is carried by `CreateSandboxRequest.entrypoint`: in free-init
  mode the request entrypoint becomes the guest PID 1 (`init=` argv); in
  managed mode it replaces the business command executed under the injected
  guest init, which stays the PID 1.
- **entrypoint semantics**: `spec.entrypoint` is an argv list with a fixed
  default of `["tail", "-f", "/dev/null"]` when empty — the sandbox stays
  alive as an environment and work is driven through execd or the SDK. An
  explicit value fully replaces the default with intact argument boundaries.
- **Readiness precedence**: custom `probe` (`tcp://` or `cmd://`) → execd
  `/ping` (default) → `warmupSeconds` + `healthCheck` (fallback, e.g. when
  execd is not injected; empty healthcheck uses the image `CMD-SHELL`).
- **Format tiers are inclusive**: `overlaybd` implies `snapshot` implies
  `ext4`; a single value selects the pipeline depth.
- **Guest network**: the snapshot stage bakes a NIC (iface `eth0` with a
  static guest IP/MAC, recorded in the manifest as `guestNetwork`) so a
  restored instance owns its address; consumers replace only the host tap
  via `network_overrides`. The stage deliberately does not configure
  external connectivity, so no **active connections** are captured in the
  snapshot — snapshotting workloads with live connections is out of scope.
- **Source image compatibility**: the converter applies layer ordering,
  compression, hard links, and whiteouts, but may skip device nodes and
  timestamps/xattrs; the boot (and restore, for snapshot formats)
  validation is a mandatory compatibility gate for the selected source
  image.
- **Privileged-build entry point**: a SandboxTemplate is effectively
  "run an arbitrary OCI image as root on a KVM host" — creating templates
  must be restricted to trusted operators (cluster-admin or a dedicated
  Role). The build Pods run in the **template's namespace** under the
  `sandbox-template-builder` ServiceAccount, which the controller provisions
  per namespace with only `pods/patch` (the builder self-reports its
  outcome) and **converges on every reconcile**: a same-named Role or
  RoleBinding with broader content (e.g. pre-created by a tenant) is
  rewritten back onto the enforced shape rather than trusted, so the
  boundary holds by construction, not by assumption; the controller RBAC (`sandboxtemplates` create/update) and the
  builder SA are deliberately scoped, and the controller never reads
  secrets — publish credentials reach the build Pod as a mounted Secret
  volume (secret lives in the template's namespace, next to the Pod). Because the builder is privileged (host
  `/dev/kvm` passthrough), the tenant namespace must allow privileged Pods
  (PodSecurityAdmission); this moves the privilege boundary from "control
  plane only" to "template namespace", in exchange for native cascading
  deletion: the build Pod carries a controller owner reference to the
  template, so deleting a template garbage-collects its build Pods without
  a finalizer.
- **Publish credentials**: `output.publish` is optional and defaults to the
  platform artifact store; when `output.publishSecretRef` names an
  imagePullSecrets-style secret
  (`accessKeyId`/`secretAccessKey`/`endpoint`/`region`), the controller
  mounts the secret into the build Pod and the builder reads the keys as
  files. The secret MUST live in the template's namespace — a Pod can only
  mount Secrets from its own namespace; the platform operator manages it out of
  band. Without the secret, the build relies on platform-level credentials
  (e.g. IRSA / node metadata), which must be present on KVM nodes.
- **Build lifecycle safety**: build Pods have `activeDeadlineSeconds`
  (2 h), deterministic names (`<template>-build-<generation>`, so
  concurrent reconciles dedupe), an owner reference to the template (GC
  cascades deletion), and a Pending timeout (10 min) that fails builds which
  never get scheduled (e.g. no node labeled `sandbox.fast.io/kvm=true`).
  Finished Pods are retained for `BuildTTL` (24 h) so their annotations stay
  inspectable, then reaped by the controller. KVM/tun devices are passed
  through as hostPath CharDevice mounts, and the Pod is pinned to KVM nodes
  via nodeSelector.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Converter fidelity (whiteout leftovers, missing device nodes) | `e2fsck -fy` repair + read-only re-check + mandatory boot validation per build (restore validation for snapshot formats) |
| Snapshot captures an unready or half-initialized guest | Readiness gate before snapshot; warmup baseline; application-specific readiness preferred |
| Snapshot portability (kernel/CPU features) | Manifest records the compatibility tuple (kernel digest, host kernel, CPU model, Firecracker version); consumers match it against node labels before restore |
| Artifact tampering or corruption in transit | Content-addressed manifest + `SHA256SUMS`; consumers verify digests |
| Two executors drift (CLI vs controller) | Shared build engine library + shared schema validation; both consume the same template |
| Build resource spikes (30 GiB rootfs, snapshot memory) | Phased disk usage (archive → layout → rootfs only), page-cache dropping during import, configurable machine size |

## Design Details

### CRD schema

`SandboxTemplate` lives in `kubernetes/apis/sandbox/v1alpha1`
(`sandboxtemplate_types.go`), same group and kubebuilder conventions as
`BatchSandbox`.

```go
type SandboxTemplateSpec struct {
    Image       string            `json:"image"`                 // required
    // Argv list; empty defaults to ["tail","-f","/dev/null"].
    Entrypoint  []string          `json:"entrypoint,omitempty"`
    Execd       string            `json:"execd,omitempty"`
    Kernel      string            `json:"kernel"`                // required
    // Init is the injected PID 1 path inside the rootfs; empty means
    // no injection (the kernel default or the image's own init applies).
    // When set, the init script is injected at exactly this path and the
    // kernel boots with init=<this path>.
    Init        string            `json:"init,omitempty"`
    // Envs is injected as /etc/sandbox-init.env. Literal values only —
    // valueFrom is unsupported, and the OCI image's Config.Env is not
    // merged.
    Envs []corev1.EnvVar     `json:"envs,omitempty"`
    Machine     MachineSpec       `json:"machine"`
    Readiness   ReadinessSpec     `json:"readiness"`
    Output      OutputSpec        `json:"output"`
}

type MachineSpec struct {
    // Kubernetes resource quantity (e.g. "1", "4000m").
    // +kubebuilder:default="1"
    VCPU string `json:"vcpu"`
    // Kubernetes resource quantity (e.g. "512Mi", "2Gi", "8Gi").
    // +kubebuilder:default="2Gi"
    Memory string `json:"memory"`
}

type ReadinessSpec struct {
    // tcp://host:port or cmd://<command>; empty falls back to execd /ping.
    Probe         string `json:"probe,omitempty"`
    // +kubebuilder:default=60
    WarmupSeconds int32  `json:"warmupSeconds"`
    // Empty uses the source image CMD-SHELL healthcheck.
    HealthCheck   string `json:"healthCheck,omitempty"`
}

// +kubebuilder:validation:Enum=ext4;snapshot;overlaybd
// +kubebuilder:default=ext4
type ArtifactFormat string

type OutputSpec struct {
    // Kubernetes resource quantity (e.g. "10Gi", "30Gi").
    // +kubebuilder:default="30Gi"
    RootfsSize string          `json:"rootfsSize"`
    Format        ArtifactFormat `json:"format"`
    Publish       string         `json:"publish,omitempty"`
    // Prime optionally selects seed nodes (by label selector) whose agent
    // warms the local cache after a successful build. Always best-effort:
    // it never blocks or fails the build, and the object store stays the
    // authoritative source.
    // +optional
    Prime *PrimeSpec `json:"prime,omitempty"`
}

type PrimeSpec struct {
    // +kubebuilder:validation:MinProperties=1
    NodeSelector map[string]string `json:"nodeSelector"`
}

type SandboxTemplateStatus struct {
    // +kubebuilder:validation:Enum=Pending;Building;Succeeded;Failed
    Phase             SandboxTemplatePhase  `json:"phase,omitempty"`
    Conditions        []SandboxTemplateCondition `json:"conditions,omitempty"`
    ArtifactDigest    string                `json:"artifactDigest,omitempty"`
    ManifestRef       string                `json:"manifestRef,omitempty"`
    LastBuildTime     *metav1.Time          `json:"lastBuildTime,omitempty"`
    ObservedGeneration int64                `json:"observedGeneration,omitempty"`
}
```

### Build engine

The stages (convert / validate-boot / snapshot / package) are implemented as
a shared library (`components/internal` or a dedicated package) invoked by
both executors. External tools are dependencies, not embedded logic:
`oci2rootfs` (or an equivalent layer-applier), `firecracker`, `e2fsprogs`,
and the OverlayBD toolchain. The engine emits the manifest and `SHA256SUMS`.
Every format runs the validate-boot gate; only `snapshot`/`overlaybd`
continue into snapshot and package.

### Phase 1 — `osb image` CLI

```
osb image template init -n <name>            # generate a template skeleton
osb image template validate -f template.yaml # schema + reference checks
osb image build -f template.yaml             # run the full build locally
osb image build -f template.yaml --set image=...:v1.0.22   # overrides
```

The CLI shares the schema validation and the engine with the controller.
Local execution requires a KVM-capable host for `snapshot`/`overlaybd`
formats.

### Phase 2 — controller

A controller watches `SandboxTemplate`:

```
reconcile(template):
  validate spec
  → create a Kubernetes Job (builder image, spec → env/args, format
    decides pipeline depth; /dev/kvm device for snapshot/overlaybd)
  → wait for completion → read manifest (digest verified)
  → publish to spec.output.publish (digest-addressed)
  → if output.prime set: notify the agent pods on matching seed nodes
    (best-effort; failures are logged, never failing the build) so they
    warm their local caches for P2P spread
  → update status {phase, artifactDigest, manifestRef, lastBuildTime,
    observedGeneration}
```

Generation tracking drives rebuilds on template changes; failed builds set
`phase: Failed` with conditions.

### Runtime consumption

The produced artifacts are consumed by the Firecracker runtime driver:

- `ext4`: the existing content-addressed cache
  (`<StateRoot>/images/<sha256(imageRef)>/rootfs.img`) — the template's
  `publish` target becomes the source for on-demand pull with digest
  verification.
- `snapshot`: restore path for fast startup.
- `overlaybd`: root drive / memory backend backed by LSMT layers (runtime
  support lands separately).

The per-sandbox `init` override resolves as:
`CreateSandboxRequest.entrypoint > template manifest.init > kernel default`.

## Test Plan

- **Unit**: schema validation (field combos, format tiers, readiness
  precedence), manifest digest computation, engine stage wiring.
- **Integration (CLI)**: build each format tier against a small fixture OCI
  image; assert manifest fields, file digests, and `e2fsck` clean state.
- **E2E**: boot the produced `ext4` rootfs in a Firecracker microVM and run
  the execd smoke path; restore a produced snapshot and assert guest
  readiness; byte-for-byte verify OverlayBD layers against sources.
- **Controller (Phase 2, Kind)**: apply a `SandboxTemplate`, assert the Job
  is created, status transitions Pending → Building → Succeeded, manifest
  digest recorded, and a spec change triggers a rebuild.
- **Negative**: converter-unsupported images fail the boot/restore
  validation gate; digest mismatch fails publication; unreadable template
  fails validation before any Job is created.

## Drawbacks

- A new CRD and a new build pipeline are a real investment; for teams that
  only need plain rootfs images the `ext4` tier plus CLI may be sufficient
  and the controller is extra machinery.
- Template-driven builds are only as good as their validation: a template
  whose readiness gate is misconfigured can produce "healthy" snapshots that
  are not actually ready. The readiness semantics mitigate but do not
  eliminate this.
- Golden-image builds shift flexibility from runtime to build time:
  per-sandbox variance is constrained to what the template declares
  (init override, envs, entrypoint).

## Alternatives

- **Keep ad-hoc CI scripts**: no shared contract, no reviewability, no
  cluster-driven rebuilds — rejected as the status quo this OSEP replaces.
- **CLI only, no CRD**: simpler, but the schema would be a plain YAML
  convention with no cluster representation; the controller phase would
  require inventing the schema later. CRD-first keeps one contract.
- **CRD only, no CLI**: complicates local iteration and debugging; a
  template build should be runnable on a workstation before being driven by
  the cluster.
- **Single monolithic binary performing conversion**: couples the build to
  embedded logic; delegating to focused external tools (layer applier,
  OverlayBD toolchain) keeps each piece maintainable and verifiable.
- **General OCI-to-VM converter**: out of scope; the template is
  Firecracker/OpenSandbox-specific.

## Infrastructure Needed

- A KVM-capable build environment for `snapshot`/`overlaybd` formats
  (CI runners or dedicated build nodes with `/dev/kvm`).
- A digest-addressed object store for artifact publication
  (`spec.output.publish`), e.g. S3/OSS-compatible storage.
- The upstream toolchain pinned by digest: layer applier, Firecracker
  release, embedded guest kernel, OverlayBD import tool.

## Upgrade & Migration Strategy

- Additive: the new CRD and CLI subcommands do not change existing sandbox
  or runtime behavior.
- Existing ad-hoc builders can migrate incrementally by expressing their
  steps as a `SandboxTemplate` and switching to `osb image build` (Phase 1)
  before adopting the controller (Phase 2).
- Runtime consumption of the new artifacts is optional per format: `ext4`
  artifacts remain the default until snapshot/overlaybd runtime support
  lands.
