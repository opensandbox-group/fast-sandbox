#!/usr/bin/env bash
# sandboxtemplate-oci-e2e.sh — exercise ONLY the OverlayBD OCI image
# packaging stage of the builder (attach empty raw volume → byte-exact
# write → commit [→ push]), decoupled from the full golden-image pipeline:
# no KVM, no Firecracker, no S3.
#
# Default (local mode): commit only. The artifacts — manifest.json + the
# LSMT layer blob per image — stay under the workspace strmvold session
# root; nothing is uploaded. The script asserts the local artifact shape
# (single layer, OverlayBD annotations, empty config, digest pins, blob
# present) and keeps the workspace for inspection.
#
# --push: additionally uploads both images to a throwaway registry:2,
# asserts the registry serves what the pins claim, and byte-verifies the
# roundtrip (strmvolctl attach --readonly, read the device, compare with
# the fixture — holes must read as zeros, the mkfs-residue guard).
#
# Requirements (internal build host):
#   - Linux x86_64 with EITHER ublk (kernel >= 5.19, /dev/ublk-control) OR
#     tcmu (target_core_user module + configfs; auto-selected when ublk is
#     absent — export SANDBOX_TEMPLATE_BLOCKDRIVER to pin manually)
#   - the streamingvolume runtime installed on the HOST and running:
#     strmvold (systemd unit from the t-storage-strmvold rpm), and for the
#     ublk path the overlaybd api server (127.0.0.1:9862); strmvolctl on PATH
#   - docker + registry:2 (--push only), go toolchain, jq, sha256sum
#
# Usage:
#   ./scripts/sandboxtemplate-oci-e2e.sh [--push] [--skip-roundtrip] [--clean]

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.sandboxtemplate-oci-e2e}"
REGISTRY_PORT="${REGISTRY_PORT:-15000}"
REGISTRY="127.0.0.1:${REGISTRY_PORT}"
PUSH=0
SKIP_ROUNDTRIP=0
CLEAN=0

log() { printf '\033[1;34m[st-oci-e2e]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[st-oci-e2e] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --push) PUSH=1; shift ;;
        --skip-roundtrip) SKIP_ROUNDTRIP=1; shift ;;
        --clean) CLEAN=1; shift ;;
        *) die "unknown argument: $1" ;;
    esac
done

[[ "$(uname -s)" == "Linux" ]] || die "requires Linux (ublk/strmvold stack)"
[[ "$(uname -m)" == "x86_64" ]] || die "requires x86_64"
command -v go >/dev/null || die "missing required command: go"
command -v jq >/dev/null || die "missing required command: jq"
command -v sha256sum >/dev/null || die "missing required command: sha256sum"
if [[ "$PUSH" == 1 ]]; then
    command -v docker >/dev/null || die "missing required command: docker (--push)"
    command -v strmvolctl >/dev/null || die "missing strmvolctl on PATH (t-storage-strmvold rpm, --push roundtrip)"
fi

# --- block driver selection ---------------------------------------------------
BLOCK_DRIVER="${SANDBOX_TEMPLATE_BLOCKDRIVER:-}"
if [[ -z "$BLOCK_DRIVER" ]]; then
    if [[ -e /dev/ublk-control ]]; then
        BLOCK_DRIVER=ublk
    else
        BLOCK_DRIVER=tcmu
    fi
fi
[[ "$BLOCK_DRIVER" == "ublk" || "$BLOCK_DRIVER" == "tcmu" ]] || die "SANDBOX_TEMPLATE_BLOCKDRIVER must be ublk or tcmu"
if [[ "$BLOCK_DRIVER" == "ublk" && ! -e /dev/ublk-control ]]; then
    die "blockDriver=ublk requires /dev/ublk-control (kernel >= 5.19); use tcmu or upgrade"
fi
if [[ "$BLOCK_DRIVER" == "tcmu" ]]; then
    modprobe target_core_user 2>/dev/null || true
    if ! lsmod 2>/dev/null | grep -q target_core_user; then
        die "blockDriver=tcmu requires the target_core_user kernel module (kernel too old or module missing)"
    fi
    mountpoint -q /sys/kernel/config || mount -t configfs configfs /sys/kernel/config 2>/dev/null || true
fi
log "block driver: $BLOCK_DRIVER"
# The ublk path additionally needs the overlaybd api server (the builder's
# strmvold session posts create-device requests there).
if [[ "$BLOCK_DRIVER" == "ublk" ]] && ! (echo > /dev/tcp/127.0.0.1/9862) 2>/dev/null; then
    die "overlaybd api server not reachable on 127.0.0.1:9862 — start the overlaybd runtime first"
