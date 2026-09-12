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
- **Caveats**: `resize` needs kernel 6.11+ (UBLK_F_UPDATE_SIZE) and is out of scope;
  daemon-owned devices must be deleted through its API; mounting a writable image
  twice is rejected by the daemon (upper-layer exclusivity protection).

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

## Risks and mitigations

| Risk | Mitigation |
|------|-----------|
| Existing templates with hypervisor_type=dragonball cannot be resumed | Hard pre-flight validation with a clear error; VMM abstraction reserved |
| vmstate version incompatible with the host firecracker | `firecracker_version` check; `--force` escape hatch |
| In-disk layout is only a verbal convention | Verify with a real image during integration; paths centralized in one constants block |
| Concurrent cred.json clobbering | Atomic write (tmp+rename) + file lock |
| ublkd device leakage (CLI crash) | state.json records dev_ids; delete is idempotent; a janitor may be added later |
| Kernel requirements | Baseline ublk support suffices (overlaybd upstream validated on a 5.10 backport kernel; 6.11+ only needed for resize) |
