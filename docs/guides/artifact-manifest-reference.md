# Artifact manifest reference

`manifest.json` is the metadata document of an artifact set published to the
artifact store. SandboxTemplate builds, SandboxSnapshots, and pause
checkpoints all share one schema; the SHA-256 of the whole document is the
set's `artifactDigest` — changing any byte produces a different artifact set.

## Complete example

```json
{
  "schemaVersion": 1,
  "runtime": "firecracker",
  "lineage": {
    "image": "registry.example.com/sandbox:v1.0.21",
    "imageDigest": "sha256:4d05…",
    "execd": "ghcr.io/opensandbox/execd@sha256:9f1c…",
    "kernel": { "name": "vmlinux-6.1.177", "digest": "3e8d…" },
    "entrypoint": ["/opt/gem/run.sh"],
    "init": "/usr/local/sbin/sandbox-init",
    "envs": [{ "name": "FOO", "value": "bar" }]
  },
  "machine": { "vcpu": "2", "memory": "2Gi", "rootfs": "30Gi" },
  "compatibility": {
    "vendor": "GenuineIntel",
    "cpuFamily": 6,
    "cpuModel": 85,
    "cpuModelName": "Intel(R) Xeon(R) Platinum 8163 CPU @ 2.50GHz",
    "cpuTemplate": "T2",
    "firecrackerVersion": "1.16.1",
    "hostKernel": "5.10.134-18.al8.x86_64"
  },
  "guestNetwork": {
    "iface": "eth0",
    "mac": "02:00:00:00:00:01",
    "ip": "172.30.0.3",
    "gateway": "172.30.0.1",
    "netmask": "255.255.255.0",
    "mtu": 1500
  },
  "format": "overlaybd",
  "files": {
    "rootfs.ext4":  { "sha256": "a1b2…", "sizeBytes": 32212254720 },
    "vmstate.snap": { "sha256": "c3d4…", "sizeBytes": 1835008 },
    "memory.snap":  { "sha256": "e5f6…", "sizeBytes": 2147483648 },
    "overlaybd/rootfs/layer.lsmt":  { "sha256": "…", "sizeBytes": … },
    "overlaybd/memory/layer.lsmt":  { "sha256": "…", "sizeBytes": … }
  },
  "validation": { "booted": true, "restored": true }
}
```

## Field reference

