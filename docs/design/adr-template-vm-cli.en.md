# ADR: template-vm CLI — A Standalone CLI That Creates VMs from Remote Templates

> Document type: Architecture Decision Record (ADR)
>
> Date: 2026-09-10
>
> Status: Confirmed (converged after three design-interview rounds)
>
> Companion glossary: [template-vm-cli-glossary.md](./template-vm-cli-glossary.md) (Chinese)

## Context

We need a standalone CLI tool: given a `template.json` (registry repo + template_id +
dockerAuth), it restores a running VM on the local host from a template stored in a
remote OCI registry. A template is a **pair** of overlaybd images:
`${repo}:${template_id}_rootfs` and `${repo}:${template_id}_snapfiles`. The device creation goes through overlaybd-ublkd;
the VMM resume path follows AgentENV and other firecracker sandbox implementation.

This tool **bypasses every control plane** (no fast-sandbox fastpath/CRD/reconciler
involvement). It is a single-host data-plane tool.

## Decisions

### ADR-001 Code location: a standalone binary inside the fast-sandbox repo

- **Decision**: the code lives in `fast-sandbox/cmd/template-vm/` (Go, cobra), as a
  sibling of fastctl. It is NOT wired into fastctl's subcommand tree and does NOT
  reuse the fastpath gRPC stack.
- **Rationale**: the user explicitly requested a standalone CLI hosted in the
  fast-sandbox repository. With zero control-plane interaction, a separate binary
  prevents data-plane dependencies (oras, ublkd client) from leaking into the
  control plane via the layering rules enforced by
  `internal/architecture/dependencies_test.go`.
- **Trade-off**: fastctl's endpoint/config plumbing cannot be reused — this tool
  does not need it either (there is no server to talk to).

### ADR-002 Device creation: overlaybd-ublkd (HTTP over unix socket)

- **Decision**: call the overlaybd-ublkd control API: `POST /v1/add {"config": <path>}` /
  `POST /v1/del {"dev_id": N}` / `GET /v1/list`. The socket defaults to
  `/var/run/overlaybd-ublk/ublkd.sock` (overridable via flag).
- **Rationale**: user requirement, per the official overlaybd
  [standalone-usage.md](https://github.com/containerd/overlaybd/blob/main/docs/standalone-usage.md#overlaybd-ublkd-many-devices-in-one-process).
- **Rejected alternatives** (investigation findings):
  - AgentENV `uvm-ublk-daemon`: JSON RPC over a unix socket
    (`DaemonRequest::CreateOverlaybd`), not HTTP, and coupled to AgentENV's
    image.json/config model;
  - accelerated-container-image `overlaybd-attacher`: tcmu-based, not ublk.
- **Deployment assumption**: ublkd is pre-deployed via systemd (upstream unit
  template `/opt/overlaybd/overlaybd-ublkd.service`); the CLI only checks socket
  reachability at startup and never launches the daemon itself.
- **Caveats**: daemon-owned devices must be deleted through its API; mounting a
  writable image twice is rejected by the daemon (upper-layer exclusivity
  protection). Build-time shrinking does not use the online `UBLK_F_UPDATE_SIZE`
  operation that requires kernel 6.11+: it deletes the device, resizes the upper
  offline, and adds the device again.

### ADR-003 Template resolution: pull manifests with oras, generate config.v1.json in the CLI

- **Decision**: the CLI uses an oras/distribution client with the dockerAuth from
  template.json to pull both image manifests, then generates `config.v1.json`
  the way accelerated-container-image does
  (`OverlayBDBSConfig{lowers[], upper?, repoBlobUrl}`, see `pkg/types/types.go`).
- **Details**: the rootfs config carries an `upper` (a local hybrid writable layer;
  create the empty data/index files before `add`); the snapfiles config has no
  `upper` (read-only). `repoBlobUrl` points at
  `https://<registry>/v2/<repo>/blobs`; overlaybd performs range reads against it
  at runtime.
- **Credential handoff**: the dockerAuth (a docker config json string) is merged into
  overlaybd's global credential file (default `/opt/overlaybd/cred.json`; overlaybd
  lazily reloads it per remote_path), otherwise on-demand pulls fail with 401.
  The merge must be atomic (tmp+rename) and file-locked so concurrent CLI
  instances cannot clobber each other.

### ADR-004 Kernel: host-local, validated against metadata, flag-overridable

- **Decision**: the kernel is not distributed with the images. The
  `metadata.json.kernel_version` maps to a vmlinux under a host convention
  directory (default `/opt/template-vm/kernels/vmlinux-<kernel_version>`,
  overridable with `--kernel`). A missing file is a hard error.

