# OverlayBD OCI quick start

This guide uses the FastPath gRPC API to create, pause, and resume Sandboxes on an existing Firecracker cluster.

**Enable this path with a Firecracker Pool, an OCI-capable Fastlet image, and an `oci://` template.** `output.format: overlaybd` requires `output.publish: oci://...`; OverlayBD + S3 is unsupported. To retain S3-compatible storage, explicitly use `output.format: native` and rebuild the template. Old OverlayBD + S3 artifacts, including cached manifests, are rejected.

## 1. Prepare the Pool

Deploy OpenSandbox images that include OCI support, on nodes with KVM and ublk support. Replace `<version>` below with a release that includes this feature.

| Component | Image |
| --- | --- |
| Controller / FastPath | `opensandbox/fsb-controller:release-<version>` |
| Runtime agent | `opensandbox/fsb-firecracker-runtime:release-<version>` |
| Builder | `opensandbox/fsb-sandboxtemplate-builder:release-<version>` |
| Fastlet | `opensandbox/fsb-fastlet:release-<version>` |
| execd | `opensandbox/execd:release-<version>` |

Fastlet uses an Ubuntu 22.04 runtime image without the Go toolchain or bundled OverlayBD binaries. Builder and runtime-agent copy their OCI artifacts from the same `oci-tools` image. The runtime-agent image carries the node tool bundle; its init container verifies and installs an immutable release under `/opt/fast-sandbox/oci/releases/<digest>`, then atomically updates `current`. Firecracker Fastlets receive that directory read-only and call the installed tools locally.

Deploy the updated `config/dev/agent-daemonset.yaml`, using your namespace and component image names. Set `FAST_SANDBOX_OCI_ENABLED=true` on both `firecracker-agent` and `overlaybd-ublkd` to enable the node service, and on OCI Firecracker Pools to admit OCI workloads. The flag defaults to false. The tags above illustrate release naming rather than published versions. Build and publish the components with:

```bash
make images COMPONENT=oci-tools
make images COMPONENT=sandboxtemplate-builder SANDBOX_TEMPLATE_BUILDER_IMAGE=opensandbox/fsb-sandboxtemplate-builder:dev
make images COMPONENT=firecracker-runtime FIRECRACKER_RUNTIME_IMAGE=opensandbox/fsb-firecracker-runtime:dev
make images COMPONENT=janitor JANITOR_IMAGE=opensandbox/fsb-janitor:dev
make images COMPONENT=fastlet FASTLET_IMAGE=opensandbox/fsb-fastlet:dev
docker push opensandbox/fsb-sandboxtemplate-builder:dev
docker push opensandbox/fsb-firecracker-runtime:dev
docker push opensandbox/fsb-janitor:dev
docker push opensandbox/fsb-fastlet:dev
```

The default local `fast-sandbox/oci-tools:dev` image is rebuilt automatically when building Builder or runtime-agent through Make. To reuse a published artifact image, pass the same immutable reference to both builds:

```bash
make images COMPONENT=oci-tools OCI_TOOLS_IMAGE=registry.example.com/opensandbox/oci-tools:dev
docker push registry.example.com/opensandbox/oci-tools:dev
export OCI_TOOLS_IMAGE="registry.example.com/opensandbox/oci-tools@sha256:<digest>"
make images COMPONENT=sandboxtemplate-builder
make images COMPONENT=firecracker-runtime
```

`OCI_TOOLS_IMAGE` is also a Docker build argument for both consumer Dockerfiles. A custom image reference is consumed as supplied; publish a new tools image whenever fsb-vm or its native dependencies change. The artifact image is a build input, not a running service. Both consumers retain Ubuntu 22.04 and install their required system libraries.

