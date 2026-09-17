#!/usr/bin/env bash
# firecracker-chain-e2e.sh — full-chain E2E (stage-1 closeout of the
# Firecracker on-demand loading design): builder publish -> real MinIO
# -> runtime-agent pull -> driver golden restore -> guest reachability ->
# idempotency + cleanup. No component is faked: the builder image, the MinIO
# store, the firecracker-runtime binary, and the Firecracker driver
# (via its Go E2E suite) all run for real.
#
# Scenarios:
#   A. pull chain — the builder publishes a golden snapshot set (index +
#      SHA256SUMS + digest-addressed build); the runtime-agent pulls it over
#      the wire (path-style SigV4 against MinIO) through its UDS API;
#   B. restore — the driver restores sandboxes from the pulled artifacts
#      (FC_SKIP_PREP: no self-bootstrap, the builder output IS the golden
#      set) and the guest is reachable at the baked address;
#   C. full chain — A+B plus idempotency (re-PinImage pulls nothing),
#      lease lifecycle (LeaseDevices/ReleaseDevices/ListLeases), and driver
#      delete -> agent unpin through the UDS API.
#
# Requirements: Linux x86_64, root (netns/tap/iptables setup; the script
# re-invokes itself with sudo), /dev/kvm, /dev/net/tun, docker, go, curl, jq.
# Same reference host as firecracker-e2e.sh and sandboxtemplate-e2e.sh.
#
# Host impact: the driver E2E creates a private 172.30.0.0/24 bridge plus one
# MASQUERADE rule and host ip_forward; all restored automatically on exit.
# MinIO runs as a docker container (removed on exit). The builder creates and
# deletes its own build tap.
#
# Overrides (environment):
#   CHAIN_SOURCE_IMAGE   OCI image converted by the builder (default alpine:3.19)
#   CHAIN_IMAGE          image reference addressed end to end (default chain-test:v1)
#   CHAIN_MACHINE_VCPU   builder machine vcpu (default 1; must fit the E2E spec)
#   CHAIN_MACHINE_MEM    builder machine memory (default 512Mi)
#   CHAIN_ROOTFS_SIZE    builder rootfsSize (default 10Gi)
#   MINIO_IMAGE          minio/minio image tag (default minio/minio:latest)
#   MINIO_PORT           MinIO API port on the host (default 9000)
#   MINIO_AK / MINIO_SK  MinIO root credentials (default chain-test / chain-test-secret)
#   WORK                 workspace, default $PWD/.firecracker-chain-e2e
#   SANDBOX_TEMPLATE_BUILDER_IMAGE  builder image name (default sandboxtemplate-builder:chain-e2e)
#   plus the driver E2E overrides FC_VERSION/FC_BINARY/FC_JAILER/FC_KERNEL/FC_ROOTFS/FC_STATE_ROOT.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.firecracker-chain-e2e}"
BIN_DIR="$WORK/bin"
ART_DIR="$WORK/artifacts"

CHAIN_SOURCE_IMAGE="${CHAIN_SOURCE_IMAGE:-alpine:3.19}"
CHAIN_IMAGE="${CHAIN_IMAGE:-chain-test:v1}"
CHAIN_MACHINE_VCPU="${CHAIN_MACHINE_VCPU:-1}"
CHAIN_MACHINE_MEM="${CHAIN_MACHINE_MEM:-512Mi}"
CHAIN_ROOTFS_SIZE="${CHAIN_ROOTFS_SIZE:-2Gi}"
# The execd image baked into the golden snapshot: the builder injects its
# runtime files, the guest init starts it, and scenario B probes GET /ping
# as the VM-running evidence. Override with CHAIN_EXECD if needed.
CHAIN_EXECD="${CHAIN_EXECD:-opensandbox/execd:1.1.0}"
MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:latest}"
MINIO_PORT="${MINIO_PORT:-9000}"
MINIO_AK="${MINIO_AK:-chain-test}"
MINIO_SK="${MINIO_SK:-chain-test-secret}"
MINIO_BUCKET="sandbox-images"
MINIO_CONTAINER="chain-e2e-minio"
BUILDER_IMAGE="${SANDBOX_TEMPLATE_BUILDER_IMAGE:-sandboxtemplate-builder:chain-e2e}"

FC_VERSION="${FC_VERSION:-v1.16.1}"
FC_BINARY="${FC_BINARY:-$BIN_DIR/firecracker}"
FC_JAILER="${FC_JAILER:-$BIN_DIR/jailer}"
FC_KERNEL="${FC_KERNEL:-$ART_DIR/vmlinux.bin}"
FC_ROOTFS="${FC_ROOTFS:-$ART_DIR/bionic.rootfs.ext4}"
FC_STATE_ROOT="${FC_STATE_ROOT:-$WORK/state-root}"
AGENT_SOCKET="$WORK/runtime.sock"
AGENT_BIN="$WORK/firecracker-runtime"
REGISTRY_FILE="$WORK/registry.json"
# The agent reads the platform artifact store from the mounted ConfigMap
# projection (fast-sandbox-artifact-store); this e2e runs the agent as a host
# process, so it writes the same files the kubelet projection would.
ARTIFACT_STORE_DIR="/etc/fast-sandbox/artifact-store"
_artifact_store_dir_created="no"
# The agent cache and the driver E2E must share one state root; following
# FC_STATE_ROOT lets the operator point both at a reflink-capable mount
# (scripts/firecracker-xfs-stateroot.sh).
STATE_ROOT_DIR="$FC_STATE_ROOT"
MINIO_DATA="$WORK/minio-data"
AGENT_PID=""

STORE_ROOT="s3://$MINIO_BUCKET/publish"
MINIO_ENDPOINT="http://127.0.0.1:$MINIO_PORT"

PRIVATE_CIDR="172.30.0.0/24"
BRIDGE_NAME="fsb0"

