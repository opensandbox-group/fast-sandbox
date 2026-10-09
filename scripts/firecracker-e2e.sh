#!/usr/bin/env bash
# firecracker-e2e.sh — run the Firecracker runtime driver E2E on a Linux host.
#
# Requirements:
#   - Linux x86_64 with KVM (/dev/kvm) and /dev/net/tun
#   - root (netns, tap, iptables setup) — the script re-invokes itself with sudo
#   - ip, iptables, sysctl, ping, tcpdump, tar, curl
#
# The script downloads a firecracker release and the firecracker quickstart
# kernel/rootfs, then runs:
#   go test -tags firecracker -run TestFirecrackerDriverE2E ./internal/runtime/firecracker/
#
# Restore is the only startup path: the kernel and rootfs are snapshot-prep
# assets — the test cold-boots one preparation VM to produce the golden
# snapshot set (rootfs.img + vmstate.snap + memory.snap + manifest.json)
# under FC_STATE_ROOT, and every Sandbox restores from it (no kernel at
# runtime). The prepared set is cached under the StateRoot, so reruns with a
# persistent FC_STATE_ROOT skip the prep boot.
#
# Host impact: the test uses a private 172.30.0.0/24 bridge and sets host-wide
# ip_forward plus one MASQUERADE rule; these are restored automatically on
# exit (only when they did not exist before the run). --cleanup is available
# for interrupted runs.
#
# Overrides (environment):
#   FC_VERSION    firecracker release tag, default v1.16.1 (latest upstream stable)
#   FC_BINARY     firecracker binary path (skips download when set)
#   FC_JAILER     jailer binary path (skips download when set); empty disables
#                 the jailer (direct launch, no per-clone netns/chroot)
#   FC_KERNEL     kernel image path, snapshot-prep asset (skips download when set)
#   FC_ROOTFS     converted rootfs ext4 image, snapshot-prep input (skips download when set)
#   FC_STATE_ROOT driver StateRoot; defaults to $WORK/state-root so the
#                 prepared golden snapshot set is reused across runs (use a
#                 reflink-capable filesystem to avoid the first-fsync
#                 writeback cost)
#   WORK          workspace dir, default $PWD/.firecracker-e2e
#
# Version baseline: latest upstream stable firecracker (vanilla). The driver
# client only uses the stable v1.x snapshot API surface; the version matters
# once snapshot ABI compatibility is pinned per the runtime manifest.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.firecracker-e2e}"
BIN_DIR="$WORK/bin"
ART_DIR="$WORK/artifacts"
FC_VERSION="${FC_VERSION:-v1.16.1}"
FC_BINARY="${FC_BINARY:-$BIN_DIR/firecracker}"
FC_JAILER="${FC_JAILER:-$BIN_DIR/jailer}"
FC_KERNEL="${FC_KERNEL:-$ART_DIR/vmlinux.bin}"
FC_ROOTFS="${FC_ROOTFS:-$ART_DIR/bionic.rootfs.ext4}"
FC_STATE_ROOT="${FC_STATE_ROOT:-$WORK/state-root}"

PRIVATE_CIDR="172.30.0.0/24"
BRIDGE_NAME="fsb0"

