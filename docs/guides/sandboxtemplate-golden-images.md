# SandboxTemplate golden-image builds

SandboxTemplate turns an OCI image into a **Firecracker golden image**: a
converted rootfs plus a validated full snapshot, published to an S3-compatible
object store. The Firecracker runtime consumes the artifacts on demand
(`SandboxSpec.Image` → index → manifest → digest-verified pull → restore), so
the template is the build-time half of on-demand loading.

This guide covers operating the resource end to end.

## End-to-end workflow

```text
SandboxTemplate CR (spec)
  → controller creates a privileged build Pod on a KVM node
  → convert: OCI layers → sparse ext4 rootfs + runtime injection (execd/init/envs)
  → validate-boot: cold boot + guest readiness
  → snapshot: pause → full snapshot (vmstate.snap + memory.snap) → restore validation
  → package (format=overlaybd): rootfs/memory → LSMT layers
  → manifest.json + SHA256SUMS (content-addressed)
  → publish: s3://<publish>/<sha256(manifest)[:16]>/… + index/<sha256(image)>.json
  → status.manifestRef / status.artifactDigest recorded on the CR
  → runtime-agent PullImage/PinImage resolves SandboxSpec.Image → restore
```

## Prerequisites

| Item | Requirement |
|------|-------------|
| KVM build nodes | Nodes labeled `sandbox.fast.io/kvm=true`, exposing `/dev/kvm` and `/dev/net/tun`; the build Pod is pinned to them via nodeSelector |
| Object store | S3-compatible bucket (MinIO/OSS/S3); reachable from the builder |
| Publish secret | `imagePullSecrets`-style Secret in the **template's namespace** with keys `accessKeyId`, `secretAccessKey`, `endpoint`, `region` |
| Builder RBAC | Provisioned automatically: the controller creates the `sandbox-template-builder` ServiceAccount + Role (`pods/patch`) + RoleBinding in the template's namespace on first build |
| Pod security | The template's namespace must permit **privileged** Pods (e.g. PodSecurityAdmission `allow` privileged), so the builder can mount `/dev/kvm` and `/dev/net/tun` |
| Controller | The SandboxTemplate reconciler must be running (it drives the build Pod and status) |

The builder image is `build/Dockerfile.sandboxtemplate-builder` (oci2rootfs,
firecracker v1.16.1, embedded guest kernel, OverlayBD toolchain, aws CLI v2).

## Example

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxTemplate
metadata:
  name: ai-office-sandbox
  namespace: fast-sandbox
spec:
  image: registry.example.com/sandbox:v1.0.21
  kernel: vmlinux.bin
  machine:
    vcpu: "2"
    memory: "2Gi"
  entrypoint:
    - /opt/gem/run.sh
  execd: ghcr.io/opensandbox/execd:v1.0.21
  init: /usr/local/sbin/sandbox-init
  envs:
    - name: SANDBOX_MODE
      value: office
  readiness:
    probe: tcp://127.0.0.1:44772
    warmupSeconds: 60
  output:
    rootfsSize: "30Gi"
    format: overlaybd
    # publish is optional when the platform artifact store is configured
    publish: s3://sandbox-images/publish
    publishSecretRef:
      name: sandbox-images-readwrite