log() { printf '\033[1;34m[chain-e2e]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[chain-e2e] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
pass() { printf '\033[1;32m[chain-e2e] PASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[chain-e2e] FAIL\033[0m %s\n' "$*" >&2; exit 1; }

# --- host state (shared with the driver E2E: bridge, MASQUERADE, forward) ----
fsb_netns_pattern='^fsb[0-9a-f]{32,}$'
fsb0_dev_pattern='^(fc|fh)[0-9a-f]{10,}$'
PRE_RUN_NETNS="$WORK/pre-run-netns"

is_live_netns() { ip netns list 2>/dev/null | awk '{print $1}' | grep -qx "$1"; }

kill_firecracker_vms() {
    for pid in $(pgrep -f "firecracker .*--id e2e-sandbox-" 2>/dev/null || true); do
        kill "$pid" 2>/dev/null || true
    done
    sleep 1
}

purge_fsb_resources() {
    kill_firecracker_vms
    for path in /var/run/netns/fsb*; do
        [[ -e "$path" ]] || continue
        name="$(basename "$path")"
        [[ "$name" =~ $fsb_netns_pattern ]] || continue
        if is_live_netns "$name"; then
            for attempt in 1 2 3; do
                ip netns del "$name" 2>/dev/null && break
                sleep 0.2
            done
        fi
        [[ -e "$path" ]] && rm -f "$path"
    done
    for dev in $(ip -o link show master "$BRIDGE_NAME" 2>/dev/null | awk -F'[ :]+' '{print $2}' | sort -u); do
        [[ "$dev" =~ $fsb0_dev_pattern ]] || [[ "$dev" =~ ^fc-[0-9a-f]{10}$ ]] || continue
        ip link del "$dev" 2>/dev/null || true
    done
}

purge_jail_dirs() {
    # Jailer chroot roots under the StateRoot: <base>/<exec-basename>/<id>/.
    local base="${FC_STATE_ROOT}/jails"
    [[ -d "$base" ]] || return 0
    for jail in "$base"/firecracker/e2e-*/; do
        [[ -d "$jail" ]] || continue
        rm -rf "$jail"
        log "removed stale jail $jail"
    done
}

# Defaults for the pre-run host state, so cleanup is safe even when the
# failure happened before record_host_state ran.
_bridge_before="no"
_masq_before="no"
_forward_before="1"

record_host_state() {
    _forward_before="$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 1)"
    _bridge_before="no"
    if ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then _bridge_before="yes"; fi
    _masq_before="no"
    if iptables -t nat -C POSTROUTING -s "$PRIVATE_CIDR" ! -d "$PRIVATE_CIDR" -j MASQUERADE 2>/dev/null; then _masq_before="yes"; fi
}

restore_host_state() {
    if [[ "$_bridge_before" == "no" ]] && ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then
        ip link del "$BRIDGE_NAME" 2>/dev/null || true
    fi
    if [[ "$_masq_before" == "no" ]]; then
        iptables -t nat -D POSTROUTING -s "$PRIVATE_CIDR" ! -d "$PRIVATE_CIDR" -j MASQUERADE 2>/dev/null || true
    fi
    if [[ "$_forward_before" != "1" ]]; then
        sysctl -w net.ipv4.ip_forward="$_forward_before" >/dev/null 2>&1 || true
    fi
}

stop_agent() {
    if [[ -n "$AGENT_PID" ]] && kill -0 "$AGENT_PID" 2>/dev/null; then
        kill "$AGENT_PID" 2>/dev/null || true
        wait "$AGENT_PID" 2>/dev/null || true
    fi
    rm -f "$AGENT_SOCKET"
}

stop_minio() {
    docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
}

write_artifact_store_config() {
    if [[ ! -d "$ARTIFACT_STORE_DIR" ]]; then
        mkdir -p "$ARTIFACT_STORE_DIR"
        _artifact_store_dir_created="yes"
    fi
    printf '%s\n' "$STORE_ROOT" > "$ARTIFACT_STORE_DIR/store"
    printf '%s\n' "$MINIO_ENDPOINT" > "$ARTIFACT_STORE_DIR/endpoint"
}

cleanup() {
    stop_agent
    stop_minio
    if [[ "$_artifact_store_dir_created" == "yes" ]]; then
        rm -rf "$ARTIFACT_STORE_DIR"
    fi
    purge_fsb_resources
    purge_jail_dirs
    restore_host_state
}

# --- re-invoke as root -------------------------------------------------------
if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null; then
        log "re-invoking as root (sudo -E)"
        exec sudo -E env "PATH=$PATH" "WORK=$WORK" "CHAIN_SOURCE_IMAGE=$CHAIN_SOURCE_IMAGE" \
            "CHAIN_IMAGE=$CHAIN_IMAGE" "CHAIN_MACHINE_VCPU=$CHAIN_MACHINE_VCPU" \
            "CHAIN_MACHINE_MEM=$CHAIN_MACHINE_MEM" "CHAIN_ROOTFS_SIZE=$CHAIN_ROOTFS_SIZE" \
            "CHAIN_EXECD=$CHAIN_EXECD" \
            "MINIO_IMAGE=$MINIO_IMAGE" "MINIO_PORT=$MINIO_PORT" "MINIO_AK=$MINIO_AK" \
            "MINIO_SK=$MINIO_SK" "SANDBOX_TEMPLATE_BUILDER_IMAGE=$BUILDER_IMAGE" \
            "FC_VERSION=$FC_VERSION" "FC_BINARY=$FC_BINARY" "FC_JAILER=$FC_JAILER" \
            "FC_KERNEL=$FC_KERNEL" \
            "FC_ROOTFS=$FC_ROOTFS" "FC_STATE_ROOT=$FC_STATE_ROOT" \
            "$0" "$@"
    fi
    die "must run as root (or install sudo)"
fi

# --- explicit cleanup mode ---------------------------------------------------
if [[ "${1:-}" == "--cleanup" ]]; then
    record_host_state
    cleanup
    log "host cleanup done (agent, MinIO container, netns, firecracker/jailer processes, jail dirs, bridge, MASQUERADE rule)"
    exit 0
fi

# --- host prechecks ----------------------------------------------------------
[[ "$(uname -s)" == "Linux" ]] || die "requires Linux, got $(uname -s)"
[[ "$(uname -m)" == "x86_64" ]] || die "requires x86_64, got $(uname -m)"
[[ -e /dev/kvm ]] || die "/dev/kvm is missing (enable KVM/nested virt)"
[[ -e /dev/net/tun ]] || die "/dev/net/tun is missing"
for cmd in ip iptables sysctl ping curl jq docker go; do
    command -v "$cmd" >/dev/null || die "missing required command: $cmd"
done
docker info >/dev/null 2>&1 || die "docker is not usable (daemon running?)"

# Every set -e termination prints the failing line: without this, a failed
# command substitution (e.g. a curl that could not connect) exits silently.
trap 'echo "[chain-e2e] ERROR: line $LINENO: $BASH_COMMAND" >&2' ERR

mkdir -p "$BIN_DIR" "$ART_DIR" "$WORK/input" "$STATE_ROOT_DIR" /run/netns /run/fast-sandbox/netns

# --- artifacts (firecracker binary + snapshot-prep inputs for the driver E2E)
download() { # url target
    log "downloading $2"
    curl -fL --retry 3 -o "$2.tmp" "$1" || die "download failed: $1"
    mv "$2.tmp" "$2"
}
if [[ ! -x "$FC_BINARY" ]]; then
    tarball="$WORK/firecracker-$FC_VERSION-x86_64.tgz"
    download "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz" "$tarball"
    mkdir -p "$WORK/extract"
    tar -xzf "$tarball" -C "$WORK/extract"
    found="$(find "$WORK/extract" -type f -name "firecracker-v${FC_VERSION#v}-x86_64" | head -1)"
    [[ -n "$found" ]] || die "firecracker binary not found in release tarball"
    cp "$found" "$FC_BINARY" && chmod +x "$FC_BINARY"
    rm -rf "$WORK/extract"
fi
if [[ ! -x "$FC_JAILER" ]]; then
    tarball="$WORK/firecracker-$FC_VERSION-x86_64.tgz"
    if [[ ! -f "$tarball" ]]; then
        download "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz" "$tarball"
    fi
    mkdir -p "$WORK/extract"
    tar -xzf "$tarball" -C "$WORK/extract"
    found="$(find "$WORK/extract" -type f -name "jailer-v${FC_VERSION#v}-x86_64" | head -1)"
    [[ -n "$found" ]] || die "jailer binary not found in release tarball"
    cp "$found" "$FC_JAILER" && chmod +x "$FC_JAILER"
    rm -rf "$WORK/extract"
fi
[[ -f "$FC_KERNEL" ]] || download "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260722-38359b8055fc-0/x86_64/vmlinux-6.1.176" "$FC_KERNEL"
[[ -f "$FC_ROOTFS" ]] || download "https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/rootfs/bionic.rootfs.ext4" "$FC_ROOTFS"

# --- MinIO -------------------------------------------------------------------
# From here on every created resource (agent, MinIO container, test bridge,
# netns, taps) is torn down on exit; --cleanup handles interrupted runs.
trap 'cleanup' EXIT
log "starting MinIO ($MINIO_IMAGE) on :$MINIO_PORT"
docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
# A fresh data dir guarantees the bucket state matches this run: a previous
# crashed run can leave the store half-initialized (and a stale bucket
# would make the later mc mb non-idempotent).
rm -rf "$MINIO_DATA"
mkdir -p "$MINIO_DATA"
docker run -d --name "$MINIO_CONTAINER" \
    -p "$MINIO_PORT":9000 -p 9001:9001 \
    -e MINIO_ROOT_USER="$MINIO_AK" -e MINIO_ROOT_PASSWORD="$MINIO_SK" \
    -v "$MINIO_DATA:/data" \
    "$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
for attempt in $(seq 1 30); do
    if curl -fsS "$MINIO_ENDPOINT/minio/health/live" >/dev/null 2>&1; then break; fi
    sleep 1
    [[ "$attempt" == 30 ]] && die "MinIO did not become healthy"
done

# mc runs as a throwaway container; the alias/config must persist across
# calls, so the mc config dir is mounted from the workspace.
mc() { docker run --rm --network host -v "$WORK/mc-config:/root/.mc" minio/mc "$@"; }
mc alias set chain "$MINIO_ENDPOINT" "$MINIO_AK" "$MINIO_SK" >/dev/null || die "mc alias failed"
mc mb "chain/$MINIO_BUCKET" >/dev/null || die "mc mb failed"
# The bucket is the publish target of the builder; verify it really exists
# (mc mb is a no-op report on an existing bucket) before publishing into it.
mc stat "chain/$MINIO_BUCKET" >/dev/null || die "bucket $MINIO_BUCKET was not created on MinIO"
log "MinIO ready: bucket=$MINIO_BUCKET existing objects=$(mc ls --recursive "chain/$MINIO_BUCKET" 2>/dev/null | wc -l)"

# --- builder publish ---------------------------------------------------------
IMAGE_TAR="$WORK/input/image.tar"
log "exporting source image $CHAIN_SOURCE_IMAGE"
docker pull -q "$CHAIN_SOURCE_IMAGE" >/dev/null 2>&1 || die "docker pull failed: $CHAIN_SOURCE_IMAGE"
docker save "$CHAIN_SOURCE_IMAGE" -o "$IMAGE_TAR" >/dev/null 2>&1 || die "docker save failed"

log "building builder image ($BUILDER_IMAGE)"
docker build --progress=plain -t "$BUILDER_IMAGE" \
    -f "$REPO_ROOT/build/Dockerfile.sandboxtemplate-builder" "$REPO_ROOT" || die "builder image build failed"

SPEC_JSON=$(jq -n \
    --arg image "$CHAIN_IMAGE" \
    --arg vcpu "$CHAIN_MACHINE_VCPU" \
    --arg mem "$CHAIN_MACHINE_MEM" \
    --arg rootfs "$CHAIN_ROOTFS_SIZE" \
    --arg publish "$STORE_ROOT" \
    --arg execd "$CHAIN_EXECD" \
    '{image: $image, entrypoint: ["/bin/sh"], kernel: "vmlinux.bin",
      machine: {vcpu: $vcpu, memory: $mem}, init: "/usr/local/sbin/sandbox-init",
      readiness: {warmupSeconds: 15},
      output: {rootfsSize: $rootfs, format: "native", publish: $publish}}
     | if $execd != "" then . + {execd: $execd} else . end')

log "running the builder pipeline (publish -> $STORE_ROOT)"
BUILD_DIR="$WORK/build"
# A fresh build dir: a crashed run leaves stale unix sockets (boot.sock /
# restore.sock) that make the next firecracker bind fail and its API
# wait-for-file pass against the dead socket.
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"
if ! docker run --rm \
    --privileged \
    --device /dev/kvm:/dev/kvm \
    --device /dev/net/tun:/dev/net/tun \
    --add-host host.docker.internal:host-gateway \
    -v "$WORK/input":/input:ro \
    -v "$BUILD_DIR":/build \
    -e SANDBOX_TEMPLATE_SPEC="$SPEC_JSON" \
    -e SANDBOX_TEMPLATE_WORKDIR=/build \
    -e SANDBOX_TEMPLATE_IMAGE_TAR=/input/image.tar \
    -e POD_NAME=chain-e2e \
    -e POD_NAMESPACE=default \
    -e AWS_ENDPOINT_URL="http://host.docker.internal:$MINIO_PORT" \
    -e AWS_ACCESS_KEY_ID="$MINIO_AK" \
    -e AWS_SECRET_ACCESS_KEY="$MINIO_SK" \
    -e AWS_DEFAULT_REGION=us-east-1 \
    "$BUILDER_IMAGE" 2>&1 | tee "$WORK/builder.log"; then
    # The builder surfaces its own error; the firecracker serial consoles
    # carry the actual VMM failure (e.g. KVM open errors) and are the first
    # place to look.
    echo "=== boot.console.log (tail) ===" >&2
    tail -60 "$BUILD_DIR/boot.console.log" 2>/dev/null || true
    echo "=== restore.console.log (tail) ===" >&2
    tail -60 "$BUILD_DIR/restore.console.log" 2>/dev/null || true
    echo "=== build tree ===" >&2
    ls -la "$BUILD_DIR" 2>/dev/null || true
    die "builder pipeline failed (full log: $WORK/builder.log)"
fi

# --- publish layout assertions (verification point 1) -------------------------
log "asserting the published layout"
INDEX_KEY="index/$(printf '%s' "$CHAIN_IMAGE" | sha256sum | awk '{print $1}').json"
INDEX_JSON="$(mc cat "chain/$MINIO_BUCKET/publish/$INDEX_KEY")" || fail "index object missing: $INDEX_KEY"
INDEX_IMAGE="$(printf '%s' "$INDEX_JSON" | jq -r .image)"
[[ "$INDEX_IMAGE" == "$CHAIN_IMAGE" ]] || fail "index.image=$INDEX_IMAGE != $CHAIN_IMAGE"
MANIFEST_REF="$(printf '%s' "$INDEX_JSON" | jq -r .manifestRef)"
MANIFEST_KEY="${MANIFEST_REF#s3://$MINIO_BUCKET/}"
MANIFEST_JSON="$(mc cat "chain/$MINIO_BUCKET/$MANIFEST_KEY")" || fail "manifest missing: $MANIFEST_REF"
INDEX_DIGEST="$(printf '%s' "$INDEX_JSON" | jq -r .artifactDigest)"
# Hash the raw object bytes (command substitution strips the trailing
# newline the digest was computed over).
MANIFEST_DIGEST="$(mc cat "chain/$MINIO_BUCKET/$MANIFEST_KEY" | sha256sum | awk '{print $1}')"
[[ "$INDEX_DIGEST" == "$MANIFEST_DIGEST" ]] || fail "index.artifactDigest=$INDEX_DIGEST != sha256(manifest)=$MANIFEST_DIGEST"
BUILD_DIR_KEY="$(dirname "$MANIFEST_KEY")"
for object in rootfs.ext4 vmstate.snap memory.snap SHA256SUMS manifest.json; do
    mc stat "chain/$MINIO_BUCKET/$BUILD_DIR_KEY/$object" >/dev/null || fail "published object missing: $object"
done
while IFS= read -r name; do
    want="$(printf '%s' "$MANIFEST_JSON" | jq -r --arg n "$name" '.files[$n].sha256')"
    [[ "$want" != "null" && -n "$want" ]] || fail "manifest.files has no entry for $name"
    local_path="$BUILD_DIR/$name"
    [[ -f "$local_path" ]] || fail "local build artifact missing: $name"
    # 1) The builder artifact must match its manifest digest (sparse-aware
    #    hash equals the plain hash: holes are zero bytes).
    local_hash="$(sha256sum "$local_path" | awk '{print $1}')"
    [[ "$local_hash" == "$want" ]] || fail "build artifact $name digest mismatch: got=$local_hash want=$want"
    # 2) The stored object must carry the full logical size. (mc cat
    #    truncates large objects on this host - a 3 GiB rootfs came back
    #    2779 bytes short - so the download below is only used with a
    #    size check.)
    stored_size="$(mc stat --json "chain/$MINIO_BUCKET/$BUILD_DIR_KEY/$name" 2>/dev/null | jq -r .size)"
    local_size="$(stat -c %s "$local_path")"
    [[ "$stored_size" == "$local_size" ]] || fail "stored $name size $stored_size != local $local_size"
    # 3) Download verification is best-effort: this mc release returns 0
    #    without writing the target for large objects (and mc cat
    #    truncated a 3 GiB rootfs by 2779 bytes), so a failed or
    #    size-mismatched download warns instead of failing. Upload
    #    correctness is guaranteed separately by aws CLI's per-part
    #    Content-MD5; the authoritative checks are the local digest
    #    (step 1) and the stored size (step 2).
    rm -f "$WORK/verify-$name"
    if mc cp "chain/$MINIO_BUCKET/$BUILD_DIR_KEY/$name" "$WORK/verify-$name" >/dev/null 2>&1 && [[ -s "$WORK/verify-$name" ]]; then
        dl_size="$(stat -c %s "$WORK/verify-$name")"
        if [[ "$dl_size" == "$local_size" ]]; then
            dl_hash="$(sha256sum "$WORK/verify-$name" | awk '{print $1}')"
            [[ "$dl_hash" == "$want" ]] || fail "downloaded $name digest mismatch: got=$dl_hash want=$want"
        else
            echo "warning: mc cp downloaded $name with size $dl_size != $local_size; download check skipped" >&2
        fi
    else
        echo "warning: mc cp download of $name skipped (tool limitation); upload covered by local digest + stored size" >&2
    fi
    rm -f "$WORK/verify-$name"
done < <(printf '%s' "$MANIFEST_JSON" | jq -r '.files | keys[]' | grep -v '^overlaybd/')
pass "verification point 1: publish layout (index + digest16 build + SHA256SUMS)"

# --- runtime-agent -----------------------------------------------------------
log "building the runtime-agent"
(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$AGENT_BIN" ./cmd/firecracker-runtime)

# Fresh agent state: the pull layer treats a committed cache as final
# (idempotent, never refreshed), so a cache from an earlier run would keep
# answering with the PREVIOUS build's manifest digest. Only the agent's own
# subtree (images cache + journal) is removed: the state root may be a
# shared mount (e.g. /var/lib/fast-sandbox on a reflink filesystem).
rm -rf "$STATE_ROOT_DIR/images" "$STATE_ROOT_DIR/agent"
mkdir -p "$STATE_ROOT_DIR"

log "writing the registry configuration (read-only credential)"
mkdir -p "$REPO_ROOT/.chain-e2e-gen"
cat > "$REPO_ROOT/.chain-e2e-gen/gen-registry.go" <<'EOF'
// Command gen-registry emits a compiled registry configuration for one
// S3 credential (used by the chain E2E to feed the runtime-agent).
package main

import (
	"fmt"
	"os"

	"fast-sandbox/internal/registryconfig"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: gen-registry <host> <username> <password> <endpoint>")
		os.Exit(1)
	}
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: os.Args[1], Username: os.Args[2], Password: os.Args[3], Endpoint: os.Args[4],
	}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	payload, err := compiled.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(payload)
}
EOF
(cd "$REPO_ROOT" && GOTOOLCHAIN=local go run .chain-e2e-gen/gen-registry.go \
    "127.0.0.1:$MINIO_PORT" "$MINIO_AK" "$MINIO_SK" "$MINIO_ENDPOINT") > "$REGISTRY_FILE"
jq -e . "$REGISTRY_FILE" >/dev/null || die "generated registry.json is invalid"
rm -rf "$REPO_ROOT/.chain-e2e-gen"

log "starting the runtime-agent (socket=$AGENT_SOCKET, store=$STORE_ROOT)"
# Clear leftovers of crashed runs: a stale socket file (or a surviving
# agent process from an interrupted run) would let the readiness loop pass
# and the first RPC hit the OLD listener instead of this agent.
rm -f "$AGENT_SOCKET"
pkill -f "$AGENT_BIN" 2>/dev/null || true
sleep 0.3
write_artifact_store_config
# The agent reads all tunables from one YAML config (the mounted ConfigMap
# in-cluster; a plain file here). Bare-process runs also rely on the
# built-in defaults for the rest.
cat > "$WORK/agent.yaml" <<EOF
socket: $AGENT_SOCKET
stateRoot: $STATE_ROOT_DIR
registryConfig: $REGISTRY_FILE
EOF
env FAST_SANDBOX_AGENT_CONFIG="$WORK/agent.yaml" \
    "$AGENT_BIN" > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
# Readiness = the agent actually answers, not just a socket file existing.
for attempt in $(seq 1 40); do
    if curl -sS --noproxy '*' --unix-socket "$AGENT_SOCKET" -X POST \
        http://firecracker-agent/v1/health -H 'Content-Type: application/json' \
        -d '{"requestId":"","namespace":"chain-e2e","podUid":"chain-e2e-pod"}' 2>/dev/null \
        | jq -e .ok >/dev/null 2>&1; then
        break
    fi
    kill -0 "$AGENT_PID" 2>/dev/null || die "runtime-agent exited early: $(tail -5 "$WORK/agent.log")"
    sleep 0.5
    [[ "$attempt" == 40 ]] && die "runtime-agent did not answer health in time: $(tail -5 "$WORK/agent.log")"
done
log "runtime-agent healthy"

# --- StateRoot reflink probe ---------------------------------------------------
# On a non-reflink filesystem (ext4) every per-Sandbox rootfs copy falls
# back to a full ~3 GiB copy (~1.8 s per create, the dominant cost of the
# NoInfra create path). Point FC_STATE_ROOT at a reflink-capable mount to
# make the copy CoW; see scripts/firecracker-xfs-stateroot.sh.
REFLINK_PROBE="$STATE_ROOT_DIR/.reflink-probe"
REFLINK_COPY="$STATE_ROOT_DIR/.reflink-probe-copy"
rm -f "$REFLINK_PROBE" "$REFLINK_COPY"
printf 'reflink-probe' > "$REFLINK_PROBE"
if cp --reflink=always "$REFLINK_PROBE" "$REFLINK_COPY" 2>/dev/null; then
    log "StateRoot reflink: supported (CoW per-instance rootfs copies)"
else
    log "StateRoot reflink: NOT supported (ext4?) - each create pays a full rootfs copy (~1.8s); see scripts/firecracker-xfs-stateroot.sh --loop"
fi
rm -f "$REFLINK_PROBE" "$REFLINK_COPY"

agent_rpc() { # path request-json (leading slash optional)
    # ${1#/} strips the leading slash: the callers pass /v1/... and a raw
    # concatenation would produce http://firecracker-agent//v1/... .
    # curl 7.61 sends the double-slash path verbatim and the agent's route
    # table never matches it (404 page not found); curl >= 8 normalizes
    # the path, which is why local runs cannot reproduce the failure.
    local url="http://firecracker-agent/${1#/}"
    curl -sS --noproxy '*' --unix-socket "$AGENT_SOCKET" -X POST \
        "$url" -H 'Content-Type: application/json' \
        -d "$2"
}

# agent_ok fails the run with the RPC response, the agent log, and the
# MinIO request log when an RPC did not succeed.
agent_ok() { # description response-json
    local description=$1 response=$2
    # Empty response = success: ReleaseDevices / UnpinImage answer 204 No
    # Content. (jq 1.6 on RHEL 8 silently exits 0 on empty input, which
    # would misclassify a successful 204 as an error JSON.)
    if [[ -z "$response" ]]; then
        return 0
    fi
    # The agent encodes failures as {"code": ...} JSON; treat that as a
    # failure even though it parses as valid JSON.
    if printf '%s' "$response" | jq -e 'has("code")' >/dev/null 2>&1; then
        echo "RPC returned an error ($description): $(printf '%s' "$response" | jq -c .)" >&2
        echo "=== curl verbose replay (response headers identify the server) ===" >&2
        curl -v --noproxy '*' --unix-socket "$AGENT_SOCKET" -X POST \
            "http://firecracker-agent/v1/health" -H 'Content-Type: application/json' \
            -d '{"requestId":"","namespace":"chain-e2e","podUid":"chain-e2e-pod"}' 2>&1 | tail -25 >&2 || true
        echo "=== socket ===" >&2
        ls -la "$AGENT_SOCKET" >&2 || true
        echo "=== socket listeners ===" >&2
        (ss -xlp 2>/dev/null || netstat -lxp 2>/dev/null) | grep -F "$(basename "$AGENT_SOCKET")" >&2 || true
        echo "=== agent processes ===" >&2
        pgrep -fl firecracker-runtime >&2 || true
        echo "=== agent.log (tail) ===" >&2
        tail -30 "$WORK/agent.log" >&2
        echo "=== MinIO logs (tail) ===" >&2
        docker logs --tail 30 "$MINIO_CONTAINER" >&2 2>&1 || true
        fail "$description failed"
    fi
    if ! printf '%s' "$response" | jq -e . >/dev/null 2>&1; then
        echo "RPC failed ($description), raw response: $response" >&2
        echo "=== agent.log (tail) ===" >&2
        tail -30 "$WORK/agent.log" >&2
        echo "=== MinIO logs (tail) ===" >&2
        docker logs --tail 30 "$MINIO_CONTAINER" >&2 2>&1 || true
        fail "$description failed"
    fi
}

# --- scenario A: pull chain through the real agent ----------------------------
log "scenario A: PinImage through the runtime-agent (SigV4 + credential mapping)"
IDENTITY='{"requestId":"chain-pin-1","namespace":"chain-e2e","podUid":"chain-e2e-pod"}'
PIN_START="$(date +%s%N)"
# Health first: a 404 here means the request never reached the agent's
# routes (proxy/socket issue); a healthy reply isolates PinImage.
# The literal call is the control: it is byte-identical to the readiness
# probe that succeeded, so a failure here means agent state, while a
# success here plus a failure of the function form means the function.
LITERAL_HEALTH="$(curl -sS --noproxy '*' --unix-socket "$AGENT_SOCKET" -X POST \
    http://firecracker-agent/v1/health -H 'Content-Type: application/json' \
    -d '{"requestId":"","namespace":"chain-e2e","podUid":"chain-e2e-pod"}')"
agent_ok "Health (literal control)" "$LITERAL_HEALTH"
HEALTH_CHECK="$(agent_rpc /v1/health '{"requestId":"","namespace":"chain-e2e","podUid":"chain-e2e-pod"}')"
agent_ok "Health (agent_rpc)" "$HEALTH_CHECK"
PIN1="$(agent_rpc /v1/pin-image "$(jq -nc --argjson id "$IDENTITY" --arg img "$CHAIN_IMAGE" '$id + {image: $img}')")"
agent_ok "PinImage" "$PIN1"
PIN_DIGEST="$(printf '%s' "$PIN1" | jq -er .manifestDigest)"
[[ "$(printf '%s' "$PIN1" | jq -r .ready)" == "true" ]] || fail "PinImage ready=false"
[[ "$PIN_DIGEST" == "$MANIFEST_DIGEST" ]] || fail "PinImage manifestDigest=$PIN_DIGEST != published $MANIFEST_DIGEST"

CACHE_DIR="$STATE_ROOT_DIR/images/$(printf '%s' "$CHAIN_IMAGE" | sha256sum | awk '{print $1}')"
for file in rootfs.img vmstate.snap memory.snap manifest.json; do
    [[ -s "$CACHE_DIR/$file" ]] || fail "cache file missing after pull: $file"
done
LOCAL_MANIFEST="$(cat "$CACHE_DIR/manifest.json")"
# Hash the file bytes directly: the cached manifest keeps the trailing
# newline the index digest was computed over.
LOCAL_DIGEST="$(sha256sum "$CACHE_DIR/manifest.json" | awk '{print $1}')"
[[ "$LOCAL_DIGEST" == "$INDEX_DIGEST" ]] || fail "cached manifest != published manifest (index digest)"
for file in rootfs.img vmstate.snap memory.snap; do
    publish_name="${file/rootfs.img/rootfs.ext4}"
    want="$(printf '%s' "$LOCAL_MANIFEST" | jq -r --arg n "$publish_name" '.files[$n].sha256')"
    got="$(sha256sum "$CACHE_DIR/$file" | awk '{print $1}')"
    [[ "$got" == "$want" ]] || fail "cached $file digest mismatch: got=$got want=$want"
done
pass "verification point 2/3: SigV4 + credential mapping (pull succeeded over MinIO)"
PIN_MS="$(( ($(date +%s%N) - PIN_START) / 1000000 ))"
PIN_MS_LOGGED="1"

ROOTFS_MTIME_BEFORE="$(stat -c %Y "$CACHE_DIR/rootfs.img")"
PIN2="$(agent_rpc /v1/pin-image "$(jq -nc --argjson id "$IDENTITY" --arg img "$CHAIN_IMAGE" '$id + {image: $img}')")"
[[ "$(printf '%s' "$PIN2" | jq -r .manifestDigest)" == "$PIN_DIGEST" ]] || fail "re-PinImage digest changed"
ROOTFS_MTIME_AFTER="$(stat -c %Y "$CACHE_DIR/rootfs.img")"
[[ "$ROOTFS_MTIME_AFTER" == "$ROOTFS_MTIME_BEFORE" ]] || fail "re-PinImage re-pulled (committed cache must be idle)"
pass "verification point 5a: PinImage idempotency (committed cache, zero re-pull)"

# --- scenario B: driver restore from the pulled artifacts ---------------------
log "scenario B: driver restore from builder artifacts (FC_SKIP_PREP, agent wired)"
record_host_state
purge_fsb_resources
# The NoInfra case is the control: infra (GuestCopy delivery) dominates the
# create time when enabled, and the runtime agent wiring is identical. The
# Concurrent cases (parallel + serial baseline) restore 5 VMs and require
# every instance's execd to answer through its own slot DNAT (per-clone
# netns data plane); their stage breakdowns expose the load bottlenecks.
log "execd baked ($CHAIN_EXECD); scenario B will probe GET /ping in the guest (single + concurrent)"
(cd "$REPO_ROOT" && env FC_BINARY="$FC_BINARY" FC_JAILER="$FC_JAILER" \
    FC_KERNEL="$FC_KERNEL" FC_ROOTFS="$FC_ROOTFS" \
    FC_STATE_ROOT="$FC_STATE_ROOT" FC_IMAGE_REF="$CHAIN_IMAGE" \
    FC_SKIP_PREP=1 FC_AGENT_SOCKET="$AGENT_SOCKET" \
    FC_EXECD_PROBE=1 \
    go test -tags firecracker -count=1 -v \
    -run '^(TestFirecrackerDriverE2E|TestFirecrackerDriverE2ENoInfra|TestFirecrackerDriverE2EConcurrent|TestFirecrackerDriverE2EConcurrentSerial)$' \
    ./internal/runtime/firecracker/) 2>&1 | tee "$WORK/driver-e2e.log" || fail "driver E2E failed (see output above)"
pass "verification point 4: builder snapshot <-> driver restore (Running + guest reachable + concurrent execd ready)"

# --- scenario C: lease lifecycle + driver delete -> agent unpin --------------
log "scenario C: lease lifecycle and cleanup semantics"
READ_IDENTITY='{"requestId":"","namespace":"chain-e2e","podUid":"chain-e2e-pod"}'
# The driver E2E just ran against this agent; make sure it is still alive
# before the lease sequence (a dead socket makes the first RPC curl fail
# and set -e would exit without diagnostics).
HEALTH_BEFORE="$(agent_rpc /v1/health "$READ_IDENTITY")" || {
    echo "runtime-agent unreachable after scenario B: $(tail -10 "$WORK/agent.log")" >&2
    fail "runtime-agent died during the driver E2E"
}
agent_ok "Health (scenario C)" "$HEALTH_BEFORE"
PINS_BEFORE="$(printf '%s' "$HEALTH_BEFORE" | jq -r .pinCount)"
# The driver E2E deleted its sandbox through the UDS-wired driver, so its
# UnpinImage must have released the scenario-A pin.
[[ "$PINS_BEFORE" == "0" ]] || fail "agent pinCount=$PINS_BEFORE after driver delete, expected 0 (driver delete must unpin)"
pass "driver delete released the scenario-A pin through the UDS API"

# Each RPC needs its OWN requestId: the agent's journal replays a committed
# request id, so reusing chain-pin-1 for LeaseDevices would return the
# recorded PinImage result instead of a lease.
PIN2_RESP="$(agent_rpc /v1/pin-image "$(jq -nc --arg req 'chain-pin-2' --arg img "$CHAIN_IMAGE" \
    '{requestId: $req, namespace: "chain-e2e", podUid: "chain-e2e-pod", image: $img}')")"
agent_ok "PinImage (re-pin)" "$PIN2_RESP"
LEASE="$(agent_rpc /v1/lease-devices "$(jq -nc --arg req 'chain-lease-1' \
    --arg sb "chain-sbx-1" --arg img "$CHAIN_IMAGE" \
    '{requestId: $req, namespace: "chain-e2e", podUid: "chain-e2e-pod",
      sandboxId: $sb, image: $img, memSizeMiB: 512, rootfsWritable: false}')")"
agent_ok "LeaseDevices" "$LEASE"
LEASE_ID="$(printf '%s' "$LEASE" | jq -er .leaseId)"
[[ "$(printf '%s' "$LEASE" | jq -r .rootfsDev)" == "$CACHE_DIR/rootfs.img" ]] || fail "lease rootfsDev != cache path"
LISTED="$(agent_rpc /v1/list-leases '{"requestId":"chain-list-1","namespace":"chain-e2e","podUid":"chain-e2e-pod"}')"
agent_ok "ListLeases" "$LISTED"
[[ "$(printf '%s' "$LISTED" | jq -r '.leases | length')" == "1" ]] || fail "ListLeases != 1 after lease"
RELEASE_RESP="$(agent_rpc /v1/release-devices "$(jq -nc --arg req 'chain-release-1' --arg lid "$LEASE_ID" \
    '{requestId: $req, namespace: "chain-e2e", podUid: "chain-e2e-pod", leaseId: $lid}')")"
agent_ok "ReleaseDevices" "$RELEASE_RESP"
LISTED="$(agent_rpc /v1/list-leases '{"requestId":"chain-list-2","namespace":"chain-e2e","podUid":"chain-e2e-pod"}')"
agent_ok "ListLeases (after release)" "$LISTED"
[[ "$(printf '%s' "$LISTED" | jq -r '.leases | length')" == "0" ]] || fail "ListLeases != 0 after release"
UNPIN_RESP="$(agent_rpc /v1/unpin-image "$(jq -nc --arg req 'chain-unpin-1' --arg img "$CHAIN_IMAGE" \
    '{requestId: $req, namespace: "chain-e2e", podUid: "chain-e2e-pod", image: $img}')")"
agent_ok "UnpinImage" "$UNPIN_RESP"
HEALTH_AFTER="$(agent_rpc /v1/health "$READ_IDENTITY")"
[[ "$(printf '%s' "$HEALTH_AFTER" | jq -r .pinCount)" == "0" ]] || fail "pinCount != 0 after unpin"
pass "verification point 5b: lease release + unpin (state returns to zero)"

# --- performance and artifact summary -----------------------------------------
log "=== performance summary ==="

GREEN='\033[1;32m'; CYAN='\033[1;36m'; BOLD='\033[1m'; NC='\033[0m'

echo "builder pipeline (from builder.log):"
grep -h "sandbox template build stages" "$WORK/builder.log" 2>/dev/null \
    | sed -E 's/.*"sandbox template build stages" (.*)/  \1/' || echo "  (builder.log missing)"
grep -h "snapshot stage phases" "$WORK/builder.log" 2>/dev/null \
    | sed -E 's/.*(format=.*)$/  \1/' || true
if [[ "${PIN_MS_LOGGED:-}" == "1" ]]; then
    echo "scenario A: cold PinImage (index + manifest + artifacts over MinIO) = ${PIN_MS} ms"
    echo "  cache size: $(du -sh "$STATE_ROOT_DIR/images" 2>/dev/null | cut -f1)"
fi

# --- aggregated driver restore statistics -------------------------------------
# Raw per-sandbox lines stay in driver-e2e.log; this section prints the
# min/avg/max of every create stage and the execd readiness delta.
LOG="$WORK/driver-e2e.log"
CREATES=$(grep -ic "firecracker sandbox created" "$LOG" 2>/dev/null || echo 0)
REACHED=$(grep -c "guest reachable via slot" "$LOG" 2>/dev/null || echo 0)
EXECD_OK=$(grep -c "execd /ping OK" "$LOG" 2>/dev/null || echo 0)

summarize_ms() { # stdin: one ms value per line -> min/avg/max (n)
    awk '{ sum+=$1; if (NR==1 || $1<min) min=$1; if ($1>max) max=$1 }
         END { if (NR>0) printf "min %7.2fms  avg %7.2fms  max %7.2fms  (n=%d)\n", min, sum/NR, max, NR; else print "  -" }'
}

field_ms() { # $1 = klog field name; matches `field="<number><unit>` without quotes
    grep -o "$1=\"[0-9.]*[a-zµ]*" "$LOG" 2>/dev/null \
        | sed -E 's/.*="([0-9.]+)([a-zµ]*)$/\1 \2/' \
        | awk '{ n=$1+0; if ($2=="µs" || $2=="us") n/=1000; else if ($2=="s") n*=1000; print n }'
}

echo ""
echo -e "${BOLD}driver restore (${CREATES} creates, aggregated):${NC}"
echo "  (the single Infra case inflates total/rootfs; NoInfra and batches are pure restore)"
for stage in total acquire rootfs launch configure boot; do
    printf "  %-9s " "${stage}:"
    field_ms "$stage" | summarize_ms
done
echo -e "  ${GREEN}reachability: ${REACHED}/${CREATES} slots${NC}   ${GREEN}execd ready: ${EXECD_OK}/${CREATES} probes${NC}"

echo ""
echo -e "${BOLD}execd readiness (delta after VM running):${NC}"
printf "  %-9s " "delta:"
grep -o "after [0-9.]*[a-zµ]*" "$LOG" 2>/dev/null \
    | sed -E 's/after ([0-9.]+)([a-zµ]*)/\1 \2/' \
    | awk '{ n=$1+0; if ($2=="µs" || $2=="us") n/=1000; else if ($2=="s") n*=1000; print n }' \
    | summarize_ms

echo ""
echo -e "${BOLD}load stage breakdown (serial vs parallel):${NC}"
grep -hE "load-mode=.*(wall=|stage breakdown)" "$LOG" 2>/dev/null \
    | sed -E 's/^.*(load-mode.*)$/  \1/' \
    | sed -E 's/^(  load-mode=.*wall=.*)$/\x1b[1;36m\1\x1b[0m/'
grep -hE "  (acquire|rootfs|infra|launch|configure|boot) +[0-9]" "$LOG" 2>/dev/null \
    | sed -E 's/^.*(  (acquire|rootfs|infra|launch|configure|boot) +.*)$/    \1/'

echo ""
echo "infra delivery breakdown (klog V(4), from driver-e2e.log):"
if grep -q '"infra delivery:' "$LOG" 2>/dev/null; then
    grep -h '"infra delivery:' "$LOG" 2>/dev/null \
        | sed -E 's/^.*"(infra delivery[^"]*)" (.*)$/  \1/'
else
    echo "  (no infra timing lines; klog V(4) requires -v=4)"
fi
echo "component sizes:"
echo "  build dir:   $(du -sh "$BUILD_DIR" 2>/dev/null | cut -f1)"
echo "  MinIO data:  $(du -sh "$MINIO_DATA" 2>/dev/null | cut -f1)"
echo "  agent state: $(du -sh "$STATE_ROOT_DIR" 2>/dev/null | cut -f1)"
echo "  MinIO objects: $(mc ls --recursive "chain/$MINIO_BUCKET" 2>/dev/null | wc -l)"
echo "disk: $(df -h "$WORK" 2>/dev/null | tail -1 | awk '{print "free="$4" of "$2" ("$5" used)"}')"
log "component logs: $WORK/builder.log, $WORK/agent.log, $WORK/driver-e2e.log, MinIO: docker logs $MINIO_CONTAINER"

log "full chain E2E passed: publish -> MinIO -> agent pull -> driver restore -> reachability -> cleanup"
log "artifacts under $WORK (builder.log, agent.log, state-root/images/$CHAIN_IMAGE)"