With `FAST_SANDBOX_OCI_ENABLED=true`, Fastlet skips the XFS/reflink startup requirement and logs a warning. Set the same variable on the `firecracker-runtime` DaemonSet to relax the node readiness check, or on the standalone `scripts/firecracker-host-check.sh` invocation. Directory preparation and the node's minimum free-space check still apply. Native templates remain usable when OCI is enabled, but without reflink their rootfs copies can take longer and extend snapshot pause windows. With OCI disabled, XFS with `reflink=1` remains required.

When OCI is disabled, OCI creation is rejected with an explicit error. Resume is asynchronous: the rejection is reported in the Sandbox `status.runtime.message` and `Ready=False` condition; `GetSandbox` may report `Unavailable` while no runtime is assigned.

Select that Fastlet image in the Pool. Its Dockerfile compiles only Fastlet and sandbox-init in a Go build stage. `build/Dockerfile.oci-tools` builds fsb-vm, pinned OverlayBD tools and the userspace convertor once. Builder copies the full build toolchain; runtime-agent copies only its packaged node tools, so it does not gain unused convertor dependencies. Fastlet keeps the system libraries and network, mount, and filesystem tools needed to execute runtime operations; it neither builds nor starts ublkd.

Configure these fields on an existing Firecracker Pool, retaining its node settings, agent socket, and storage mounts. This is a configuration fragment; see the [Firecracker sample](../../config/samples/pool-firecracker.yaml) for a complete Pool.

```yaml
spec:
  runtime: firecracker
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: opensandbox/fsb-fastlet:release-<version>
        env:
        - name: FAST_SANDBOX_OCI_ENABLED
          value: "true"
```

One `overlaybd-ublkd` container in the runtime-agent DaemonSet serves all Fastlets on each node. fsb-vm connects directly over HTTP through `/run/fast-sandbox/overlaybd/ublkd.sock`; runtime-agent does not proxy device operations. The daemon and Fastlets must mount the same StateRoot at the same absolute path and share the host `/dev`. If StateRoot is customized, update the agent, daemon, Janitor `--oci-state-root`, runtime environment, and their hostPath mounts together.

StateRoot must be node-local storage, not a cache directory shared by multiple nodes. The daemon caches blocks under `<StateRoot>/overlaybd/cache`. `OVERLAYBD_CACHE_SIZE_GB` configures the node cache capacity (default 16 GiB); it is independent of VM upper layers and fsb-vm metadata caches. Fastlet removal preserves the node cache. Each VM still owns separate devices and writable layers under `<StateRoot>/oci/pods/<fastletPodUID>/sandboxes/<sandboxUID>`. Janitor revalidates Pod absence before invoking fsb-vm cleanup for that Pod's retained devices and checkpoint pack resources. Builders retain their own temporary daemon/cache.

Upgrade by draining OCI VMs from each node before replacing its daemon or switching tool releases. A daemon failure or DaemonSet Pod replacement can affect every OCI VM on that node; an agent-only container restart leaves direct fsb-vm requests independent. Remove old per-Pod cache directories only after their daemon, mounts, and VM consumers have exited. Do not delete a running daemon's cache directory or share it between daemon processes.

The examples below assume a Pool named `firecracker-pool` in namespace `fast-sandbox`, with a per-Sandbox profile of 1 CPU and 512Mi memory.

## 2. Configure registry credentials

OCI registry access supports anonymous or username/password authentication. Docker config `auth` entries are supported; identity-token-only credentials are rejected. A token supplied as a username/password credential is treated as a password. Existing public credential fields and other runtimes are unchanged.

Fastlet passes its namespace/Pool registry credentials and the OCI reference directly to `fsb-vm create --prepare-only`. The CLI resolves the pair, caches immutable manifests within the registry credential scope, and returns the metadata needed for restore validation. Fastlets share `<StateRoot>/oci/manifest-cache` on the node, independently of Pod-owned VM state, so replacing a Fastlet preserves both manifest and block caches. The manifest cache evicts least recently used entries above 128 MiB or 4,096 files; eviction only causes a registry metadata refetch. OCI creation and resume do not require a separate registry account or metadata client on the node runtime-agent. The native S3 delivery path continues to use the agent.