```

```sh
kubectl apply -f template.yaml
kubectl -n fast-sandbox get sandboxtemplate ai-office-sandbox -o yaml   # status.phase
```

## Field reference

### `spec.image`

Required. The OCI image reference to convert, e.g.
`registry.example.com/sandbox:v1.0.21`. This exact string becomes the
**consumer-side addressing key**: `SandboxSpec.Image` must be byte-identical
(no default tag, no digest pin, no trimming) — the published index is keyed
by `sha256(image)` verbatim, and both sides reject empty references.

### `spec.kernel`

Required. The guest kernel name embedded in the builder image
(`/usr/local/share/firecracker/<name>`, default `vmlinux.bin`; the
`KERNEL_NAME`/`KERNEL_URL` build args pin it). The kernel is a **build-time
asset**: nodes never preinstall it, restore does not boot a kernel. Its digest
is recorded in the manifest for compatibility matching.

### `spec.machine`

The reference machine of the golden image — the CPU/memory the snapshot is
created with. Restore requires the same memory as the snapshot (Firecracker
rejects a different `mem_size_mib`), so the request memory must be ≥ this
value. Both are Kubernetes resource quantities.

| Field | Default | Meaning |
| --- | --- | --- |
| `machine.vcpu` | `"1"` | vCPUs of the snapshot VM |
| `machine.memory` | `"2Gi"` | Guest memory of the snapshot VM |

### `spec.entrypoint`

Optional. The guest business command as an argv list; empty defaults to
`["tail", "-f", "/dev/null"]` (the sandbox stays alive as an environment and
work is driven through execd/SDK). An explicit value fully replaces the
default. At runtime `CreateSandboxRequest.entrypoint` overrides the template
value.

### `spec.execd`

Optional. The OpenSandbox execd image whose runtime files are injected into
the guest rootfs at build time. When set, the guest init probes execd `/ping`
as the default readiness gate.

### `spec.init`

Optional. The injected guest PID 1 path inside the rootfs; empty defaults to
`/usr/local/sbin/sandbox-init`. The init script carries the readiness marker
(`SANDBOX_READY`), the heartbeat loop (`SANDBOX_HEARTBEAT`), mounts, envs, and
the entrypoint. It does **not** re-run after a snapshot restore — that is why
readiness and network are baked into the snapshot.

### `spec.envs`

Optional. Literal `EnvVar` array written verbatim into
`/etc/sandbox-init.env`. `valueFrom` is not supported, and the source image's
own `Config.Env` is not merged. Do not place secrets here — they are published
verbatim in the manifest; use `publishSecretRef` for credentials.

### `spec.readiness`

Defines when the guest is considered ready for the snapshot.

| Field | Default | Meaning |
| --- | --- | --- |
| `readiness.probe` | empty | Custom gate, checked first: `tcp://host:port` or `cmd://<command>` |
| `readiness.warmupSeconds` | `60` | Fallback time-based warmup (minimum 0) |
| `readiness.healthCheck` | empty | Fallback health command; empty uses the source image `CMD-SHELL` |

Precedence: custom `probe` → execd `/ping` (when execd is injected) →
`warmupSeconds` + `healthCheck`.

### `spec.output`

| Field | Required | Default | Meaning |
| --- | ---: | --- | --- |
| `output.rootfsSize` | Yes | `"30Gi"` | Logical capacity of the produced `rootfs.ext4` (Kubernetes quantity). Rounded **up** to SI GiB for oci2rootfs (`30Gi` → 33G → ~30.7 GiB sparse file) |
| `output.format` | Yes | `overlaybd` | `native` (raw snapshot files only) or `overlaybd` (plus LSMT layers for on-demand range reads); both contain the full artifact set |
| `output.publish` | No | platform store | S3-compatible target, e.g. `s3://sandbox-images/publish`. Empty defaults to the platform artifact store (`fast-sandbox-artifact-store` ConfigMap); a non-empty value must equal that store or the build fails with an `InvalidOutput` condition, so a template cannot silently publish where no node agent reads. Digest-addressed publication; the builder itself refuses to run with an empty target unless `SANDBOX_TEMPLATE_ALLOW_NO_PUBLISH=1` |
| `output.publishSecretRef` | No | — | Name of the write-credential Secret in the template's namespace (`accessKeyId`/`secretAccessKey`/`endpoint`/`region`); the controller mounts it into the build Pod and the builder reads the keys as files (never env-injected). Without it the build relies on platform-level credentials (IRSA/node metadata). Runtime nodes read with a **separate** read-only credential — write keys never reach them |
| `output.prime` | No | — | Reserved: seed-node cache priming after a successful build; not yet implemented |