log() { printf '\033[1;34m[e2e]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[e2e] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# --- cleanup -----------------------------------------------------------------
# Every resource this E2E family creates is derived from a per-run Pod UID:
#   netns      fsb<64 hex>      resourceName("fsb", podUID, slotID, 63)
#   host veth  fh<13 hex>       resourceName("fh", podUID, slotID, 15)
# (the guest tap vmtap0 lives INSIDE the slot netns and vanishes with it;
# fc* host taps only exist as leftovers of older E2E versions).
# Preparation owns an isolated fc-prep-<hex> netns.
# Stale copies from earlier runs (crashed script, interrupted test) survive
# `ip netns del` and carry the same private addresses, which corrupts ARP on
# the shared bridge. purge_fsb_resources removes every fsb netns and every
# fsb0-attached fh/fc device so a rerun starts from a clean host. The bridge
# and its own address are restored separately by cleanup_host_state.

fsb_netns_pattern='^fsb[0-9a-f]{32,}$'
fsb0_dev_pattern='^(fc|fh)[0-9a-f]{10,}$'
PRE_RUN_NETNS="$WORK/pre-run-netns"

is_live_netns() { ip netns list 2>/dev/null | awk '{print $1}' | grep -qx "$1"; }

kill_firecracker_vms() {
    # Firecracker processes this E2E family launched carry --id e2e-sandbox-*.
    # The jailer execs firecracker (same PID), so matching the firecracker
    # argv covers both launch modes.
    for pid in $(pgrep -f "firecracker .*--id e2e-(sandbox-|prep)" 2>/dev/null || true); do
        kill "$pid" 2>/dev/null || true
    done
    # Netns deletion races the dying VMM holding the namespace (EBUSY), so
    # wait a moment for the processes to exit before tearing devices down.
    sleep 1
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

purge_fsb_resources() {
    kill_firecracker_vms
    for path in /var/run/netns/fsb* /var/run/netns/fc-prep-*; do
        [[ -e "$path" ]] || continue
        name="$(basename "$path")"
        [[ "$name" =~ $fsb_netns_pattern ]] || [[ "$name" =~ ^fc-prep-[0-9a-f]+$ ]] || continue
        if is_live_netns "$name"; then
            for attempt in 1 2 3; do
                ip netns del "$name" 2>/dev/null && break
                sleep 0.2
            done
        fi
        if [[ -e "$path" ]]; then
            rm -f "$path"
            log "removed stale netns $name"
        fi
    done
    # Host-side veths/taps left on the bridge by earlier runs (or by the
    # firecracker fallback tap naming fc-<10 hex>) keep stale addresses alive.
    for dev in $(ip -o link show master "$BRIDGE_NAME" 2>/dev/null | awk -F'[ :]+' '{print $2}' | sort -u); do
        [[ "$dev" =~ $fsb0_dev_pattern ]] || [[ "$dev" =~ ^fc-[0-9a-f]{10}$ ]] || continue
        ip link del "$dev" 2>/dev/null || true
        log "removed stale bridge device $dev"
    done
}

record_pre_run_netns() {
    ip netns list 2>/dev/null | awk '{print $1}' | sort > "$PRE_RUN_NETNS"
}

# Records the pre-existing host state so cleanup only removes what the test
# created (the bridge, MASQUERADE, and ip_forward are restored by
# cleanup_host_state; per-slot resources are always purged).
record_host_state() {
    _forward_before="$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 1)"
    _bridge_before="no"
    if ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then _bridge_before="yes"; fi
    _masq_before="no"
    if iptables -t nat -C POSTROUTING -s "$PRIVATE_CIDR" ! -d "$PRIVATE_CIDR" -j MASQUERADE 2>/dev/null; then _masq_before="yes"; fi
}

cleanup_leftovers() {
    # Per-slot resources of this and earlier runs: netns, taps, veths, and
    # jailer chroot dirs. The bridge and host-wide rules are restored by
    # cleanup_host_state.
    purge_fsb_resources
    purge_jail_dirs
}

cleanup_host_state() {
    # Restore only what the test created.
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

# --- re-invoke as root -------------------------------------------------------
if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null; then
        log "re-invoking as root (sudo -E)"
        exec sudo -E env "PATH=$PATH" "WORK=$WORK" "FC_VERSION=$FC_VERSION" \
            "FC_BINARY=$FC_BINARY" "FC_JAILER=$FC_JAILER" "FC_KERNEL=$FC_KERNEL" \
            "FC_ROOTFS=$FC_ROOTFS" \
            "FC_STATE_ROOT=$FC_STATE_ROOT" \
            "$0" "$@"
    fi
    die "must run as root (or install sudo)"
fi

# --- explicit cleanup mode ---------------------------------------------------
if [[ "${1:-}" == "--cleanup" ]]; then
    record_host_state
    cleanup_leftovers
    cleanup_host_state
    log "host cleanup done (netns, firecracker/jailer processes, jail dirs, $BRIDGE_NAME bridge, MASQUERADE rule)"
    exit 0
fi

# --- host prechecks ----------------------------------------------------------
[[ "$(uname -s)" == "Linux" ]] || die "requires Linux, got $(uname -s)"
[[ "$(uname -m)" == "x86_64" ]] || die "requires x86_64, got $(uname -m)"
[[ -e /dev/kvm ]] || die "/dev/kvm is missing (enable KVM/nested virt)"
[[ -w /dev/kvm ]] || die "/dev/kvm is not writable"
[[ -e /dev/net/tun ]] || die "/dev/net/tun is missing"
for cmd in ip iptables sysctl ping tcpdump tar curl; do
    command -v "$cmd" >/dev/null || die "missing required command: $cmd"
done

mkdir -p "$BIN_DIR" "$ART_DIR" /run/netns /run/fast-sandbox/netns

# --- artifacts ---------------------------------------------------------------
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
    cp "$found" "$FC_BINARY"
    chmod +x "$FC_BINARY"
    rm -rf "$WORK/extract"
fi
"$FC_BINARY" --version >/dev/null || die "firecracker binary does not run (needs recent glibc)"

if [[ ! -x "$FC_JAILER" ]]; then
    tarball="$WORK/firecracker-$FC_VERSION-x86_64.tgz"
    if [[ ! -f "$tarball" ]]; then
        download "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz" "$tarball"
    fi
    mkdir -p "$WORK/extract"
    tar -xzf "$tarball" -C "$WORK/extract"
    found="$(find "$WORK/extract" -type f -name "jailer-v${FC_VERSION#v}-x86_64" | head -1)"
    [[ -n "$found" ]] || die "jailer binary not found in release tarball"
    cp "$found" "$FC_JAILER"
    chmod +x "$FC_JAILER"
    rm -rf "$WORK/extract"
fi
"$FC_JAILER" --version >/dev/null || die "jailer binary does not run"

if [[ ! -f "$FC_KERNEL" ]]; then
    # Snapshot-prep asset: the kernel only boots the one preparation VM that
    # produces the golden snapshot set; restored Sandboxes never boot a kernel.
    download "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260722-38359b8055fc-0/x86_64/vmlinux-6.1.176" "$FC_KERNEL"
fi
if [[ ! -f "$FC_ROOTFS" ]]; then
    # Snapshot-prep input: the preparation VM's root drive; the golden
    # rootfs.img in the StateRoot is derived from it.
    download "https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/rootfs/bionic.rootfs.ext4" "$FC_ROOTFS"
fi

log "firecracker: $FC_BINARY ($("$FC_BINARY" --version | head -1))"
log "jailer:      $FC_JAILER (per-clone netns + chroot)"
log "kernel:     $FC_KERNEL (snapshot-prep asset)"
log "rootfs:     $FC_ROOTFS (snapshot-prep input)"
log "state root: $FC_STATE_ROOT (golden snapshot set cached here)"

# --- run the test ------------------------------------------------------------
record_host_state
# Purge leftovers of earlier runs (stale fsb netns with duplicate private
# addresses corrupt ARP on the bridge) before recording the pre-run set.
purge_fsb_resources
record_pre_run_netns
# On any exit: remove run-created netns/taps/firecracker processes, then
# restore host resources (bridge, MASQUERADE, ip_forward) that did not exist
# before the run. --cleanup remains available for interrupted runs.
trap 'cleanup_leftovers; cleanup_host_state' EXIT
log "running TestFirecrackerDriverE2E (+NoInfra/Concurrent/Serial/ImageGC/Leak, root)"
cd "$REPO_ROOT"
env FC_BINARY="$FC_BINARY" FC_JAILER="$FC_JAILER" FC_KERNEL="$FC_KERNEL" \
    FC_ROOTFS="$FC_ROOTFS" \
    FC_STATE_ROOT="$FC_STATE_ROOT" \
    FC_LEAK_ROUNDS="${FC_LEAK_ROUNDS:-}" \
    go test -tags firecracker -count=1 -v -run '^TestFirecrackerDriverE2E' \
    ./internal/runtime/firecracker/

log "E2E passed"
log "host resources created by this run (netns, taps, rules) are restored automatically on exit"
log "per-sandbox stage timing is printed by the driver above (acquire/rootfs/launch/configure/boot)"
log "run the same command with --cleanup after an interrupted run"