The current node daemon has a service-wide credential context. Use a single credential per repository across Fastlets sharing a node. fsb-vm runs with `--reject-credential-conflicts`, so a second account or anonymous replacement for an already configured repository fails explicitly rather than overriding another VM's credentials. To rotate credentials, drain the node's OCI consumers, stop the daemon, remove `/run/fast-sandbox/overlaybd/cred.json`, update Secrets, and restart. General per-device multi-account isolation requires upstream OverlayBD support and is not provided by this deployment. The socket and credential directory are root-only and shared exclusively with trusted runtime components.

For the simplest setup, use one repository for templates and checkpoints, with one account that has pull and push permissions. Set `REGISTRY_USER` and `REGISTRY_PASSWORD`, then run:

```bash
kubectl -n fast-sandbox create secret docker-registry registry-credentials \
  --docker-server=registry.example.com \
  --docker-username="$REGISTRY_USER" \
  --docker-password="$REGISTRY_PASSWORD"
```

Create this ConfigMap so the Pool uses that account to read templates and publish and restore checkpoints:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: fast-sandbox-registry
  namespace: fast-sandbox
data:
  registries.yaml: |
    registries:
      - host: registry.example.com
        repositoryPrefix: sandbox
        secretRef:
          name: registry-credentials
```

OCI templates require `output.publishSecretRef` with a non-empty Secret name and a `.dockerconfigjson` key. Missing references are rejected before a build Pod is created. The builder preserves an existing `/etc/hosts` in the source image and only supplies defaults when it is absent.

Keep the Secret, ConfigMap, Pool, and template in the same namespace. For private source or execd images, also include their credentials in the Builder's `.dockerconfigjson`.

## 3. Create a template

Save this as `template.yaml`, replace the registry address and image version, then apply it:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxTemplate
metadata:
  name: alpine-oci
  namespace: fast-sandbox
spec:
  image: alpine:3.19
  execd: opensandbox/execd:release-<version>
  kernel: vmlinux.bin
  machine:
    vcpu: "1"
    memory: 512Mi
  readiness:
    probe: tcp://127.0.0.1:44772
    warmupSeconds: 15
  output:
    rootfsSize: 2Gi
    format: overlaybd
    publish: oci://registry.example.com/sandbox/templates
    publishSecretRef:
      name: registry-credentials
```

```bash
kubectl apply -f template.yaml
kubectl -n fast-sandbox get sandboxtemplate alpine-oci -w
```

Once `status.phase` is `Succeeded`, retrieve the result:

```bash
TEMPLATE_REF=$(kubectl -n fast-sandbox get sandboxtemplate alpine-oci \
  -o jsonpath='{.status.manifestRef}')
```

`output.publish` is the destination repository only: `oci://registry/repository`. A tag such as `:alpine-v1`, a digest such as `@sha256:...`, or a malformed repository causes `InvalidOutput` before a build Pod is launched. The source `spec.image` can still include its normal image tag or digest.

`TEMPLATE_REF` should look like `oci://registry.example.com/sandbox/templates@sha256:...`. Use this immutable `status.manifestRef` when creating Sandboxes. The repository-only `output.publish` is a build destination; it is not a runnable template reference.

### Rebuilds and generated tags

Each build Pod receives a system-generated random UUID. The builder uses it as the shared prefix for the two registry tags:

```text
# First build
registry.example.com/sandbox/templates:<uuid-1>_rootfs
registry.example.com/sandbox/templates:<uuid-1>_snapfiles

# Next build, with the same output.publish
registry.example.com/sandbox/templates:<uuid-2>_rootfs
registry.example.com/sandbox/templates:<uuid-2>_snapfiles
```

