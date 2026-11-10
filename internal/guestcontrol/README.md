# Experimental Guest filesystem control

This package and `cmd/fsb-guest-control` implement the Guest-control portion of FSEP-0001. The builder injects the helper unconditionally into OverlayBD templates, and the internal Fastlet RootfsOnly capture path uses it at capture time. Production enablement requires qualified templates, node binaries, and filesystem writer coverage for each supported runtime configuration.

## Components

- `Manager` serializes filesystem protection within one agent epoch. A caller explicitly cancels with `Thaw`; cancellation cannot interrupt an ioctl already executing. Terminal intent and completed cleanup are separate states. Failed freeze/thaw quarantines the manager for service-level recovery.
- `Service` accepts bounded newline-delimited JSON requests: `capabilities`, `freeze`, `status`, and `thaw`. Requests identify protocol `fsb-freeze-v1`; mutations and status also carry the discovered agent epoch, a monotonic generation, and an operation ID. Freeze supplies `leaseMillis`. Conflicting identities and stale epochs are rejected. One operation is permitted per connection.
- `ServeVSock` accepts only host CID 2, caps concurrent connections, and applies I/O deadlines. The host Unix socket must remain private to the runtime. This boundary does not defend against guest root.
- `Client` speaks Firecracker's host Unix-socket `CONNECT <port>` handshake, honors context deadlines/cancellation, and never retries mutations automatically. A lost reply after sending freeze is ambiguous. Inspect both response state and `cleanupPending`; the `failure` field reports asynchronous ioctl errors.
- `FreezeAndWait` and `ThawAndWait` poll completion without retrying mutations. Failed acquisition still requires explicit cleanup with the same identity and a fresh bounded recovery context. Thaw must follow VM Resume and termination of capture work; these helpers do not provide lifecycle fencing or a storage barrier.
- `LinuxFilesystem` supports ext4 on Linux amd64/arm64, requires an exact mount point, and keeps its descriptor open for pending operations.

## Experimental binary

Build for the intended guest architecture:

```bash
GOOS=linux GOARCH=arm64 go build -o fsb-guest-control ./cmd/fsb-guest-control
```

On a disposable guest with a configured vsock device, run as root with an explicit mount and private port. Template builds and the Fastlet coordinator use port 1234; the standalone helper still requires an explicit mount:

```bash
./fsb-guest-control --mount /mnt/test-ext4 --port 1234 --max-lease 30s
```

`/run` must be tmpfs. The service leaves `/run/fsb-guest-control.started` as a fail-closed crash marker; automatic restart refuses to proceed. Do not remove it until any outstanding freeze has been reconciled and the prior process/workers are gone. Rebooting a disposable test guest clears tmpfs. The process does not yet implement graceful shutdown recovery; termination can leave a filesystem frozen. The host must treat helper death as loss of capture capability, not spawn a replacement and continue.

The opt-in Fastlet coordinator binds the runtime owner, VMM incarnation, block device and OverlayBD config, persists capture intent, and reconciles unfinished operations before normal recovery. Helper supervision and full production-template qualification remain deployment requirements. A capability declaration alone does not qualify a node.

## Validation

```bash
go test -race ./internal/guestcontrol
go vet ./internal/guestcontrol
```

Tests cover cancellation/late success, lease expiry, protocol identity fences, malformed requests, and the host-side handshake using a local Unix socket. They do not establish real vsock/KVM/OverlayBD behavior.

An opt-in Linux test creates a dedicated loopback ext4 mount, verifies that a write blocks during freeze and resumes after thaw, and rejects subdirectory targets. It requires root, mount/loop privileges, and `mkfs.ext4`; it never targets the machine's root filesystem:

```bash
sudo env FSB_TEST_GUEST_FREEZE=1 go test ./internal/guestcontrol -run '^TestLinuxFreezeBlocksWritesAndThaws$' -v -count=1 -timeout=30s
```

Run it only in a disposable Linux test VM. It is skipped by default. Cross-compilation of the test or binary is not execution of this test.

### Real VMM continuation check

Before testing freeze across Pause, boot a disposable Firecracker VM serving this protocol and run the following on its Linux host (or cross-compile the Firecracker package test binary and pass the equivalent `-test.run` flag). Supply that VM's private sockets and the helper's port:

