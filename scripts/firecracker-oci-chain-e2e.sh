#!/usr/bin/env bash
# firecracker-oci-chain-e2e.sh — real pause/checkpoint chain E2E over an OCI
# overlaybd template: local registry:2, real template build, real
# overlaybd-ublkd, and N (default 100) pause/publish/restore cycles through
# the Firecracker driver, asserting the rootfs chain stays bounded.
#
# Requirements: Linux x86_64, root, /dev/kvm, /dev/net/tun, docker, go, jq,
# curl. Everything it creates (registry container, ublkd, netns/tap, state)
# is cleaned up on exit.
#
# Overrides: FC_CHAIN_CYCLES (100), CHAIN_SOURCE_IMAGE (alpine:3.19),
# REGISTRY_IMAGE (registry:2), REGISTRY_PORT (5000), WORK, the
# Firecracker assets FC_BINARY/FC_JAILER/FC_KERNEL/FC_ROOTFS, and CONVERTOR
# (overlaybd userspace convertor; probed at the usual install paths when
# unset). Tools come from OCI_TOOLS_DIR or OCI_TOOLS_IMAGE; without either,
# overlaybd is built from source (~10-20 min, first run only).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.firecracker-oci-chain-e2e}"
BIN_DIR="$WORK/bin"
RUN_DIR="$WORK/run"
mkdir -p "$BIN_DIR" "$RUN_DIR"

CYCLES="${FC_CHAIN_CYCLES:-100}"
CHAIN_SOURCE_IMAGE="${CHAIN_SOURCE_IMAGE:-alpine:3.19}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:2}"
REGISTRY_PORT="${REGISTRY_PORT:-5000}"
REGISTRY="127.0.0.1:${REGISTRY_PORT}"
TEMPLATE_REPO="templates"
SNAPSHOT_REPO="snapshots"
TEMPLATE_REF="oci://${REGISTRY}/${TEMPLATE_REPO}:tpl"
OCISocketDir="/run/fast-sandbox/overlaybd"
OCIToolsDir="/opt/fast-sandbox/oci"

log() { echo "[oci-chain-e2e] $*"; }
fail() { echo "[oci-chain-e2e] FAIL: $*" >&2; exit 1; }
pass() { echo "[oci-chain-e2e] PASS: $*"; }

[[ "$(uname -s)" == "Linux" ]] || fail "Linux required"
[[ "$(uname -m)" == "x86_64" ]] || fail "x86_64 required"
[[ "$(id -u)" == "0" ]] || fail "root required (re-run with sudo)"
[[ -e /dev/kvm ]] || fail "/dev/kvm required"
[[ -e /dev/net/tun ]] || fail "/dev/net/tun required"
for tool in docker go jq curl; do command -v "$tool" >/dev/null || fail "$tool required"; done
[[ -n "${FC_BINARY:-}" && -n "${FC_KERNEL:-}" ]] || fail "FC_BINARY and FC_KERNEL are required"
for name in FC_BINARY FC_KERNEL; do
    [[ -z "${!name:-}" ]] || [[ -e "${!name}" ]] || fail "$name=${!name} does not exist"
done

cleanup() {
    [[ -n "${REGISTRY_CID:-}" ]] && docker rm -f "$REGISTRY_CID" >/dev/null 2>&1 || true
    [[ -n "${UBLKD_PID:-}" ]] && kill "$UBLKD_PID" >/dev/null 2>&1 || true
    ip netns list 2>/dev/null | awk '{print $1}' | grep '^fsb' | xargs -r -n1 ip netns del 2>/dev/null || true
    ip -o link show master fsb0 2>/dev/null | awk -F: '{print $2}' | tr -d ' ' | xargs -r -n1 ip link del 2>/dev/null || true
}
trap cleanup EXIT
# Route signals through the EXIT trap so Ctrl-C and CI timeouts still clean up.
trap 'exit 130' INT
trap 'exit 143' TERM

# --- 1. build fsb-vm and put it on PATH --------------------------------------
log "building fsb-vm"
(cd "$REPO_ROOT" && go build -o "$BIN_DIR/fsb-vm" ./cmd/fsb-vm/)
export PATH="$BIN_DIR:$PATH"

# --- 2. provision the overlaybd tool set -------------------------------------
if [[ -x "$OCIToolsDir/current/bin/overlaybd-merge" && -x "$OCIToolsDir/current/bin/overlaybd-commit" ]]; then
    log "oci-tools already installed at $OCIToolsDir/current"