Editing the Template spec, for example `spec.image` or `spec.envs`, increments its generation and starts a new build with a new UUID. Applying the same unchanged spec does not trigger another build. Reconciliation retries reuse the existing Pod and its UUID; a replacement build Pod receives a new UUID. Callers do not supply the UUID in the CR.

On success, `status.manifestRef` and `status.artifactDigest` identify the latest build's snapfiles manifest. Rebuilding leaves previously published pairs intact. Save the earlier `manifestRef` before rebuilding if you need to create VMs from that version later.

For an existing OCI Template with a tagged `output.publish`, remove the tag before its next build, for example change `oci://registry.example.com/sandbox/templates:alpine-v1` to `oci://registry.example.com/sandbox/templates`. This spec change triggers a new build. Historical pairs and their digest references remain usable. The standalone `fsb-vm push --destination repository:tag` command continues to accept an explicit tag; automatic UUID allocation belongs to the SandboxTemplate build flow.

### Registry artifact format

New publications contain exactly two image tags, following fsb-vm's existing naming rule:

```text
repository:<id>_rootfs
repository:<id>_snapfiles
```

No third metadata image is published. Both images describe their own layers in their OCI manifests. The snapfiles filesystem contains `vmstate.bin`, `memfile` and a single `manifest.json` using the [shared VM manifest schema](artifact-manifest-reference.md). fsb-vm generates the VM fields; Builder supplies optional platform and network information through `--manifest-extra-file`. No `metadata.json` is produced. Fastlet reads the shared manifest after mounting snapfiles, validates restore admission and imports verified state before data-plane readiness. Create-request bindings seed handler registration; template policy state remains authoritative when carried by the bundle. FastPath does not query the OCI registry or need registry credentials for create. Its OCI manifest carries only pairing and ownership annotations:

- `io.fast-sandbox.template.id`: the shared tag prefix used to find `<id>_rootfs` in the same repository.
- For checkpoints, `io.fast-sandbox.sandbox.uid`, `io.fast-sandbox.sandbox.namespace`, and `io.fast-sandbox.checkpoint.id`: owner and runtime snapshot ID. The latter is the runtime snapshot UUID used as the tag prefix; the control-plane checkpoint task ID is unchanged.

The publisher uploads rootfs first and snapfiles last. It rejects replacing an existing tag with different content; configure registry tag immutability as well, since the two images are paired by ID rather than a third manifest containing both digests. Use a fresh ID for every new checkpoint. A retry with identical contents is allowed.

`manifestRef` and `artifactDigest` identify the snapfiles manifest. On first resolution, fsb-vm resolves `<id>_rootfs` and caches the pair as digest references. A logical `oci://repository:<id>` input also resolves the pair; mutable tag inputs are resolved remotely on each preparation. Rebuild historical OCI templates, including three-image publications with a separate metadata image and dual-file `metadata.json`/`manifest.json` templates: fsb-vm and Fastlet now require the two-image pair and unified schemaVersion 1 `snapfiles/manifest.json`. Checkpoints and live snapshots produced by the upgraded Fastlet include this file. Deploy matching Builder/fsb-vm and Fastlet versions before publishing new templates. Existing registry artifacts are not rewritten or deleted.

## 4. Create, pause, and resume through FastPath

Run the examples in Bash from the repository root, with `grpcurl` and `jq` installed. First, forward FastPath in one terminal:

```bash
kubectl -n fast-sandbox-system port-forward svc/fast-sandbox-fastpath 9090:9090
```

In another terminal, retrieve `TEMPLATE_REF` as above, define the helper, and create a Sandbox:

```bash
fp() {
  grpcurl -plaintext -max-time 120 \
    -import-path api/proto/v2 -proto fastpath.proto \
    -d "$2" 127.0.0.1:9090 "fastpath.v2.FastPathService/$1"
}

fp CreateSandbox "{
  \"requestId\": \"oci-create-$(date +%s)-${RANDOM}\",
  \"namespace\": \"fast-sandbox\",
  \"poolRef\": \"firecracker-pool\",
  \"image\": \"$TEMPLATE_REF\",
  \"expiresAtUnixSeconds\": $(( $(date +%s) + 3600 )),
  \"completion\": \"CREATE_COMPLETION_RUNTIME_READY\"
}" > created.json

SANDBOX_REF=$(jq -c '.sandbox.identity |
  {namespacedName: {namespace: .namespace, name: .name}, expectedUid: .uid}' created.json)
```

`RuntimeReady` means the runtime is ready. To wait for aggregate readiness, use `CREATE_COMPLETION_READY` instead.

Pause, then inspect the state:

```bash
fp PauseSandbox "{\"sandbox\": $SANDBOX_REF}"
fp GetSandbox "{\"sandbox\": $SANDBOX_REF}"
```

Pause is asynchronous. **Repeat `GetSandbox` until the runtime state is `RUNTIME_STATE_PAUSED` before resuming.** While pausing or paused, `GetSandbox` returns the controller’s durable state, including during runtime release and assignment cleanup. At that point, the checkpoint is durable and the runtime has been released; assignment cleanup may still be converging.

```bash
CHECKPOINT_ID=$(fp GetSandbox "{\"sandbox\": $SANDBOX_REF}" \
  | jq -r '.sandbox.checkpoint.checkpointId')

fp ResumeSandbox "{
  \"sandbox\": $SANDBOX_REF,
  \"expectedCheckpointId\": \"$CHECKPOINT_ID\"
}"
fp GetSandbox "{\"sandbox\": $SANDBOX_REF}"
```

Wait for the runtime state to return to `RUNTIME_STATE_READY`. Resume restores the same Sandbox; a checkpoint cannot be used as a template to create another Sandbox. The Sandbox expiration time still applies while paused.

Delete the test Sandbox when finished:

```bash
fp DeleteSandbox "{\"sandbox\": $SANDBOX_REF}"
```

## 5. Template and checkpoint destinations

- **Template:** set `SandboxTemplate.spec.output.publish` to `oci://registry/repository`, without a tag or digest. The system generates a UUID pair ID for each build Pod.
- **Checkpoint:** defaults to the repository of the current restore source, which is the template repository on the first pause. The system generates the checkpoint tag.

For a separate checkpoint repository, add this environment variable to the Pool's Fastlet container:

```yaml
env:
- name: FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY
  value: registry.example.com/sandbox/checkpoints
```

Use `registry/repository` without `oci://`, a tag, or a digest. The account must be able to read both repositories and push to the checkpoint repository. For separate read/write accounts, see [Private registries](private-registries.md) and [Sandbox snapshots](sandbox-snapshots.md).

## Recorded policy and state in snapfiles

OCI pause checkpoints and live snapshots use the same recorded-state layout as
native snapshots. Fastlet exports the egress-owned `policy.json` record without
parsing or rewriting it. The snapfiles filesystem carries the unified `manifest.json`, optional opaque `policy.json` and `vault.json`, `vmstate.bin` and `memfile`. `platform.stateFiles` records each state file's digest and size exactly once for verified Fastlet imports. Rootfs remains the other OCI image: no third
artifact or separate policy format is introduced.

Pause exports checkpoint state before releasing the runtime. A live template
snapshot uses the handler's seal-free export capability so the source remains
mutable; a declared handler that lacks this capability causes the snapshot to
fail, matching the native path. On resume, Fastlet verifies the bundled state
and imports it before data-plane readiness. A new VM created from a snapshot
lands the verified state for its first handler registration, under the new VM's
identity. The recorded policy takes precedence over the create-time binding
seed; absent or invalid state follows the same observable fallback as native
restore. As in the native path, create-request `actionBindings` provide the bootstrap seed; the manifest carries no action-binding channel. Declare the handler and pass its seed when cloning a snapshot. OCI snapfiles mounts are private to the Fastlet container.

