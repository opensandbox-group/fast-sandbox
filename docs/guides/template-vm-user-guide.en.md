# template-vm CLI user guide

[中文版](template-vm-user-guide.md)

`template-vm` directly controls OverlayBD ublkd and Firecracker on one host. It can:

1. build local template artifacts from an ordinary OCI/Docker image or a Native OverlayBD image;
2. restore a rootfs/snapfiles template pair from a registry as a running Firecracker VM;
3. inspect and delete local VMs and their resources.

The tool bypasses the fast-sandbox control plane and is intended for development, validation, and host-side integration. Most operations require root privileges.

All commands in this guide use the fast-sandbox project as their root. Set these environment variables first:

```bash
cd ./fast-sandbox
export FAST_SANDBOX_ROOT="$PWD"
export TEMPLATE_VM="$FAST_SANDBOX_ROOT/bin/template-vm"
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/../accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
export TEMPLATE_VM_ASSETS="$FAST_SANDBOX_ROOT/.template-vm"
export FIRECRACKER="$TEMPLATE_VM_ASSETS/firecracker"
export KERNEL="$TEMPLATE_VM_ASSETS/vmlinux-6.1.176"
export INIT_SCRIPT="$TEMPLATE_VM_ASSETS/init.sh"
export TEMPLATE_FILE="$TEMPLATE_VM_ASSETS/template.json"
export DOCKER_CONFIG_FILE="$TEMPLATE_VM_ASSETS/docker-config.json"
export BUILD_WORK_DIR="$TEMPLATE_VM_ASSETS/builds"
export BUILD_OUTPUT_DIR="$TEMPLATE_VM_ASSETS/artifacts"
export STATE_DIR="$TEMPLATE_VM_ASSETS/state"
export RUNTIME_DIR="$TEMPLATE_VM_ASSETS/runtime"
mkdir -p "$TEMPLATE_VM_ASSETS"
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
make template-vm-convertor
```

To use a different checkout under the fast-sandbox root:

```bash
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/third_party/accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
make template-vm-convertor \
  ACCELERATED_CONTAINER_IMAGE_DIR="$ACCELERATED_CONTAINER_IMAGE_DIR"
```

The convertor is used only for ordinary OCI/Docker inputs. Native OverlayBD inputs do not require it. `template-vm` always invokes the convertor with `--no-upload`.

## 2. Build the CLI

From the fast-sandbox repository root:

```bash
go test ./cmd/template-vm/
go vet ./cmd/template-vm/
make template-vm
```

The output is:

```text
$FAST_SANDBOX_ROOT/bin/template-vm
```

Alternatively, build it directly:

```bash
go build -o "$TEMPLATE_VM" ./cmd/template-vm/
```

Inspect the command help:

```bash
"$TEMPLATE_VM" --help
"$TEMPLATE_VM" build --help
"$TEMPLATE_VM" create --help
"$TEMPLATE_VM" list --help
"$TEMPLATE_VM" delete --help
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
| `--kernel-dir` | `/opt/template-vm/kernels` | Default directory for `vmlinux-<version>` during create |
| `--state-dir` | `/var/lib/template-vm/sandboxes` | Persistent sandbox state root |
| `--runtime-dir` | `/run/template-vm/sandboxes` | API socket and serial-log root |
| `--firecracker` | `firecracker` | Firecracker executable |
| `--overlaybd-convertor` | `/opt/overlaybd/snapshotter/convertor` | OCI userspace convertor |
| `--build-work-dir` | `/var/lib/template-vm/builds` | Build scratch root |
| `--build-output-dir` | `/var/lib/template-vm/artifacts` | Local artifact root |
| `--plain-http` | `false` | Use plain HTTP for registry requests |

Place global options before the subcommand, for example:

```bash
sudo "$TEMPLATE_VM" --plain-http --state-dir "$STATE_DIR" list
```

## 4. Build a template

Minimal invocation:

```bash
sudo "$TEMPLATE_VM" build \
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
| `--init-script-file` | built-in Redis init | Script injected into the guest and run as PID 1 |
| `--ready-pattern` | `Ready to accept connections` | Serial-log text that marks workload readiness |
| `--ready-timeout` | `3m` | Readiness timeout |
| `--auth-file` | empty | Docker config JSON credential file |
| `--username` | empty | Registry credentials as `username:password` |
| `--keep-work-dir` | `false` | Keep build scratch after success |
| `--no-compress` | `false` | Disable zfile compression when committing layers |

`build` does not execute the source image's OCI Entrypoint, Cmd, or Env automatically. For non-Redis workloads, provide `--init-script-file` and make the script print a stable string matching `--ready-pattern` only after the service is ready.