elif [[ -n "${OCI_TOOLS_DIR:-}" ]]; then
    log "installing oci-tools from $OCI_TOOLS_DIR"
    "$REPO_ROOT/build/oci/install-tools.sh" "$OCI_TOOLS_DIR" "$OCIToolsDir"
elif [[ -n "${OCI_TOOLS_IMAGE:-}" ]]; then
    log "extracting oci-tools from image $OCI_TOOLS_IMAGE"
    cid="$(docker create "$OCI_TOOLS_IMAGE")"
    mkdir -p "$WORK/oci-tools"
    docker cp "$cid":/. "$WORK/oci-tools/" >/dev/null
    docker rm "$cid" >/dev/null
    sha_sums="$(find "$WORK/oci-tools" -name SHA256SUMS -print -quit)"
    [[ -n "$sha_sums" ]] || fail "no packaged tools (SHA256SUMS) in $OCI_TOOLS_IMAGE"
    "$REPO_ROOT/build/oci/install-tools.sh" "$(dirname "$sha_sums")" "$OCIToolsDir"
else
    log "building overlaybd tools from source (first run takes ~10-20 minutes; set OCI_TOOLS_DIR to skip)"
    mkdir -p "$WORK/overlaybd-src" "$WORK/oci-tools"
    docker run --rm -v "$REPO_ROOT":/src -v "$WORK/overlaybd-src":/out -w /src \
        ubuntu:24.04 sh /src/build/oci/build-overlaybd.sh /out
    (cd "$WORK/overlaybd-src" && "$REPO_ROOT/build/oci/package-tools.sh" \
        "$WORK/overlaybd-src" "$BIN_DIR/fsb-vm" "$WORK/oci-tools")
    "$REPO_ROOT/build/oci/install-tools.sh" "$WORK/oci-tools" "$OCIToolsDir"
fi
[[ -x "$OCIToolsDir/current/bin/overlaybd-merge" ]] || fail "overlaybd-merge missing after tool installation"

# --- 3. start overlaybd-ublkd on the fast-sandbox socket ---------------------
mkdir -p "$OCISocketDir" "$WORK/ublkd-cache"
chmod 0700 "$OCISocketDir"
rm -f "$OCISocketDir/ublkd.sock"
cat > "$OCISocketDir/service.json" <<EOF_JSON
{
  "logConfig": {"logLevel": 1, "logPath": "$RUN_DIR/ublkd.log"},
  "cacheConfig": {"cacheType": "file", "cacheDir": "$WORK/ublkd-cache", "cacheSizeGB": 8},
  "gzipCacheConfig": {"enable": false},
  "credentialConfig": {"mode": "file", "path": "$OCISocketDir/cred.json"},
  "ioEngine": 0, "workpoolSize": 8,
  "download": {"enable": false}, "p2pConfig": {"enable": false},
  "registryFsVersion": "v2", "serviceConfig": {"enable": false}
}
EOF_JSON
"$OCIToolsDir/current/bin/overlaybd-ublkd" --socket-path "$OCISocketDir/ublkd.sock" \
    --service-config "$OCISocketDir/service.json" --cache-dir "$WORK/ublkd-cache" \
    >"$RUN_DIR/ublkd-stdout.log" 2>&1 &
UBLKD_PID=$!
for _ in $(seq 1 50); do
    [[ -S "$OCISocketDir/ublkd.sock" ]] && break
    kill -0 "$UBLKD_PID" 2>/dev/null || fail "ublkd exited during startup (see $RUN_DIR/ublkd.log)"
    sleep 0.2
done
[[ -S "$OCISocketDir/ublkd.sock" ]] || fail "ublkd socket never appeared"
pass "overlaybd-ublkd running (pid $UBLKD_PID)"

# --- 4. local registry + template seed ---------------------------------------
log "starting local registry on $REGISTRY"
REGISTRY_CID="$(docker run -d --rm -p "${REGISTRY_PORT}:5000" "$REGISTRY_IMAGE")"
for _ in $(seq 1 50); do
    curl -fsS "http://${REGISTRY}/v2/" >/dev/null 2>&1 && break
    sleep 0.2
done
curl -fsS "http://${REGISTRY}/v2/" >/dev/null || fail "local registry never became ready"

log "seeding $CHAIN_SOURCE_IMAGE into the local registry"
(cd "$REPO_ROOT" && go run ./test/e2e/support/registryseed \
    -from "$CHAIN_SOURCE_IMAGE" \
    -to "${REGISTRY}/${TEMPLATE_REPO}:base" \
    -compiled-out "$RUN_DIR/registry.json")