### ADR-005 memfile: raw memory file mmap (Firecracker File backend)

- **Decision**: after read-only mounting the snapfiles device, firecracker
  `PUT /snapshot/load` uses
  `mem_backend = {backend_type: "File", backend_path: <mnt>/memfile}`.
- **Rationale**: The memfile lives inside ext4 on a ublk device, so page faults 
  flow through ext4 → ublk → overlaybd remote range reads; the guest's first 
  writes are copy-on-written into anonymous pages inside the firecracker process 
  and never touch the read-only layer.

### ADR-006 In-disk layout convention: ext4 + fixed paths

- **Decision**: the snapfiles device is ext4 with fixed paths after mount:
  `/vmstate.bin`, `/memfile`, `/metadata.json`. This is an **existing external
  convention that the CLI adapts to**; changes on the builder side require
  versioned announcements.
- **Risk**: the convention is currently only verbal; verify paths and the FS type
  against a real sample image during integration testing.

### ADR-007 Machine spec source: metadata.json is authoritative, flags may override

- **Decision**: `vcpu_count / memory_mb / disk_size_mb` etc. come from
  metadata.json; the CLI offers `--vcpu / --memory-mb / --kernel` overrides.
  Pre-flight validation: `hypervisor_type` currently supports only `firecracker`
  (any other value, e.g. `dragonball`, fails with Unsupported — a VMM abstraction
  seam is reserved); `cpu_arch` must match the host; the local firecracker binary
  version must be compatible with `firecracker_version` (mismatch is an error by
  default, downgraded to a warning with `--force`).

### ADR-008 rootfs upper: hybrid writable layer, no commit by default

- **Decision**: each sandbox gets its own LSMT hybrid-mode upper (data/index in the
  state directory), discarded when the VM is destroyed. The `overlaybd-commit` +
  push-back-to-registry flow triggered by sandbox snapshot requests is **out of
  scope for this iteration**; the extension point is reserved.

### ADR-009 Command surface and lifecycle: create / list / delete

- **Decision**: semantics follow aenv (start = bring a sandbox up from a template;
  no start/resume distinction). The MVP has three commands:
  - `template-vm create -f template.json [--kernel ...] [--vcpu ...] [--network=none]`
  - `template-vm list`: enumerate the state directory + verify pid liveness
  - `template-vm delete <sandbox_id>`: kill firecracker → umount snapfiles →
    ublkd `del` both devices → remove the state directory (strictly reversed
    order; every step reports failures but cleanup continues best-effort)
- **Layout**: state directory `/var/lib/template-vm/sandboxes/<id>/` (upper,
  config.v1.json, mount point, state.json); runtime directory
  `/run/template-vm/sandboxes/<id>/` (api socket, serial log).

### ADR-010 Networking: NetworkProvider abstraction, MVP supports none

- **Decision**: metadata.json's `network{host_dev_name, mac}` belongs to an existing
  networking stack (`cnid-*`). The CLI defines a NetworkProvider interface
  (Acquire(metadata) → device + cleanup callback); the concrete integration spec
  is **pending input from the networking side**. The MVP defaults to
  `--network=none` so the snapshot-resume main path is unblocked.
- **Open input**: the device-allocation interface of that networking stack (to be
  captured in a follow-up ADR once provided).

### ADR-011 Template building: `template-vm build` via cold-boot snapshotting (build iteration)

- **Decision**: `template-vm build --source <OCI or overlaybd image> --output <name:tag>`
  converts a remote source into a local template pair (`<tag>_rootfs` /
  `<tag>_snapfiles`). The CLI resolves registry manifests with ORAS. For an
  ordinary OCI/Docker tar image it invokes the `accelerated-container-image`
  userspace `convertor`, produces Native OverlayBD locally, and then enters the
  same build path as an already accelerated source. template-vm never invokes
  the docker CLI.
- **Read-path divergence between build and create**: create stays manifest-only
  (lowers are addressed by `digest` + `repoBlobUrl` and range-read remotely).
  Build resolves and classifies the host-platform manifest first. Native sources
  are downloaded with ORAS; ordinary OCI sources are converted with
  `--no-upload --dump-manifest --reserve`. Both paths finish with Native layers
  under `blobs/sha256/`, referenced through `lowers[].file` during the build.
- **Source format gate**: the whole manifest is classified before layer download.
  An all-Native image proceeds directly; an all-standard OCI/Docker tar image is
  converted. Turbo OCI, tar-wrapped OverlayBD, mixed Native/tar manifests, and
  unknown media types are rejected. Converted output is validated as Native and
  original OCI tar blobs never enter the final artifact.
