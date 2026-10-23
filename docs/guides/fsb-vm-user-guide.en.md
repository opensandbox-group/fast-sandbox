# fsb-vm CLI user guide

[中文版](fsb-vm-user-guide.md)

`fsb-vm` directly controls OverlayBD ublkd and Firecracker on one host. It can:

1. build local template artifacts from an ordinary OCI/Docker image or a Native OverlayBD image;
2. restore a rootfs/snapfiles template pair from a registry as a running Firecracker VM;
3. inspect and delete local VMs and their resources.

The tool bypasses the fast-sandbox control plane and is intended for development, validation, and host-side integration. Most operations require root privileges.

All commands in this guide use the fast-sandbox project as their root. Set these environment variables first:

```bash
cd ./fast-sandbox
export FAST_SANDBOX_ROOT="$PWD"
export FSB_VM="$FAST_SANDBOX_ROOT/bin/fsb-vm"
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/../accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
export FSB_VM_ASSETS="$FAST_SANDBOX_ROOT/.fsb-vm"
export FIRECRACKER="$FSB_VM_ASSETS/firecracker"
export KERNEL="$FSB_VM_ASSETS/vmlinux-6.1.176"
export INIT_SCRIPT="$FSB_VM_ASSETS/init.sh"
export TEMPLATE_FILE="$FSB_VM_ASSETS/template.json"
export DOCKER_CONFIG_FILE="$FSB_VM_ASSETS/docker-config.json"
export BUILD_WORK_DIR="$FSB_VM_ASSETS/builds"
export BUILD_OUTPUT_DIR="$FSB_VM_ASSETS/artifacts"
export STATE_DIR="$FSB_VM_ASSETS/state"
export RUNTIME_DIR="$FSB_VM_ASSETS/runtime"
mkdir -p "$FSB_VM_ASSETS"
```

Place Firecracker, the kernel, the init script, the template manifest, and the optional Docker config at the paths represented by these variables, or adjust the variable values for the host.

## 1. Prerequisites

The host requires:

- Linux, KVM, and a Firecracker binary matching the host architecture;
- a running `overlaybd-ublkd`, with `/var/run/overlaybd-ublk/ublkd.sock` as the default socket;
- these OverlayBD tools:
  - `/opt/overlaybd/bin/overlaybd-create`
  - `/opt/overlaybd/bin/overlaybd-apply`
  - `/opt/overlaybd/bin/overlaybd-commit`
  - `/opt/overlaybd/bin/overlaybd-resize`
- `e2fsck`, `resize2fs`, `dumpe2fs`, `mount`, and `umount`;
- the accelerated-container-image userspace convertor when building from an ordinary OCI image.

Run a basic preflight check:

```bash
test -c /dev/kvm
test -S /var/run/overlaybd-ublk/ublkd.sock
test -x /opt/overlaybd/bin/overlaybd-create
test -x /opt/overlaybd/bin/overlaybd-apply
test -x /opt/overlaybd/bin/overlaybd-commit
test -x /opt/overlaybd/bin/overlaybd-resize
curl -fsS --unix-socket /var/run/overlaybd-ublk/ublkd.sock \
  http://localhost/v1/ping
```

### 1.1 Install the OCI convertor

`accelerated-container-image` is a separate prerequisite and is not downloaded automatically by fast-sandbox. The currently tested revision is `a00e366f9bfc970ec4aaa00887541c4c1d9f9dab`:

```bash
cd "$ACCELERATED_CONTAINER_IMAGE_DIR"
git checkout a00e366f9bfc970ec4aaa00887541c4c1d9f9dab
make bin/convertor
sudo install -D -m0755 bin/convertor \
  /opt/overlaybd/snapshotter/convertor
cd "$FAST_SANDBOX_ROOT"
```

If its checkout is adjacent to fast-sandbox, build only the dependency with:

```bash
make fsb-vm-convertor
```

To use a different checkout under the fast-sandbox root:

```bash
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/third_party/accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
make fsb-vm-convertor \
  ACCELERATED_CONTAINER_IMAGE_DIR="$ACCELERATED_CONTAINER_IMAGE_DIR"
```

The convertor is used only for ordinary OCI/Docker inputs. Native OverlayBD inputs do not require it. `fsb-vm` always invokes the convertor with `--no-upload`.