export FAST_SANDBOX_OCI_ENABLED=true
export FAST_SANDBOX_REGISTRY_CONFIG_PATH="$RUN_DIR/registry.json"
export FAST_SANDBOX_OCI_PLAIN_HTTP=1
export FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY="${REGISTRY}/${SNAPSHOT_REPO}"

# fsb-vm credential file: an anonymous entry for the loopback registry, both
# for the template repository and the snapshot repository (base64 ":").
printf '{"auths":{"%s/templates":{"auth":"Og=="},"%s/snapshots":{"auth":"Og=="}}}\n' \
    "$REGISTRY" "$REGISTRY" > "$OCISocketDir/cred.json"
pass "registry ready and credentials staged"

# --- 5. build the OCI template pair (real cold boot + full snapshot) ---------
convertor="${CONVERTOR:-}"
if [[ -z "$convertor" ]]; then
    for candidate in "$OCIToolsDir/current/bin/convertor" /opt/overlaybd/snapshotter/convertor /usr/local/bin/convertor; do
        if [[ -x "$candidate" ]]; then
            convertor="$candidate"
            break
        fi
    done
fi
[[ -n "$convertor" && -x "$convertor" ]] || fail "overlaybd convertor binary not found (set CONVERTOR); a plain OCI source needs it to build the template"
tools_bin="$OCIToolsDir/current/bin"
if curl -fsS "http://${REGISTRY}/v2/${TEMPLATE_REPO}/manifests/tpl_snapfiles" >/dev/null 2>&1; then
    log "template pair already present, skipping build"
else
    rm -rf "$WORK/artifacts/${TEMPLATE_REPO}_tpl"
    log "building the OCI template pair from $CHAIN_SOURCE_IMAGE (one cold boot)"
    fsb-vm build --plain-http \
        --firecracker "$FC_BINARY" \
        --ublkd-socket "$OCISocketDir/ublkd.sock" \
        --cred-file "$OCISocketDir/cred.json" \
        --overlaybd-create "$tools_bin/overlaybd-create" \
        --overlaybd-commit "$tools_bin/overlaybd-commit" \
        --overlaybd-merge "$tools_bin/overlaybd-merge" \
        --overlaybd-resize "$tools_bin/overlaybd-resize" \
        --overlaybd-convertor "$convertor" \
        --source "oci://${REGISTRY}/${TEMPLATE_REPO}:base" \
        --output "templates:tpl" \
        --kernel "$FC_KERNEL" \
        --vcpu 1 --memory-mb 512 --disk-size-gb 2 \
        --build-work-dir "$WORK/builds" --build-output-dir "$WORK/artifacts" \
        2>&1 | tee "$RUN_DIR/build.log" || fail "fsb-vm build failed (see $RUN_DIR/build.log)"
    fsb-vm push --plain-http \
        --artifact-dir "$WORK/artifacts/templates_tpl" \
        --destination "${REGISTRY}/${TEMPLATE_REPO}:tpl" \
        --auth-file "$OCISocketDir/cred.json" \
        || fail "fsb-vm push failed"
fi
curl -fsS "http://${REGISTRY}/v2/${TEMPLATE_REPO}/manifests/tpl_snapfiles" >/dev/null || fail "template pair not resolvable after build/push"
pass "template pair published at $TEMPLATE_REF"

# --- 6. the pause/publish/restore chain ---------------------------------------
log "running $CYCLES pause/publish/restore cycles (chain bound asserted every cycle)"
# A failing go test must not abort the script before the summary diagnostics.
(cd "$REPO_ROOT" && env \
    FC_BINARY="${FC_BINARY}" FC_JAILER="${FC_JAILER:-}" FC_KERNEL="$FC_KERNEL" \
    FC_ROOTFS="${FC_ROOTFS:-}" FC_STATE_ROOT="$WORK/state" \
    FC_OCI_TEMPLATE_REF="$TEMPLATE_REF" \
    FC_CHAIN_CYCLES="$CYCLES" \
    PATH="$BIN_DIR:$PATH" \
    go test -tags firecracker -count=1 -v -timeout 24h \
    -run '^TestFirecrackerOCICheckpointChainE2E$' \
    ./internal/runtime/firecracker/) 2>&1 | tee "$RUN_DIR/chain-test.log" || true

echo
echo "== per-cycle chain length and stage timings =="
grep -E 'CHAIN (cycle|summary)' "$RUN_DIR/chain-test.log" || true
if grep -q '^ok' "$RUN_DIR/chain-test.log"; then
    pass "chain stayed bounded across $CYCLES cycles"
else
    fail "chain E2E test did not pass (see $RUN_DIR/chain-test.log)"
fi