- **Converter prerequisite**: download `accelerated-container-image` separately
  and run `make bin/convertor`; install it at
  `/opt/overlaybd/snapshotter/convertor` or override `--overlaybd-convertor`.
  Builds always pass `--no-upload`. Private registry credentials may come from
  `--username username:password` or `--auth-file` and are forwarded to convertor.
- **Rootfs target size**: `--disk-size-gb` accepts a positive whole-GiB value,
  defaults to 20, and may be overridden. Because the source virtual size is baked
  into its LSMT headers (commonly 64–256 GiB regardless of content size), build
  first probes it with a lowers-only device and creates the initial upper at
  `max(ceil(source size), target)`. While unmounted, it runs `e2fsck -fy`,
  `resize2fs <device> <target>G` when needed, and `e2fsck -fn`. Only after ext4
  reaches the target does it delete the ublk device, run
  `overlaybd-resize --config ... --size <target>`, update the retained config's
  upper vsize, and add the device again. BLKGETSIZE64 and `dumpe2fs -h` must both
  report the exact target before boot. metadata.json keeps the existing
  `disk_size_mb` field and stores `disk_size_gb × 1024`.
- **Snapshot capture**: inject an init script into the writable rootfs (built-in
  redis sample by default, `--init-script-file` overrides) → cold-boot
  Firecracker (boot args carry `init=<init-path>`) → wait for the ready pattern
  on the serial log (default `Ready to accept connections`, configurable;
  on timeout the serial tail is printed) → pause → Full Snapshot
  (`vmstate.bin` + `memfile`) → terminate Firecracker.
- **snapfiles without raw import**: `overlaybd-create --hybrid` creates a blank
  writable device with no lowers → `mkfs.ext4` → rw-mount → write
  `/vmstate.bin`, `/memfile`, `/metadata.json` → sync → umount → ublkd del →
  `overlaybd-commit` seals it into a read-only layer. Rejected:
  `build/sandboxtemplate-builder/overlaybd-import-raw.cpp` is not part of this
  pipeline.
- **Sealing order**: commit the snapfiles upper first, then delete the rootfs
  ublk device and commit the rootfs upper; both commit outputs become the
  incremental read-only layers of the new template pair.

### ADR-012 Build artifact layout and publish contract (build iteration)

- **Decision**: build emits self-contained LOCAL artifacts only; **nothing is
  pushed to a registry in this iteration**:
  ```text
  <output-dir>/<name>_<tag>/
    blobs/sha256/<source layer digests + two committed layer digests + config digest>
    manifest/source-<tag>.json
            <name>_<tag>_rootfs.json
            <name>_<tag>_snapfiles.json
            index.json
  ```
- **Self-containment**: the rootfs manifest preserves the source layer
  descriptors (order and attributes) and appends the rootfs commit layer; every
  referenced blob (inherited layers and the generated minimal config) must exist
  under the local `blobs/sha256/`. Build validates every digest, size, and JSON
  schema in a sibling staging directory, then atomically publishes the final
  directory; it refuses to overwrite an existing artifact.
- **Commit-layer convention**: mediaType is
  `...overlaybd.image.layer.v1.zfile` (default `-z` compression) or
  `...overlaybd.image.layer.v1.lsmt` (`--no-compress`), annotated with
  `containerd.io/snapshot/overlaybd/blob-digest == own digest`, matching the
  native-layer rule of strict consumers.
- **index.json (layout index)**: records `source_ref`, the source manifest
  file/digest, and both images' manifest files/digests (schema_version=1). The
  future push iteration consumes it as the ONLY input: read a target manifest →
  upload every blob it references from `blobs/sha256/` → commit the manifest
  (ORAS SDK). Because source blobs are already local, the pair can publish to
  any registry with no cross-registry blob copy.

### ADR-013 Registry credential matching canonicalization (build iteration)

- **Decision**: ORAS client credential selection canonicalizes docker config
  auths keys the same way as the reference: strip scheme, trailing slashes and
  `/v1` or `/v2` API suffixes; map Docker Hub aliases (`docker.io`,
  `index.docker.io`, `registry.hub.docker.com`, ...) to
  `registry-1.docker.io`; then match the longest registry/repository prefix.
- **Rationale**: the standard key written by `docker login` is
  `https://index.docker.io/v1/`; without canonicalization Hub credentials never
  match and silently degrade to anonymous access (tolerable for public images,
  a 401 for private ones).