## 2. Build the CLI

From the fast-sandbox repository root:

```bash
go test ./cmd/fsb-vm/
go vet ./cmd/fsb-vm/
make fsb-vm
```

The output is:

```text
$FAST_SANDBOX_ROOT/bin/fsb-vm
```

Alternatively, build it directly:

```bash
go build -o "$FSB_VM" ./cmd/fsb-vm/
```

Inspect the command help:

```bash
"$FSB_VM" --help
"$FSB_VM" build --help
"$FSB_VM" create --help
"$FSB_VM" list --help
"$FSB_VM" delete --help
```

## 3. Command overview

| Command | Purpose |
| --- | --- |
| `build` | Build a local rootfs/snapfiles template pair from a remote source image |
| `create -f template.json` | Restore and run a VM from a registry template |
| `list` | List local sandboxes and derive status from the Firecracker PID |
| `delete <sandbox_id>` | Stop a VM, unmount snapfiles, and remove ublk devices and state |

Common global options:

| Option | Default | Purpose |
| --- | --- | --- |
| `--ublkd-socket` | `/var/run/overlaybd-ublk/ublkd.sock` | ublkd control socket |
| `--cred-file` | `/opt/overlaybd/cred.json` | OverlayBD global credential file |
| `--reject-credential-conflicts` | `false` | Reject account changes in a shared daemon credential file |
| `--kernel-dir` | `/opt/fsb-vm/kernels` | Default directory for `vmlinux-<version>` during create |
| `--state-dir` | `/var/lib/fsb-vm/sandboxes` | Persistent sandbox state root |
| `--manifest-cache-dir` | `<state-dir>/.manifest-cache` | Immutable registry metadata cache; may be shared independently of sandbox state |
| `--runtime-dir` | `/run/fsb-vm/sandboxes` | API socket and serial-log root |
| `--firecracker` | `firecracker` | Firecracker executable |
| `--overlaybd-convertor` | `/opt/overlaybd/snapshotter/convertor` | OCI userspace convertor |
| `--build-work-dir` | `/var/lib/fsb-vm/builds` | Build scratch root |
| `--build-output-dir` | `/var/lib/fsb-vm/artifacts` | Local artifact root |
| `--plain-http` | `false` | Use plain HTTP for registry requests |

Place global options before the subcommand, for example:

```bash
sudo "$FSB_VM" --plain-http --state-dir "$STATE_DIR" list
```

## 4. Build a template