```bash
FSB_TEST_VSOCK_RESUME=1 \
FSB_TEST_API_SOCKET=/path/to/disposable/api.sock \
FSB_TEST_VSOCK_SOCKET=/path/to/disposable/vsock \
FSB_TEST_GUEST_PORT=1234 \
go test ./internal/runtime/firecracker -run '^TestGuestControlPlainResume$' -v -count=1 -timeout=45s
```

This opt-in test checks baseline connectivity, then three plain Pause/Resume cycles with new control connections and an unchanged helper epoch. It never freezes a filesystem or invokes snapshot/create. An ambiguous lifecycle reply triggers a bounded best-effort Resume; the operator owns VM teardown. Success qualifies only this control-channel path, not storage consistency or Full restore. Failure after a successful Resume reply is a blocker for vsock-based thaw, not a reason to retry freeze or silently create a Full snapshot. Firecracker v1.16.1 on the tested ARM64 KVM setup fails this check because plain Resume waits for a vsock reset acknowledgement without supplying the reset event.

### Host block flush is not VMM drain

The Firecracker package's Linux-only `flushCapturedBlock` validates an already open block descriptor and propagates `fsync` errors. The opt-in coordinator calls it after Guest freeze and VM Pause. It neither pauses the VM nor drains VMM queues. The coordinator must retain the source fence and device lease until the syscall actually returns, including after cancellation. Sync and Async use the qualified Guest filesystem freeze boundary for covered writes; host fsync cannot flush work that has not reached the host block device. Raw block writes, swap, other mounts, and external writers must be excluded or separately protected. The Guest helper must remain independent of workload io_uring completion processing, and freeze must finish before Pause. OverlayBD builds that swallow upper-file synchronization errors are not qualified.

The opt-in `TestFlushCapturedBlockIntegration` accepts `FSB_TEST_FLUSH_BLOCK_DEVICE` for a disposable lab device. With controlled lower-layer fault injection, `FSB_TEST_FLUSH_EXPECT_ERROR=1` requires an error to reach the Go caller. A successful test is not permission to enable RootfsOnly capture.

### Shared capture transaction

`rootfsCapture` now handles both Sync and Async through the same filesystem boundary. The lease verifier must supply the actual engine and establish exclusive ext4 writer coverage; this is not a caller-provided capability flag. The Fastlet CreateSnapshot worker now dispatches explicit `RootfsOnly` requests to the durable coordinator; an omitted mode remains `Full`. This is the internal Fastlet protocol, not an extension to the public CRD or OpenSandbox API.

The transaction rejects mismatched Guest identities, incomplete freeze/cleanup states, insufficient lease capability, and cancellation during qualification. It waits for synchronous flush/copy to return before recovery. Identity changes, ambiguous Pause results, recovery failure, and panics retain the fence through quarantine. Panics are rethrown after recovery. A failure never authorizes publishing the staging directory; its owner must clean it up.

`TestRootfsCaptureTransactionIntegration` is an opt-in Linux test for a disposable, single-drive VM with an independently running Guest helper and an OverlayBD upper. The external harness owns device creation, teardown, and the exclusive-writer assumption. The test checks the actual configured engine/device, invokes the Go transaction with real vsock and block fsync, and covers successful capture, injected flush/copy errors, cancellation, and a successful follow-up capture. Its laboratory lease is not a production durable coordinator. It does not publish OCI artifacts or validate cold boot.


### Opt-in Fastlet integration

The internal request uses `snapshot.mode: "RootfsOnly"` and `snapshot.kind: "Template"`. Checkpoints and non-OCI sources are rejected. The template must declare validated `guestControl` on vsock port 1234 and a cold-boot kernel command line. Nodes need the matching kernel file, `fsb-vm`, and OverlayBD tools and daemon at the existing OCI runtime paths. Direct (non-jailer) launches also require `nsenter` to enter the allocated network namespace.

The helper reports `helperVersion` as the SHA-256 fingerprint of its running executable, independently of the `fsb-freeze-v1` wire protocol. The Builder records the value returned by the successfully probed helper; capture requires the live helper to match that declaration. An older helper without this field, or a template with the old hard-coded `"1"` declaration, must be rebuilt before RootfsOnly capture. Ordinary Full restore remains available.