## Create flow (strictly ordered; failures roll back in reverse)

1. Parse template.json; verify sandbox_id is free (state directory absent).
2. oras-pull the rootfs and snapfiles manifests → ordered layer digest lists.
3. Merge dockerAuth into `/opt/overlaybd/cred.json` (atomic write).
4. rootfs: create the hybrid upper (data/index) → generate config.v1.json with
   upper → ublkd `/v1/add` → record dev_id.
5. snapfiles: generate read-only config.v1.json → `/v1/add` → mount -o ro →
   read metadata.json / validate (ADR-007).
6. Resolve the kernel path (ADR-004); launch the version-matched firecracker
   (api socket under the runtime directory).
7. Resume: `PUT /drives/rootfs` (path=/dev/ublkbN, drive_id consistent with
   vmstate; PATCH first if not) → `PUT /snapshot/load` (vmstate.bin + memfile
   File backend) → networking (NetworkProvider or none) → `PATCH /vm` resume.
8. Persist the state file; print sandbox info (id, pid, ublk devices, socket path).

## Build flow (strictly ordered; failures roll back in reverse; build iteration)

1. Validate flags; ping ublkd; read `--auth-file` or
   `--username username:password`.
2. Resolve the source manifest with ORAS (traversing one image-index level and
   selecting `linux/<host arch>`) and classify it before downloading layers.
3. Download Native config/layers directly, or pass an ordinary OCI manifest
   digest and credentials to `convertor --no-upload --dump-manifest --reserve`.
   Validate and import its manifest/config/`overlaybd.commit` files into the
   local artifact, then probe the unified Native lowers-only device.
4. rootfs: create and add a hybrid upper sized to `max(source, --disk-size-gb)` →
   offline `e2fsck -fy` → `resize2fs <target>G` when needed → `e2fsck -fn` → when
   upper size differs, delete the device, run `overlaybd-resize`, update config
   vsize, and add again → verify block-device and ext4 geometry equal the target.
5. rw-mount the verified rootfs, inject the init script (0755), sync, umount.
6. Cold-boot Firecracker (1 vCPU / 1 GiB by default) → wait for the serial ready
   pattern → pause → Full Snapshot → terminate Firecracker.
7. snapfiles: blank hybrid upper → `/v1/add` → `mkfs.ext4` → rw-mount → write
   vmstate.bin/memfile/metadata.json → sync → umount → ublkd del → commit.
8. Delete the rootfs ublk device → commit the rootfs upper; move both committed
   layers into `blobs/sha256/`.
9. Emit the minimal config blob, both manifests and index.json; validate the
   whole layout → clean the work directory → print the artifact summary.

## Risks and mitigations

| Risk | Mitigation |
|------|-----------|
| Existing templates with hypervisor_type=dragonball cannot be resumed | Hard pre-flight validation with a clear error; VMM abstraction reserved |
| vmstate version incompatible with the host firecracker | `firecracker_version` check; `--force` escape hatch |
| In-disk layout is only a verbal convention | Verify with a real image during integration; paths centralized in one constants block |
| Concurrent cred.json clobbering | Atomic write (tmp+rename) + file lock |
| ublkd device leakage (CLI crash) | state.json records dev_ids; delete is idempotent; a janitor may be added later |
| Kernel requirements | Baseline ublk support suffices; build changes capacity offline with delete/resize/add and does not require online UBLK_F_UPDATE_SIZE |
| Missing resize utilities | Require `e2fsck`, `resize2fs`, `dumpe2fs`, and `/opt/overlaybd/bin/overlaybd-resize`; any failure triggers reverse rollback |
| Ordinary OCI conversion fails or emits corrupt output | Force convertor no-upload mode and verify every manifest/config/layer digest and size before any ublk operation |
| Source is Turbo OCI, tar-wrapped, mixed, or unknown | Reject during manifest classification; only an all-standard-tar image enters convertor (ADR-011) |
| Private registry credential exposure | ORAS and convertor reuse one credential and template-vm never logs full argv; document that the external convertor's argv-only API remains visible to the same user/root while running |
| Upper shrunk before ext4 (EXT4 bad geometry) | Always resize ext4 offline on the initial large device before deleting/resizing/adding the upper, then verify both geometries exactly (ADR-011) |
| No uniform guest workload start convention | Init-script injection (built-in redis sample default) + `--init-script-file`/`--ready-pattern` overrides; readiness is a serial pattern, not mere process start |
| Local artifacts drifting from registry state | No push in this iteration; index.json pins source_ref and every digest as the single publish input for reconciliation (ADR-012) |
