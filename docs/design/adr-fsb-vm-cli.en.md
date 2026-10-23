# ADR: fsb-vm CLI — A Standalone CLI That Creates VMs from Remote Templates

> Current contract: one `manifest.json`, `schemaVersion: 1`, shared by OCI and S3. See [Artifact manifest reference](../guides/artifact-manifest-reference.md). Historical metadata layouts must be rebuilt; no legacy decoder is provided.

> Document type: Architecture Decision Record (ADR)
>
> Date: 2026-09-10
>
> Status: Confirmed (converged after three design-interview rounds)
>
> Companion glossary: [fsb-vm-cli-glossary.md](./fsb-vm-cli-glossary.md) (Chinese)

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

- **Decision**: the code lives in `fast-sandbox/cmd/fsb-vm/` (Go, cobra), as a
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
- **Credential handoff**: select the credential with ORAS semantics, then write it to
  overlaybd's global credential file (default `/opt/overlaybd/cred.json`) under the
  actual blob URL's `<canonical registry host>/<repository>` prefix. Docker Hub aliases
  map to `registry-1.docker.io`; keys are serialized longest first so overlaybd's
  “first match” rule behaves as longest-prefix matching. A file lock serializes merges,
  and tmp + fsync + rename prevents clobbering and partial files.

### ADR-004 Kernel: host-local, validated against metadata, flag-overridable