The Builder and Driver cold-boot paths copy the kernel into a private boot file and record its digest. Full restoration inherits `kernel.sha256` from the artifact because its kernel is already in RAM; a matching host installation cannot qualify a different running kernel. The Driver persists `bootKernelSHA256` and carries it into subsequent Full and RootfsOnly artifacts. Capture rejects legacy live instances without that boot provenance; rebuild legacy Full templates and recreate those instances. Installing a different host kernel after boot does not change the source VM's qualification.

Set `FAST_SANDBOX_ROOTFS_QUALIFICATION` to an absolute, operator-owned JSON file:

```json
{
  "firecrackerSHA256": "<sha256 of the qualified Firecracker 1.16.1 executable>",
  "kernelSHA256": "<sha256 of the qualified guest kernel>",
  "overlaybdSHA256": "<sha256 of the qualified overlaybd-ublkd executable>",
  "storageRevision": "<qualified source revision and patch identity>",
  "exclusiveExt4": true
}
```

The VMM and kernel digests are checked locally. For the node OverlayBD daemon, set `OVERLAYBD_ROOTFS_QUALIFIED_SHA256` on the runtime-agent to the same digest as `overlaybdSHA256`. The node launcher uses the bundled `fsb-vm qualify-backend` command to hash the actual socket peer executable and atomically write `ublkd.sock.qualification.json`. It removes the record before daemon startup and on shutdown while holding the node daemon lock. Fastlet validates the root-owned record, host boot ID, socket inode/ctime, expected digest, and service reachability without sharing the host PID namespace. A missing, stale, writable-by-others, or mismatched record disables RootfsOnly capture. The shared socket directory and its ancestors must remain controlled by the node operator; this is local deployment attestation, not protection against a compromised node root. For manually launched laboratory daemons, run `fsb-vm --ublkd-socket <socket> qualify-backend --sha256 <qualified-digest>` in the daemon's PID namespace and revoke the record before replacing the daemon.

The digests are checked against actual service executables and the configured kernel artifact. `exclusiveExt4` is an operator-enforced deployment condition, not a security boundary or an automatic proof that an arbitrary Guest cannot write raw blocks. Only qualified ext4-only writers are covered; raw block access, swap, extra writable drives and external freeze/thaw must be excluded. No profile means capture is disabled. The current qualification requires the tested 1.16.1 plain-Resume/vsock patch and an OverlayBD build that propagates synchronization failures.

`TestRootfsFastletChainE2E` (Linux, build tag `firecracker`) exercises Fastlet manager admission and worker dispatch, real KVM/OverlayBD capture, a temporary local Registry, a second cold boot with a higher disk boot counter, independent network namespaces and ping, and recovery through a fresh Driver from a paused capture journal. It does not run a Kubernetes control plane, HTTP ingress, production application image, actual Fastlet SIGKILL, or Intel/AMD migration. The test uses a previously qualified rootfs seed bundle through `FSB_TEST_FASTLET_SEED`, plus `FC_BINARY`, `FC_KERNEL` and the node qualification profile. It does not install node prerequisites.


### Recovery and publication ownership

A positively absent source PID takes the aborted-capture path before Guest recovery. The coordinator persists `aborted`, clears the stale PID in terminal runtime metadata, discards the incomplete staging directory, and then uses normal owner retirement and resource cleanup. An API timeout, unreadable `/proc`, or a still-present/reused PID is not proof of death and remains fenced. Cleanup retries never publish an aborted capture.

After successful capture, source recovery, and state-file staging, the coordinator fixes the publication destination and writes `.pack-owner.json` while holding the lifecycle lock. It then releases the source lock before pack/upload. Publication consumes its private staging directory, so source deletion does not require loading `meta.json` again. The CLI and Driver share the same declarative cold-boot device configuration, including explicit `Writeback` cache mode.

### Optional layer compaction

The capture preflight budgets upper copies and mandatory packing; it does not reserve a download of every inherited layer. Pack completes mandatory snapfiles before optional squash. Before fetching a squash tail it checks free space on the artifact filesystem for missing compressed inputs, two virtual-disk-sized outputs with encoding headroom, and metadata overhead. This is a conservative preflight estimate, not a quota reservation against unrelated concurrent writers. Insufficient capacity skips compaction and publishes the original layer chain. Fetch/merge failure removes newly downloaded inputs; cleanup failure aborts publication instead of silently stranding files. Existing local blobs are preserved on failure.