| Field | Meaning and purpose |
| --- | --- |
| `schemaVersion` | Manifest schema version, currently always `1`. Forward-compatibility marker; no consumer branches on it yet |
| `runtime` | The runtime that produced the set, currently always `"firecracker"` — declares that this vmstate/memory can only be restored by the Firecracker driver |
| `lineage` | Provenance object: **where the set came from and what was baked in at build time**. Written once by the build; snapshots/checkpoints carry it forward verbatim (only `lineage.image` is rewritten to the reference each generation booted from, while `imageDigest` always anchors the original build) — the pedigree of the artifact set across generations |
| `lineage.image` | Source image reference. A template build writes the template's `spec.image`; a snapshot/checkpoint overwrites it with the `image` reference the Sandbox booted from at capture time (the template name, if it was created from one) |
| `lineage.imageDigest` | Digest of the source OCI image at the original build. No generation ever overwrites it, so it anchors the chain no matter how many snapshots deep it goes |
| `lineage.execd` | OpenSandbox execd image reference injected into the guest rootfs at build time. The injection happened during the build; this field records which execd the rootfs carries and is never re-resolved at runtime |
| `lineage.kernel` | Guest kernel: `name` is the kernel file baked into the builder image, `digest` is its SHA-256. The kernel is a build-time asset only — nodes never carry one (restore is a vmstate resume that does not boot a kernel) — and it rides forward through snapshots |
| `lineage.entrypoint` | The business command argv declared by the template. Already wired into the rootfs boot chain at build time; published verbatim as provenance/audit |
| `lineage.init` | Path of the injected guest PID 1 (default `/usr/local/sbin/sandbox-init`; empty means no injection — the image's own init is responsible). A recorded build-time fact |
| `lineage.envs` | The template envs published **verbatim** (no `valueFrom` support); written into the guest's `/etc/sandbox-init.env` at build time on top of the source image's inherited OCI `Config.Env` (not recorded here — recover it from `lineage.imageDigest` via the registry), overriding it per name. Never put secrets here — anyone who can read the manifest can read these values; credentials belong in `publishSecretRef` |
| `machine` | The snapshot's resource triad: `vcpu` and `memory` are resource quantities (e.g. `"2"`, `"2Gi"`); `rootfs` is the actual rootfs capacity (the declared minimum rounded up to whole GiB, e.g. `"30Gi"`, matching `files["rootfs.ext4"].sizeBytes`). `vcpu`/`memory` are the restore-authoritative configuration — Firecracker refuses to restore with a memory size different from the one the vmstate was created with, so the create request's cpu/mem are only validated: a request below the snapshot memory is rejected explicitly. Snapshots inherit `vcpu`/`memory` verbatim from the source image; `rootfs` is rewritten per capture from the actual artifacts |
| `compatibility` | Snapshot CPU provenance and capture environment. `vendor`/`cpuFamily`/`cpuModel` are the structured CPUID identity of the first `/proc/cpuinfo` processor — the fields consumers match before restoring (8163 and 8269CY share family 6 model 85); `cpuModelName` is the marketing string, display-only. `cpuTemplate` records how the snapshot was CPUID-masked: `"T2"` (Intel) / `"T2A"` (AMD) — portable across the template's allowlist — or `"none"` when the host refused the pinned template and the raw host CPUID was baked in (such artifacts are host-CPU specific by construction). `firecrackerVersion` (the Firecracker binary version) and `hostKernel` (host `uname -r`) complete the capture environment. The restore path does not enforce these fields yet (planned: OSEP-0024 Phase 1 admission). Template builds record the build host; snapshots and checkpoints carry `compatibility` forward **verbatim from the source manifest** — the dumped vmstate carries the source's CPU state, so its compatibility is the source's |
| `guestNetwork` | The guest static network baked into the snapshot (clone networking model): `iface`/`mac`/`ip`/`gateway`/`netmask`/`mtu`. At restore the guest side is unchanged (it lives in the memory image); the node replaces only the host tap. Consumers read `ip`/`gateway`/`netmask`/`mtu` — `mtu` 0 means an older manifest, falling back to the kernel default. Inherited verbatim |
| `format` | `native` or `overlaybd`; both formats contain the complete snapshot set, differing in the extra LSMT layers (for on-demand loading). Snapshots/checkpoints are always `native` |
| `files` | Per-artifact list: keys are publish-relative names, values are `{sha256, sizeBytes}` — sparse-aware digests and logical sizes. The pull side downloads each file and verifies its digest (already-verified files are skipped; corrupt ones are re-pulled). Fixed members: `rootfs.ext4` (renamed `rootfs.img` in the local cache), `vmstate.snap`, `memory.snap`; template builds with `format=overlaybd` add `overlaybd/rootfs/layer.lsmt` and `overlaybd/memory/layer.lsmt` |
| `validation` | Capture-time validation flags: `booted` — the image/snapshot started successfully; `restored` — an additional restore verification ran (true for template builds, always false for live dumps — the vmstate itself is the running evidence). Informational |
| `actionBindings` | Written only by snapshots/checkpoints and only when non-empty: the source Sandbox's action bindings at capture time (`handler` + `input`), typically the egress network policy. This keeps the artifact set self-contained — deleting the SandboxSnapshot CR does not affect the artifacts; the policy travels with them. `CreateSandbox(image=<templateName>)` reads and merges it into the initial bindings: an explicitly passed same-handler binding wins, handlers the target Pool does not declare are dropped, and an unreachable store is skipped. Build products (golden images) carry no such field |

See also: [SandboxTemplate golden images](sandboxtemplate-golden-images.md),
[Sandbox Snapshots](sandbox-snapshots.md),
[Pause, resume, and snapshot integration](pause-resume-snapshot-integration.md).