fi

log "workspace: $WORK"
[[ "$CLEAN" == 1 ]] && trap 'rm -rf "$WORK"' EXIT
rm -rf "$WORK"
mkdir -p "$WORK"

# --- throwaway registry (--push only) ------------------------------------------
REGISTRY_CONTAINER=st-oci-e2e-registry
if [[ "$PUSH" == 1 ]]; then
    log "starting registry:2 on ${REGISTRY}"
    docker run -d --rm --name "$REGISTRY_CONTAINER" -p "${REGISTRY_PORT}:5000" registry:2 >/dev/null
    if [[ "$CLEAN" == 1 ]]; then
        trap 'docker stop "$REGISTRY_CONTAINER" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT
    fi
    for _ in $(seq 1 30); do
        curl -fsS "http://${REGISTRY}/v2/" >/dev/null 2>&1 && break
        sleep 1
    done
    curl -fsS "http://${REGISTRY}/v2/" >/dev/null || die "registry did not come up"
fi

# --- fixtures -----------------------------------------------------------------
# rootfs-like: 1GiB sparse with random extents and holes; memory-like: 64MiB dense.
ROOTFS_FIXTURE="$WORK/rootfs.fixture"
MEMORY_FIXTURE="$WORK/memory.fixture"
log "generating fixtures"
truncate -s 1G "$ROOTFS_FIXTURE"
dd if=/dev/urandom of="$ROOTFS_FIXTURE" bs=1M count=8 conv=notrunc status=none
dd if=/dev/urandom of="$ROOTFS_FIXTURE" bs=1M count=8 seek=64 conv=notrunc status=none
dd if=/dev/urandom of="$MEMORY_FIXTURE" bs=1M count=64 status=none
ROOTFS_SHA=$(sha256sum "$ROOTFS_FIXTURE" | awk '{print $1}')
MEMORY_SHA=$(sha256sum "$MEMORY_FIXTURE" | awk '{print $1}')
MEMORY_SIZE=$(stat -c%s "$MEMORY_FIXTURE")
log "fixture digests: rootfs=$ROOTFS_SHA memory=$MEMORY_SHA"