### 4.1 Build from a Native OverlayBD image

The following public test image is Native OverlayBD. The build downloads its Native layers directly and bypasses the convertor:

```bash
sudo "$TEMPLATE_VM" \
  --firecracker "$FIRECRACKER" \
  --build-work-dir "$BUILD_WORK_DIR" \
  --build-output-dir "$BUILD_OUTPUT_DIR" \
  build \
  --source dadi-test-registry.cn-hangzhou.cr.aliyuncs.com/sample-v2/code-interpreter:v1.0.0_containerd_accelerated \
  --output code-interpreter:v1.0.0 \
  --kernel "$KERNEL" \
  --init-script-file "$INIT_SCRIPT" \
  --ready-pattern template-vm-code-interpreter-ready \
  --ready-timeout 10m
```

Every layer in the selected manifest must be Native OverlayBD. Turbo OCI, tar-wrapped OverlayBD, mixed Native/tar layers, empty manifests, and unknown layer media types are rejected.

### 4.2 Build from an ordinary OCI/Docker image

Point `--source` to an ordinary OCI/Docker image:

```bash
sudo "$TEMPLATE_VM" \
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
sudo "$TEMPLATE_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --username 'user:password'
```

The password may contain `:`; the username must not be empty. Alternatively, use a Docker config:

```bash
sudo "$TEMPLATE_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --auth-file "$DOCKER_CONFIG_FILE"
```

`--username` and `--auth-file` are mutually exclusive. For ordinary OCI input, the same credentials authenticate initial manifest resolution and convertor layer reads. The external convertor accepts credentials only through argv, so the same user or root may observe them in process information while it runs. `template-vm` does not print the complete command and redacts credentials from converter errors.

For a plain HTTP registry, place this global option before the subcommand:

```bash
sudo "$TEMPLATE_VM" --plain-http build ...
```

### 4.4 Disk size

The default rootfs size is 20 GiB. Specify another positive integer GiB value with:

```bash
sudo "$TEMPLATE_VM" build ... --disk-size-gb 30
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

The CLI does not currently have a `push` subcommand. See [template-vm manual E2E validation](template-vm-manual-e2e.md#7-将本地产物上传到临时-registry) for complete local-registry upload commands. A production publishing process must preserve the OCI manifest and OverlayBD layer media types.

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
| `template.auth.dockerAuth` | Docker config JSON encoded as a string; use an empty string for anonymous access |
| `metadata` | Reserved pass-through field; optional |

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
sudo "$TEMPLATE_VM" \
  --firecracker "$FIRECRACKER" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --network none
```

`--network` currently supports only `none`. `create`:

1. pulls the rootfs and snapfiles manifests;
2. creates and mounts the ublk devices;
3. loads the Firecracker snapshot from snapfiles;
4. redirects the restored rootfs drive to the new device;
5. resumes the VM and writes `state.json`.

By default, it checks `/opt/template-vm/kernels/vmlinux-<version>` using `kernel_version` from template metadata. Override the path when necessary:

```bash
sudo "$TEMPLATE_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --kernel "$KERNEL"
```

For a plain HTTP registry:

```bash
sudo "$TEMPLATE_VM" \
  --plain-http \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create -f "$TEMPLATE_FILE"
```

Do not reuse a `sandbox_id` that still exists locally.

## 8. Inspect VMs

```bash
sudo "$TEMPLATE_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  list
```

Example output:

```text
SANDBOX_ID       STATUS   PID    ROOTFS_DEV   SNAPFILES_DEV   API_SOCKET
application-demo running  12345  /dev/ublkb0  /dev/ublkb1     $RUNTIME_DIR/application-demo/api.sock
```

Status is derived from whether the recorded Firecracker PID is alive. A directory whose `state.json` cannot be read is shown as `corrupt`. When using custom directories, pass the same `--state-dir` and `--runtime-dir` to `list`, `create`, and `delete`.

## 9. Delete a VM

```bash
sudo "$TEMPLATE_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  delete application-demo
```

Cleanup proceeds in this order:

1. send SIGTERM to Firecracker, escalating to SIGKILL when needed;
2. unmount snapfiles;
3. delete the rootfs and snapfiles ublk devices;
4. remove sandbox state and runtime directories.

Each step is best-effort. A failure does not prevent later cleanup steps, but the command returns a nonzero error and writes warnings to stderr. Do not delete the state directory manually: `state.json` contains the PID, mount point, and ublk device IDs required for cleanup.

Verify cleanup with:

```bash
sudo "$TEMPLATE_VM" \
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

For privileged E2E steps, artifact digest verification, local registry publishing, and orphan-resource troubleshooting, see [template-vm manual E2E validation](template-vm-manual-e2e.md).