The platform store comes from the `fast-sandbox-artifact-store` ConfigMap
(`store`/`endpoint`, see [Configuration reference](../reference/configuration.md#artifact-store)),
which the controller mounts (reading it per build) and injects into build Pods:
the build endpoint and `spec.output.publish` can no longer drift apart from
what the node agents pull.

## Build lifecycle

- **Status machine**: `Pending` → `Building` → `Succeeded` / `Failed`. On
  success `status.manifestRef` (the published manifest URI) and
  `status.artifactDigest` (sha256 of the manifest document) are recorded, with
  `LastBuildTime` and a `BuildSucceeded=True` condition.
- **Rebuild**: edit the spec — the generation bump triggers a new build. A
  mid-build spec change deletes the stale build Pod immediately (concurrent
  privileged builds cannot stack up).
- **Build Pods** run in the template's own namespace as
  `<template>-build-<generation>`, owned by the template (deleting the
  template cascades to its Pods via the garbage collector), pinned to
  `sandbox.fast.io/kvm=true` nodes with `/dev/kvm` and `/dev/net/tun` passed
  through; they have a 2 h deadline and a 10 min Pending timeout (a missing
  KVM node fails the build instead of hanging forever). Finished Pods are
  kept 24 h for annotation inspection, then reaped by the controller.
- **Diagnostics**: failed builds surface the container exit reason/message in
  the condition; the Pod logs (`kubectl -n <namespace> logs <build-pod>`)
  carry per-stage timings (`pullMs`, `bootToReadyMs`, `snapshotCreateMs`,
  `restoreToHeartbeatMs`, `importRootfsMs`, …).

## Published artifacts

```text
s3://sandbox-images/publish/
├── <sha256(manifest)[:16]>/          # per-build digest namespace
│   ├── rootfs.ext4                   # sparse ext4 (native filename: rootfs.img)
│   ├── vmstate.snap
│   ├── memory.snap
│   ├── overlaybd/{rootfs,memory}/layer.lsmt   # format=overlaybd only
│   ├── SHA256SUMS                    # audit copy
│   └── manifest.json                 # uploaded last (commit point)
└── index/<sha256(image)>.json        # image → latest manifestRef, last-writer-wins
```

`manifest.json` records (among others):

- `machine` — the vcpu/memory/rootfs tuple restore validates against;
- `guestNetwork` — the NIC baked into the snapshot (`eth0` + static
  guest IP/MAC); the runtime replaces only the host tap via
  `network_overrides`;
- `files` — publish filename → `{sha256, sizeBytes}` for digest-verified pull;
- `compatibility` — firecracker version / host kernel / CPU model;
- `lineage` — build provenance carried by every derived snapshot: `image`,
  `imageDigest`, `execd`, `kernel`, `entrypoint`, `init`, `envs`;
- `validation` — build gates.

The consumer-side cache layout is
`<StateRoot>/images/<sha256(image)>/{rootfs.img, vmstate.snap, memory.snap,
manifest.json}` (native filename mapping: `rootfs.ext4` → `rootfs.img`).

## Consuming the artifacts

- Set `SandboxPool.spec.warmImages` to the template's `spec.image` reference
  to pre-warm node caches; sandboxes created with the same `image` restore
  from the published snapshot.
- The image reference must be byte-identical between template and sandbox
  (no normalization).
- The snapshot machine tuple must fit the requested sandbox resources:
  requesting less memory than the snapshot fails explicitly.
- The snapshot carries one baked guest NIC (`guestNetwork`): every restored
  instance resumes with the same guest IP/MAC (clone networking model), and
  the snapshot holds **no active connections** — re-establish connectivity
  after restore.

## Notes and constraints

- **SandboxTemplate is "run an arbitrary OCI image as root on a KVM host"**:
  restrict creation to trusted operators (cluster-admin or a dedicated Role).
- The build bakes a NIC but deliberately not connectivity; snapshots of
  workloads with live connections are out of scope.
- `rootfsSize` is a minimum, not an exact size; the file is sparse.
- Readiness must reflect the real workload: a misconfigured gate can produce a
  "healthy" snapshot that is not actually ready.

## See also

- [E2E: golden-image builder](sandboxtemplate-golden-image-e2e.md)
- [Reference: API](../reference/api.md) (SandboxTemplate CRD fields)