- **Decision**: the kernel is not distributed with the images. The
  `manifest.json.kernel.version` maps to a vmlinux under a host convention
  directory (default `/opt/fsb-vm/kernels/vmlinux-<kernel.version>`,
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
  `/vmstate.bin`, `/memfile`, `/manifest.json`. This is the shared manifest contract generated by fsb-vm; historical metadata layouts must be rebuilt.
- **Validation**: the manifest uses the shared Go schema and explicit version validation.

### ADR-007 Machine spec and drive path source: manifest.json is authoritative

- **Decision**: `machine.vcpuCount / machine.memoryMiB / machine.rootfsSizeMiB` are fixed by the snapshot;
  create exposes no `--vcpu` or `--memory-mb` override. Only `--kernel` may override
  the host kernel file path. Pre-flight validation accepts only `firecracker`, requires
  the host CPU architecture and CPU identity/template, and checks the local Firecracker version (`--force` may
  downgrade only version errors). `machine.rootfsDrivePath` must be the relative path
  `rootfs.dev`; old templates with no field or a host absolute device path must be
  rebuilt, and `--force` cannot bypass this requirement.

### ADR-008 rootfs upper: hybrid writable layer, no commit by default

- **Decision**: each sandbox gets its own LSMT hybrid-mode upper (data/index in the
  state directory), discarded when the VM is destroyed. The `overlaybd-commit` +
  push-back-to-registry flow triggered by sandbox snapshot requests is **out of
  scope for this iteration**; the extension point is reserved.

### ADR-009 Command surface and lifecycle: create / list / delete

- **Decision**: semantics follow aenv (start = bring a sandbox up from a template;
  no start/resume distinction). The lifecycle commands are:
  - `fsb-vm create -f template.json [--kernel ...] [--network=none]`
  - `fsb-vm list`: enumerate state and verify `(pid, starttime, boot_id)` identity
  - `fsb-vm delete <sandbox_id>`: safely stop Firecracker → release the private rootfs device by absolute config-path ownership → remove the snapfiles alias and release its shared lease → remove Sandbox directories → prune idle snapfiles
  - `fsb-vm cleanup-snapfiles`: release shared snapfiles entries with no Sandbox leases
- **Concurrency and identifiers**: `sandbox_id` must be one safe directory name.
  Non-blocking per-ID flock serializes create/delete, and create atomically claims the
  state directory with `Mkdir`, eliminating check-then-create races.
- **Recoverable state**: state.json is written as `creating` before the first allocation
  and atomically updated after each resource change; it becomes `running` after resume
  and `deleting` during teardown. Failures retain state and the writable upper for a
  retry. Reusable dev_ids are not ownership: delete acts only while ublkd currently
  lists a device under the sandbox's deterministic config path.
- **Process safety**: state records PID, `/proc/<pid>/stat` starttime, and boot_id;
  identity is rechecked before signals are sent through pidfd, preventing PID-reuse kills.
- **Layout**: state directory `/var/lib/fsb-vm/sandboxes/<id>/` (upper,
  config.v1.json, snapfiles alias, state.json); shared read-only device config, mount and ownership record under `/var/lib/fsb-vm/sandboxes/.snapfiles/<key>/`; runtime directory
  `/run/fsb-vm/sandboxes/<id>/` (api socket, serial log, `rootfs.dev` link).

### ADR-010 Networking: NetworkProvider abstraction, MVP supports none

- **Decision**: manifest.json records the baked guest NIC under `guestNetwork`; it does not persist host tap names. The CLI defines a NetworkProvider interface
  (Acquire(metadata) → device + cleanup callback); the concrete integration spec
  is **pending input from the networking side**. The MVP defaults to
  `--network=none` so the snapshot-resume main path is unblocked.
- **Open input**: the device-allocation interface of that networking stack (to be
  captured in a follow-up ADR once provided).

### ADR-011 Template building: `fsb-vm build` via cold-boot snapshotting (build iteration)

- **Decision**: `fsb-vm build --source <OCI or overlaybd image> --output <name:tag>`
  converts a remote source into a local template pair (`<tag>_rootfs` /
  `<tag>_snapfiles`). The CLI resolves registry manifests with ORAS. For an
  ordinary OCI/Docker tar image it invokes the `accelerated-container-image`
  userspace `convertor`, produces Native OverlayBD locally, and then enters the
  same build path as an already accelerated source. fsb-vm never invokes
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
  report the exact target before boot. manifest.json keeps the existing
  `machine.rootfsSizeMiB` field and stores `disk_size_gb × 1024`.
- **Snapshot capture**: inject an init script into the writable rootfs (built-in
  redis sample by default, `--init-script-file` overrides). `--init-path` must be a
  clean absolute guest path. Every lookup is constrained by `os.Root`, and a final
  symlink is replaced, so untrusted image contents cannot redirect the write onto the
  host or another guest file. Then cold-boot Firecracker with `init=<init-path>` →
  wait for the serial ready pattern → pause → Full Snapshot → terminate Firecracker.
- **Portable drive path**: build creates `rootfs.dev -> /dev/ublkbN` in Firecracker's
  working directory and configures the drive with relative path `rootfs.dev`, so vmstate
  contains no host dev_id. Create reconstructs that link in its own runtime directory and
  launches Firecracker with that cwd before snapshot load; no post-load drive PATCH is used.
- **snapfiles without raw import**: `overlaybd-create --hybrid` creates a blank
  writable device with no lowers → `mkfs.ext4` → rw-mount → write
  `/vmstate.bin`, `/memfile`, `/manifest.json` → sync → umount → ublkd del →
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
- **Self-containment and concurrency**: the rootfs manifest preserves the source layer
  descriptors and appends the rootfs commit layer; every referenced blob must exist
  under `blobs/sha256/`. Each build uses an independent `MkdirTemp` scratch directory.
  A non-blocking per-output flock makes a concurrent build of the same output fail
  immediately. Build validates every digest, size, and JSON schema in a sibling staging
  directory, then atomically publishes the final directory and refuses overwrites.
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

### ADR-013 Registry credential matching and OverlayBD handoff canonicalization

- **Decision**: ORAS credential selection strips scheme, trailing slashes, and `/v1`
  or `/v2`; maps Docker Hub aliases to `registry-1.docker.io`; then chooses the longest
  registry/repository prefix. For OverlayBD, the selected credential is re-keyed to the
  blob URL's canonical host/repository prefix and serialized with longest keys first.
- **Rationale**: `docker login` commonly writes `https://index.docker.io/v1/`, which
  otherwise fails to match Hub references. OverlayBD independently returns the first
  exact URL-prefix match in JSON order; without re-keying and ordering, ORAS can
  authenticate while remote block reads still fail with 401.

## Create flow (strictly ordered; failures roll back in reverse)

1. Parse template.json; strictly validate sandbox_id; take its non-blocking lock;
   atomically claim the directories; write `creating` state.
2. ORAS-pull the rootfs and snapfiles manifests → ordered layer digest lists.
3. Canonicalize dockerAuth for OverlayBD blob URL prefixes and merge cred.json.
4. snapfiles: acquire a durable lease on the cache entry for the resolved layers and credential scope; the first user persists config ownership → `/v1/add` → mounts read-only. Later users reuse that device and mount within the same state directory and mount namespace. Read and validate manifest.json for the rootfs size.
5. rootfs: persist config ownership before allocation → create a private hybrid upper sized from manifest.json → ublkd `/v1/add` → update the device record.
6. Resolve the kernel; create `rootfs.dev` in the runtime directory; launch
   Firecracker with that cwd; persist `(pid, starttime, boot_id)`.
7. `PUT /snapshot/load` directly (relative `rootfs.dev` already resolves; memfile uses
   the File backend) → networking → `PATCH /vm` resume.
8. Set state to `running` and print sandbox info. Any failure invokes the same retryable
   teardown; incomplete cleanup retains state.

## Build flow (strictly ordered; failures roll back in reverse; build iteration)

1. Validate flags; ping ublkd; read `--auth-file` or `--username`; acquire a
   non-blocking output lock and create an independent temporary scratch directory.
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
   vmstate.bin/memfile/manifest.json → sync → umount → ublkd del → commit.
8. Delete the rootfs ublk device → commit the rootfs upper; move both committed
   layers into `blobs/sha256/`.
9. Emit the minimal config blob, both manifests and index.json; validate the
   whole layout → clean the work directory → print the artifact summary.

## Risks and mitigations

| Risk | Mitigation |
|------|-----------|
| Existing templates with runtime=dragonball cannot be resumed | Hard pre-flight validation with a clear error; VMM abstraction reserved |
| vmstate version incompatible with the host firecracker | `compatibility.firecrackerVersion` check; `--force` escape hatch |
| In-disk layout is only a verbal convention | Verify with a real image during integration; paths centralized in one constants block |
| cred.json clobbering or keys that do not match blob URLs | File lock + tmp/fsync/rename; canonical host/repository keys ordered longest first |
| ublkd device leakage after a CLI crash | Persist config ownership before/after allocation; delete matches current ublkd config identity and retains state on failure |
| PID reuse kills an unrelated process | `(pid, starttime, boot_id)` identity plus pidfd signaling |
| vmstate pins a host device path | Relative `rootfs.dev` supplied through build/create cwd; old templates must be rebuilt |
| Malicious init-path symlink escapes the image | Clean absolute guest path validation plus `os.Root`-confined writes |
| Kernel requirements | Baseline ublk support suffices; build changes capacity offline with delete/resize/add and does not require online UBLK_F_UPDATE_SIZE |
| Missing resize utilities | Require `e2fsck`, `resize2fs`, `dumpe2fs`, and `/opt/overlaybd/bin/overlaybd-resize`; any failure triggers reverse rollback |
| Ordinary OCI conversion fails or emits corrupt output | Force convertor no-upload mode and verify every manifest/config/layer digest and size before any ublk operation |
| Source is Turbo OCI, tar-wrapped, mixed, or unknown | Reject during manifest classification; only an all-standard-tar image enters convertor (ADR-011) |
| Private registry credential exposure | ORAS and convertor reuse one credential and fsb-vm never logs full argv; document that the external convertor's argv-only API remains visible to the same user/root while running |
| Upper shrunk before ext4 (EXT4 bad geometry) | Always resize ext4 offline on the initial large device before deleting/resizing/adding the upper, then verify both geometries exactly (ADR-011) |
| No uniform guest workload start convention | Init-script injection (built-in redis sample default) + `--init-script-file`/`--ready-pattern` overrides; readiness is a serial pattern, not mere process start |
| Local artifacts drifting from registry state | No push in this iteration; index.json pins source_ref and every digest as the single publish input for reconciliation (ADR-012) |