Within each Fastlet, Sandboxes using the same resolved snapfiles layers and registry credential scope share one read-only device and mount. Each Sandbox retains a durable reference until teardown; writable rootfs devices and uppers remain private. Up to 16 idle snapfiles entries stay warm for 10 minutes and are pruned during subsequent Sandbox deletions. Janitor releases unused entries after their owning Fastlet Pod disappears. Upgrade node Janitor together with fsb-vm and Fastlet to reclaim orphan shared devices.

Standalone `fsb-vm create` shares read-only snapfiles by default within its `--state-dir` and mount namespace; `cleanup-snapfiles` releases entries with no Sandbox references. Idle expiry is checked during subsequent deletes and failed-create rollbacks, without a background timer.

For handler import, Fastlet copies the verified opaque record to a temporary
directory under the shared `<stateRoot>/sandboxes/<uid>/`, then removes that
directory after the handler returns. The egress container must mount the same
state root at the same absolute path. Fastlet never unwraps `policy.json`: the verified
record imported by Fastlet remains authoritative over those bootstrap inputs.

## Usage notes

- Match the Pool's CPU and memory profile to the template. After updating a template, create instances with its new `status.manifestRef`.
- Add `TEMPLATE_REF` to the Pool's `warmImages` for prewarming. This does not mean all data blocks are cached.
- To measure warm creation latency, fix the template and Fastlet, warm them up, and use a gRPC client that reuses its connection. The `grpcurl` commands above open a new connection for each call and are intended for functional checks.
- See the [FastPath integration guide](pause-resume-snapshot-integration.md) for the full pause/resume state model and concurrency fences.

## Live Snapshot API

Call the gRPC API directly; it creates and drives the `SandboxSnapshot` CR internally. Creating the CR yourself is an alternative entry point to the same flow.

`CreateSandboxSnapshot` supports running OCI Sandboxes. Omit `template_name` on this path: Fastlet generates a random UUID `snapshot_id` and publishes `<snapshot_id>_rootfs` and `<snapshot_id>_snapfiles` to `FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY`, or to the source repository when that setting is absent. No third OCI artifact or name index is written.

```text
CreateSandboxSnapshot(request_id="capture-001", sandbox=<running sandbox>)
GetSandboxSnapshot(snapshot=<capture-001>)  # wait for Succeeded
CreateSandbox(image=<snapshot.manifest_ref>, pool_ref=<firecracker pool>)
```

`manifest_ref` is the immutable `oci://repository@sha256:...` snapfiles reference. Use it to create one or more new Sandboxes. The API returns `snapshot_id` after Fastlet admits the task. The source VM resumes after the dump and continues running through publication. Its disk and memory state are captured together; inherited rootfs layers are reused and one new layer carries the captured writes.

The byte format is shared with pause checkpoints. Live template snapshots omit checkpoint-owner annotations and instance-private vault data, allowing a new Sandbox UID to boot from them. Pause checkpoints retain their owner annotations and remain restricted to the original Sandbox. `DeleteSandboxSnapshot` (or deleting the Snapshot CR) deletes the published rootfs and snapfiles manifests before removing the CR. The controller uses the source Pool's compiled registry credentials even if the source VM is gone. Keep those credentials available until deletion finishes. Registry deletion permission is required; a failed deletion keeps the CR terminating and is retried. Both image manifests carry their UUID pair ID, so snapshots with identical layer contents can still be deleted independently. Shared layer blobs are not deleted directly: registry garbage collection reclaims blobs no longer referenced by any image. Before allowing those blobs to be reclaimed, delete VMs still using this snapshot or move them to an independently published checkpoint: running OCI VMs can still fetch uncached disk or memory blocks.

Native + S3 snapshots keep their existing `template_name` requirement and image-index behavior. OCI snapshots require OCI support to be enabled and registry credentials with permission to read the parent layers and publish to the destination repository.