For Kubernetes SandboxTemplate builds, set `spec.output.publish` to the repository
only, for example `oci://registry.example.com/team/templates`, without a tag or
digest. The controller assigns a random UUID to each build Pod; the builder
publishes `<uuid>_rootfs` and `<uuid>_snapfiles` and returns the immutable
snapfiles digest in `status.manifestRef`. A new spec generation receives a new
UUID, while reconciliation retries reuse the existing Pod's UUID. Applying an
unchanged spec does not rebuild. See [OCI template rebuilds](overlaybd-oci-sandboxes.md#rebuilds-and-generated-tags)
for migration from tagged publish addresses.

The standard `SandboxTemplate` builder image includes `fsb-vm`, the convertor and OverlayBD tools. For `oci://` output, the builder starts ublkd, reads the source manifest/config to pin the linux/amd64 image digest and prepare environment variables and runtime injections, then passes the remote digest reference directly to `fsb-vm build`. The `fsb-vm` build flow reads the source layers; the builder does not stage an OCI layout or start a local registry. The `format: native` object-store path retains `pullOCILayout → oci2rootfs`. OverlayBD is supported only with OCI publication, not S3. `SANDBOX_TEMPLATE_IMAGE_TAR` is supported only on that object-store path and is rejected for OCI output. OCI builds require access to both source and destination registries.

The standard Fastlet image built with `build/Dockerfile.fastlet` includes the OCI runtime tools. Set `FAST_SANDBOX_OCI_ENABLED=true` in an OCI Firecracker Pool to start ublkd; it is disabled by default so other runtime deployments do not require ublk. The standard builder uses `auto` mode so non-OCI builds do not require ublk. OCI registry access supports anonymous or username/password authentication, including Docker config `auth` entries. Identity-token-only credentials are rejected; when both are present, username/password credentials are used. The public registry credential fields and other runtimes are unchanged.

Checkpoints default to the source template repository; `FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY=registry/repository` overrides the destination. The selected destination credential must allow push (use the registry rule's `writeSecretRef` for a separate write account). Checkpoint publication selects parent-read and destination-write credentials by full repository reference; configure both in the Pool when they differ. OCI GuestCopy mounts ublk devices directly; regular rootfs image files continue to use loop mounts.

Minimal invocation:

```bash
sudo "$FSB_VM" build \
  --source <registry>/<repository>:<tag> \
  --output <name>:<template-id> \
  --kernel "$KERNEL"
```

`--output` is a local logical name, not a registry reference, and its name component cannot contain `/`. For example, `code-interpreter:v1.0.0` produces manifests intended for:

- `<registry-repository>:v1.0.0_rootfs`;
- `<registry-repository>:v1.0.0_snapfiles`.

Common build options:

| Option | Default | Description |
| --- | --- | --- |
| `--vcpu` | `1` | Guest vCPU count |
| `--memory-mb` | `1024` | Guest memory in MiB |
| `--disk-size-gb` | `20` | Rootfs virtual disk size in GiB; must be positive |
| `--snapfiles-size-mb` | derived | Snapfiles virtual disk size |
| `--kernel-version` | derived from `vmlinux-<version>` | Kernel version recorded in template metadata |
| `--manifest-extra-file` | empty | Optional guestNetwork, lineage and platform fields that supplement the generated VM manifest; core VM fields cannot be overridden |
| `--init-script-file` | built-in Redis init | Script injected into the guest and run as PID 1 |
| `--ready-pattern` | `Ready to accept connections` | Serial-log text that marks workload readiness |
| `--ready-timeout` | `3m` | Readiness timeout |
| `--auth-file` | empty | Docker config JSON credential file |
| `--username` | empty | Registry credentials as `username:password` |
| `--keep-work-dir` | `false` | Keep build scratch after success |
| `--no-compress` | `false` | Disable zfile compression when committing layers |

`build` does not execute the source image's OCI Entrypoint, Cmd, or Env automatically. For non-Redis workloads, provide `--init-script-file` and make the script print a stable string matching `--ready-pattern` only after the service is ready. `--init-path` must be a clean absolute guest path without whitespace; `os.Root`-confined writes prevent image symlinks from escaping onto the host. Every build uses an independent temporary scratch directory, and a non-blocking per-output lock rejects a second concurrent build of the same `--output`.

### 4.1 Build from a Native OverlayBD image

The following public test image is Native OverlayBD. The build downloads its Native layers directly and bypasses the convertor:

```bash
sudo "$FSB_VM" \
  --firecracker "$FIRECRACKER" \
  --build-work-dir "$BUILD_WORK_DIR" \
  --build-output-dir "$BUILD_OUTPUT_DIR" \
  build \
  --source dadi-test-registry.cn-hangzhou.cr.aliyuncs.com/sample-v2/code-interpreter:v1.0.0_containerd_accelerated \
  --output code-interpreter:v1.0.0 \
  --kernel "$KERNEL" \
  --init-script-file "$INIT_SCRIPT" \
  --ready-pattern fsb-vm-code-interpreter-ready \
  --ready-timeout 10m
```

Every layer in the selected manifest must be Native OverlayBD. Turbo OCI, tar-wrapped OverlayBD, mixed Native/tar layers, empty manifests, and unknown layer media types are rejected.

### 4.2 Build from an ordinary OCI/Docker image

Point `--source` to an ordinary OCI/Docker image:

```bash
sudo "$FSB_VM" \
  --overlaybd-convertor "$CONVERTOR" \
  --build-work-dir "$BUILD_WORK_DIR" \
  --build-output-dir "$BUILD_OUTPUT_DIR" \
  build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --init-script-file "$INIT_SCRIPT" \
  --ready-pattern application-ready
```

The command:

1. resolves the tag or digest;
2. selects only the `linux/<current Go architecture>` manifest from a multi-platform index;
3. invokes the convertor with the selected immutable manifest digest;
4. forces `--no-upload --dump-manifest --reserve` to produce local Native OverlayBD data;
5. validates and imports the converted manifest, config, and layers;
6. builds rootfs/snapfiles from the converted Native layers.

Original OCI tar layers are not included in the final artifact. A convertor failure aborts the build; the command never falls back to direct OCI extraction.

### 4.3 Registry authentication

An anonymous registry requires no authentication option.

Pass credentials directly:

```bash
sudo "$FSB_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --username 'user:password'
```

The password may contain `:`; the username must not be empty. Alternatively, use a Docker config:

```bash
sudo "$FSB_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --auth-file "$DOCKER_CONFIG_FILE"
```

`--username` and `--auth-file` are mutually exclusive. For ordinary OCI input, the same credentials authenticate initial manifest resolution and convertor layer reads. The external convertor accepts credentials only through argv, so the same user or root may observe them in process information while it runs. `fsb-vm` does not print the complete command and redacts credentials from converter errors.

For a plain HTTP registry, place this global option before the subcommand:

```bash
sudo "$FSB_VM" --plain-http build ...
```

### Immutable manifest cache

`create` caches rootfs and snapfiles image manifests referenced by `@sha256:...`
under `<state-dir>/.manifest-cache/`. Later creates verify the local SHA256 and
manifest descriptors before reuse, avoiding registry manifest requests and their
authentication token requests on a hit. Entries are scoped by registry,
repository, HTTP/HTTPS, and the current credential; credentials are not stored.
With `template.reference`, the same directory also caches the resolved immutable
pair and checkpoint ownership. That addressing envelope is recorded in the sandbox's
`state.json.template_metadata`; platform metadata is read from mounted `manifest.json`.
Deleting one sandbox preserves this shared cache. Set `--manifest-cache-dir` to share it across separate sandbox state roots on one node. Fastlet uses `<StateRoot>/oci/manifest-cache`, so replacing a Pod does not require fetching cached registry manifests again. The cache evicts least recently used entries above 128 MiB or 4,096 files; credential scopes remain isolated and mutable tags still resolve remotely.

Missing or corrupt entries are fetched again and written atomically. Cache write
failures do not prevent using a verified remote manifest. Mutable tags always
resolve remotely; digest references to multi-platform indexes retain the existing
platform selection flow. Remove `.manifest-cache/` to clear cached manifests.
This is separate from OverlayBD's block cache: each sandbox still gets its own
writable upper and devices, and uncached blocks are read on demand by OverlayBD.

Fastlet reads platform metadata from `manifest.json` in the mounted snapfiles filesystem. FastPath uses the create-request bindings and never queries the OCI registry for recorded policy. Opaque `policy.json` and `vault.json` remain node-side state imports.

### 4.4 Disk size

The default rootfs size is 20 GiB. Specify another positive integer GiB value with:

```bash
sudo "$FSB_VM" build ... --disk-size-gb 30
```

When the source ext4 filesystem is larger than the target, the command shrinks the filesystem first, detaches the ublk device, changes the OverlayBD upper vsize, reattaches the device, and verifies both block-device and ext4 geometry. The target must be large enough for the actual filesystem data.

### 4.5 Local artifact layout

A successful build produces a directory such as:

```text
$BUILD_OUTPUT_DIR/application_v1/
  manifest/
    source-*.json
    application_v1_rootfs.json
    application_v1_snapfiles.json
    index.json
  blobs/sha256/
    <sha256 blobs>
```

For ordinary OCI input, `source-*.json` is the converted Native OverlayBD manifest, while `source_ref` in `index.json` retains the original user-supplied reference. The artifact is published atomically after validation. A pre-existing final directory causes the build to fail rather than overwrite it.

`build` creates local artifacts only; it does not upload them to a registry.

## 5. Publish the template to a registry

`create` expects two tags in the same registry repository:

```text
<template-id>_rootfs
<template-id>_snapfiles
```

For example, template ID `v1` requires:

```text
registry.example.com/team/application:v1_rootfs
registry.example.com/team/application:v1_snapfiles
```

The publishing process must upload every `blobs/sha256/*` object referenced by the two manifests before uploading the rootfs and snapfiles manifests under their tags. `manifest/index.json` records their filenames and digests for a publishing script.

For a plain HTTP registry, use `fsb-vm --plain-http push ...`. The setting applies to the destination and remote parent-layer reads for incremental artifacts; HTTPS remains the default.

`fsb-vm push` accepts `--artifact-dir`, `--destination registry/repository:tag`, `--metadata-file` and `--auth-file`. It publishes only `<tag>_rootfs` and `<tag>_snapfiles`, uploading rootfs first and snapfiles last. Platform metadata lives in `snapfiles/manifest.json`; only pair identity and checkpoint owner annotations live on the OCI manifest. `fsb-vm build` always generates the single `snapfiles/manifest.json`, including core VM fields, even without platform input. Use optional `--manifest-extra-file <manifest-extra.json>` to supplement guest networking, lineage and platform fields; it cannot replace the core contract. See the [shared manifest schema](artifact-manifest-reference.md). `push --metadata-file` is optional and only extracts checkpoint ownership; it cannot add files to sealed snapfiles. The returned `manifestRef` and `artifactDigest` identify the snapfiles manifest. The two images are paired by ID, so IDs must be immutable; the publisher rejects overwriting a tag with different contents. Rebuild historical OCI and S3 templates using the unified schemaVersion 1; dual-file metadata and the previous manifest layout are not supported. The OCI `SandboxTemplate` builder runs this step automatically, composing the destination from the repository-only `output.publish` and the system-assigned UUID. Explicit `--destination repository:tag` remains the standalone CLI contract. See the [publication contract](../design/adr-fsb-vm-cli.en.md#adr-012-build-artifact-layout-and-publish-contract-build-iteration) for layout and ordering.

## 6. Prepare template.json

`create` accepts one manifest file:

```json
{
  "sandbox_id": "application-demo",
  "template": {
    "repo": "registry.example.com/team/application",
    "template_id": "v1",
    "auth": {
      "dockerAuth": ""
    }
  },
  "metadata": {}
}
```

Fields:

| Field | Description |
| --- | --- |
| `sandbox_id` | Host-unique sandbox ID and state/runtime directory name |
| `template.repo` | Registry repository without a tag |
| `template.template_id` | Template ID; the tool pulls `<id>_rootfs` and `<id>_snapfiles` |
| `template.reference` | Platform `oci://repository@sha256:...` reference or logical tag; an alternative to repo/template_id or explicit rootfs/snapfiles |
| `template.auth.dockerAuth` | Docker config JSON encoded as a string; use an empty string for anonymous access |
| `metadata` | Reserved pass-through field; optional |

Fastlet uses `template.reference` with `create --prepare-only`. The CLI resolves
the pair and annotations, reuses its cache, and prepares devices without a prior
node-agent OCI metadata query. In this mode, the resolved pair/ownership envelope replaces
the input `metadata`; the existing repo/template_id input remains supported.

For a private registry, `dockerAuth` is a JSON **string**, not a nested object. Given this Docker config:

```json
{"auths":{"registry.example.com":{"auth":"BASE64_USER_COLON_PASSWORD"}}}
```

escape it in `template.json`:

```json
{
  "sandbox_id": "application-demo",
  "template": {
    "repo": "registry.example.com/team/application",
    "template_id": "v1",
    "auth": {
      "dockerAuth": "{\"auths\":{\"registry.example.com\":{\"auth\":\"BASE64_USER_COLON_PASSWORD\"}}}"
    }
  }
}
```

## 7. Run a VM

```bash
sudo "$FSB_VM" \
  --firecracker "$FIRECRACKER" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --network none
```

`--network` currently supports only `none`. `create`:

1. strictly validates and atomically claims `sandbox_id`, then writes `creating` state;
2. pulls manifests, acquires shared read-only snapfiles and a private writable rootfs device, and progressively records ownership;
3. creates `rootfs.dev` in the runtime directory and loads the snapshot with that cwd;
4. resumes the VM and changes `state.json` to `running`.

By default, it checks `/opt/fsb-vm/kernels/vmlinux-<version>` using `kernel.version` from template metadata. When the standard Builder uses `vmlinux.bin`, that literal `kernel.version` selects `vmlinux.bin` in the same directory without adding the `vmlinux-` prefix. Override the path when necessary:

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --kernel "$KERNEL"
```

For a plain HTTP registry:

```bash
sudo "$FSB_VM" \
  --plain-http \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create -f "$TEMPLATE_FILE"
```

Do not reuse a `sandbox_id` that still exists locally.

Snapfiles devices and mounts are shared by default for identical resolved layers and credential scope within the same `--state-dir` and mount namespace. Writable rootfs devices and uppers remain private. Each Sandbox keeps a durable snapfiles lease until deletion.

## 8. Inspect VMs

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  list
```

Example output:

```text
SANDBOX_ID       STATUS   PID    ROOTFS_DEV   SNAPFILES_DEV   API_SOCKET
application-demo running  12345  /dev/ublkb0  -               $RUNTIME_DIR/application-demo/api.sock
```

`SNAPFILES_DEV` is `-` for shared attachments because the Sandbox holds a lease rather than owning that device.

Status requires the recorded Firecracker `(PID, starttime, boot_id)` identity to still match; the PID number alone is insufficient. A directory whose `state.json` cannot be read is shown as `corrupt`. When using custom directories, pass the same `--state-dir` and `--runtime-dir` to `list`, `create`, and `delete`.

## 9. Delete a VM

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  delete application-demo
```

Cleanup proceeds in this order:

1. send SIGTERM to Firecracker, escalating to SIGKILL when needed;
2. delete the private rootfs ublk device;
3. remove the Sandbox’s snapfiles alias and release its shared lease;
4. remove sandbox state and runtime directories, then prune idle snapfiles entries.

If Firecracker cannot be stopped, delete does not release devices that the VMM may still hold; other safe steps are attempted where possible. Directories are removed only after private resources and the shared lease are released. Any failure retains `deleting` state and the writable upper so the same delete can be retried. Device ownership is verified by the absolute config path currently reported by ublkd, never by a reusable dev_id alone. Signals use pidfd after starttime and boot_id validation.

Up to 16 idle snapfiles entries remain warm. An entry becomes eligible for eviction after 10 idle minutes; entries with Sandbox leases are never evicted. Expiry is checked during subsequent deletes and failed-create rollbacks, without a background timer. To release unused entries immediately, run `sudo "$FSB_VM" --state-dir "$STATE_DIR" cleanup-snapfiles`.

Verify cleanup with:

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  list
sudo curl -fsS \
  --unix-socket /var/run/overlaybd-ublk/ublkd.sock \
  http://localhost/v1/list
```

## 10. Known limitations

- Only Linux host-platform selection is supported; a multi-platform source builds only the current host architecture.
- The only network provider is currently `none`.
- Turbo OCI, tar-wrapped OverlayBD, mixed layer formats, and unknown media types are unsupported.
- `build` creates local artifacts but does not push them to a registry.
- `build` does not inherit OCI Entrypoint, Cmd, or Env; the init script defines workload startup.
- create/delete state is local host state, not a distributed control-plane resource.

For build/create sequencing and artifact ownership, see the [fsb-vm design](../design/adr-fsb-vm-cli.en.md).

OCI Fastlets share host `/dev` so dynamically created ublk devices remain visible. A node ublkd container in the runtime-agent DaemonSet serves every Fastlet on that node. fsb-vm connects directly to its shared Unix socket; runtime-agent installs the tools and checks health without forwarding device requests. OCI jailers bind the configured snapshot spill directory before launch.

After interrupted checkpoint packaging, retry and staging deletion first recover pack devices by their config paths. Cleanup failures retain backing files. Standalone fsb-vm synchronizes credentials for the actual rootfs/snapfiles repositories; anonymous access replaces that repository’s old credential without deleting other repositories. Fastlets enable `--reject-credential-conflicts` for the node daemon, so differing accounts or an anonymous replacement for the same repository are rejected. This does not promise immediate revocation for already running VMs.

Build the Fastlet with `make images COMPONENT=fastlet FASTLET_IMAGE=opensandbox/fsb-fastlet:dev`, then push it and select that image in the Pool. This is a build example, not a claim that the tag is published. Fastlet uses an Ubuntu 22.04 runtime stage without the Go toolchain or OverlayBD build stage. The runtime-agent image installs fsb-vm and OverlayBD tools on the node; Fastlet mounts them read-only and retains their required system libraries and basic runtime tools. Enable OCI on both the node agent/daemon and the Pool with `FAST_SANDBOX_OCI_ENABLED=true`; see the [OCI deployment guide](overlaybd-oci-sandboxes.md) for paths, cache sizing, credentials, and upgrade requirements. On filesystems without extent seeking, upper copying scans zero blocks to preserve sparsity; scanning can take longer than reflink or extent copying.