# --- oci-publish ----------------------------------------------------------------
log "building sandboxtemplate-builder"
(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$WORK/sandboxtemplate-builder" ./cmd/sandboxtemplate-builder/)

PUBLISH_FLAGS=(oci-publish
    --rootfs "$ROOTFS_FIXTURE"
    --memory "$MEMORY_FIXTURE"
    --registry "${REGISTRY}/e2e/templates/t1"
    --tag e2e
    --workdir "$WORK/publish-workdir")
if [[ "$PUSH" != 1 ]]; then
    PUBLISH_FLAGS+=(--no-push)
fi
log "running oci-publish (${PUSH_FLAGS:+push}${PUSH_FLAGS:-local-only} mode)"
if ! SANDBOX_TEMPLATE_REGISTRY_PLAINHTTP=1 \
    SANDBOX_TEMPLATE_BLOCKDRIVER="$BLOCK_DRIVER" \
    "$WORK/sandboxtemplate-builder" "${PUBLISH_FLAGS[@]}" > "$WORK/refs.txt" 2> "$WORK/oci-publish.err"; then
    tail -100 "$WORK/oci-publish.err" >&2
    die "oci-publish failed"
fi
cat "$WORK/refs.txt"

ROOTFS_REF="${REGISTRY}/e2e/templates/t1-rootfs:e2e"
MEMORY_REF="${REGISTRY}/e2e/templates/t1-mem:e2e"
PIN_ROOTFS=$(awk '/^rootfs-ref:/{print $2}' "$WORK/refs.txt")
PIN_MEMORY=$(awk '/^memory-ref:/{print $2}' "$WORK/refs.txt")
[[ -n "$PIN_ROOTFS" && -n "$PIN_MEMORY" ]] || die "oci-publish did not report refs"

# --- local artifact assertions (always) ------------------------------------------
SESSION_ROOT="$WORK/publish-workdir/strmvol"
assert_local_artifacts() {
    local name=$1 pin=$2
    local manifest_path layer_blob
    manifest_path=$(awk -v n="$name" '$1==n"-manifest:"{print $2}' "$WORK/refs.txt")
    layer_blob=$(awk -v n="$name" '$1==n"-blob:"{print $2}' "$WORK/refs.txt")
    [[ -n "$manifest_path" && -s "$manifest_path" ]] || die "$name: manifest missing: $manifest_path"
    [[ -n "$layer_blob" && -s "$layer_blob" ]] || die "$name: layer blob missing: $layer_blob"
    # The manifest pin must be the digest of the manifest file itself.
    local want_digest pin_digest
    want_digest=$(sha256sum "$manifest_path" | awk '{print $1}')
    pin_digest=${pin##*@sha256:}
    [[ "$want_digest" == "$pin_digest" ]] \
        || die "$name: manifest digest mismatch file=$want_digest pin=$pin_digest"
    jq -e '
        (.layers | length == 1)
        and (.layers[0].annotations // {} | has("containerd.io/snapshot/overlaybd/bs-digest"))
        and (.config.digest == "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a")
    ' "$manifest_path" >/dev/null || die "$name: manifest shape mismatch:
$(jq . "$manifest_path")"
    # The single layer the manifest names must be the blob we printed.
    local want_blob
    want_blob=$(jq -r '.layers[0].digest' "$manifest_path")
    [[ "$layer_blob" == "$SESSION_ROOT/blobs/$want_blob" ]] \
        || die "$name: blob path $layer_blob does not match manifest layer $want_blob"
    log "$name local artifacts OK: manifest=$manifest_path blob=$layer_blob ($(stat -c%s "$layer_blob") bytes)"
}
assert_local_artifacts rootfs "$PIN_ROOTFS"
assert_local_artifacts memory "$PIN_MEMORY"

# --- registry assertions + byte-exactness roundtrip (--push only) -----------------
if [[ "$PUSH" != 1 ]]; then
    log "local-only mode: artifacts kept under $WORK (rerun with --push for registry + roundtrip)"
    log "E2E passed — OCI packaging stage verified in isolation"
    exit 0
fi

registry_digest() {
    local repo=$1 tag=$2
    curl -fsS -I -H "Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json" \
        "http://${REGISTRY}/v2/${repo}/manifests/${tag}" \
        | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}'
}
ROOTFS_DIGEST=$(registry_digest "e2e/templates/t1-rootfs" "e2e") || die "rootfs image manifest missing in registry"
MEMORY_DIGEST=$(registry_digest "e2e/templates/t1-mem" "e2e") || die "memory image manifest missing in registry"
[[ "$PIN_ROOTFS" == "${ROOTFS_REF}@${ROOTFS_DIGEST}" ]] \
    || die "rootfs pin mismatch: reported=$PIN_ROOTFS registry=${ROOTFS_REF}@${ROOTFS_DIGEST}"
[[ "$PIN_MEMORY" == "${MEMORY_REF}@${MEMORY_DIGEST}" ]] \
    || die "memory pin mismatch: reported=$PIN_MEMORY registry=${MEMORY_REF}@${MEMORY_DIGEST}"
log "registry serves both images; pins match"

if [[ "$SKIP_ROUNDTRIP" == 1 ]]; then
    log "--skip-roundtrip: skipping attach verification"
else
    roundtrip() {
        local repo=$1 digest=$2 size=$3 sha=$4 name=$5
        local ref="${REGISTRY}/${repo}@${digest}"
        log "roundtrip $name: attach $ref"
        local attach_json
        attach_json=$(strmvolctl --plainHTTP attach --readonly true "$ref" 2>"$WORK/${name}-attach.err") \
            || { cat "$WORK/${name}-attach.err" >&2; die "$name: strmvolctl attach failed"; }
        local device volume_id
        device=$(echo "$attach_json" | jq -r '.device // .mountpoint // .mount.source // empty')
        volume_id=$(echo "$attach_json" | jq -r '.volumeID // .volume_id // empty')
        [[ -n "$device" && -b "$device" ]] || die "$name: no device in attach output: $attach_json"
        local got
        got=$(dd if="$device" bs=1M count=$((size / 1048576)) status=none | sha256sum | awk '{print $1}')
        if [[ "$got" != "$sha" ]]; then
            die "$name roundtrip digest mismatch: device=$got fixture=$sha (holes did not read as zeros?)"
        fi
        log "$name roundtrip OK (device bytes match fixture)"
        [[ -z "$volume_id" ]] || strmvolctl detach "$volume_id" >/dev/null 2>&1 || true
    }
    roundtrip "e2e/templates/t1-rootfs" "$ROOTFS_DIGEST" 1073741824 "$ROOTFS_SHA" "rootfs"
    roundtrip "e2e/templates/t1-mem" "$MEMORY_DIGEST" "$MEMORY_SIZE" "$MEMORY_SHA" "memory"
fi

log "E2E passed — OCI packaging stage verified (push + roundtrip)"
