#!/usr/bin/env bash
# firecracker-mem-backend-check.sh — verify the write-through semantics of
# Firecracker file-backed memory.
#
# Background: the memory capture mechanism for runtime incremental
# snapshots depends on the mmap semantics of mem_backend=File at
# snapshot restore time:
#   - MAP_SHARED (write-through): guest writes eventually land back in
#     the memory file -> snapshot = pause + flush + seal upper layer,
#     no ptrace involved;
#   - MAP_PRIVATE (COW): guest writes stay in the process's anonymous
#     memory and the file is untouched -> snapshot must export dirty
#     ranges (process_vm_readv), which involves ptrace handling.
#
# Method (no guest interaction, no network, no root required):
#   1. Cold-boot VM1, Pause, create a Full snapshot (memory.snap +
#      vmstate.snap)
#   2. Copy memory.snap to restore-mem.bin (observable copy, identical
#      initial content)
#   3. Restore VM2 with mem_backend.File pointing at restore-mem.bin and
#      let it keep running
#   4. Decision (authoritative): mapping flags of restore-mem.bin in
#      /proc/<pid>/maps
#        r--s / rw-s  => MAP_SHARED (write-through)
#        r--p / rw-p  => MAP_PRIVATE (COW)
#   5. Corroboration 1: Shared_Dirty vs Private_Dirty for that mapping
#      in /proc/<pid>/smaps
#   6. Corroboration 2: compare restore-mem.bin with memory.snap after
#      the guest runs for a while (write-through -> differs; COW ->
#      identical)
#
# Env overrides (same as firecracker-e2e.sh):
#   FC_VERSION    firecracker release tag, default v1.16.1
#   FC_BINARY     firecracker binary path (skips download when set)
#   FC_KERNEL     kernel image path (skips download when set)
#   FC_ROOTFS     rootfs ext4 image path (skips download when set)
#   WORK          work dir, default $PWD/.fc-mem-backend-check
#   MEM_MIB       guest memory MiB, default 512
#
# Usage: ./scripts/firecracker-mem-backend-check.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.fc-mem-backend-check}"
BIN_DIR="$WORK/bin"
ART_DIR="$WORK/artifacts"
VM1_DIR="$WORK/vm1"
VM2_DIR="$WORK/vm2"
FC_VERSION="${FC_VERSION:-v1.16.1}"
FC_BINARY="${FC_BINARY:-$BIN_DIR/firecracker}"
FC_KERNEL="${FC_KERNEL:-$ART_DIR/vmlinux.bin}"
FC_ROOTFS="${FC_ROOTFS:-$ART_DIR/bionic.rootfs.ext4}"
MEM_MIB="${MEM_MIB:-512}"

BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off ro"

log() { printf '\033[1;34m[mem-backend]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[mem-backend] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# --- API helpers (fail loudly with HTTP code + response body) -----------------
api_call() { # sock method path payload desc
    local sock="$1" method="$2" path="$3" payload="$4" desc="${5:-$3}"
    local body code rc
    body="$(mktemp)"
    rc=0
    code="$(curl -s --unix-socket "$sock" -o "$body" -w '%{http_code}' -X "$method" -d "$payload" "http://localhost$path")" || rc=$?
    if [[ $rc -ne 0 ]]; then
        rm -f "$body"
        die "PUT $desc connection failed (curl rc=$rc, sock=$sock)"
    fi
    if [[ "$code" == "000" || "$code" -ge 400 ]]; then
        local errbody
        errbody="$(cat "$body" 2>/dev/null || true)"
        rm -f "$body"
        die "PUT $desc -> HTTP $code: ${errbody:-(empty response)}"
    fi
    rm -f "$body"
}

api_put() { # sock path payload desc
    api_call "$1" PUT "$2" "$3" "$4"
}

api_patch() { # sock path payload desc
    api_call "$1" PATCH "$2" "$3" "$4"
}

api_get() { # sock path desc -> body (stdout), dies on failure
    local sock="$1" path="$2" desc="${3:-$2}"
    local body code rc
    body="$(mktemp)"
    rc=0
    code="$(curl -s --unix-socket "$sock" -o "$body" -w '%{http_code}' "http://localhost$path")" || rc=$?
    if [[ $rc -ne 0 ]]; then
        rm -f "$body"
        die "GET $desc connection failed (curl rc=$rc, sock=$sock)"
    fi
    if [[ "$code" == "000" || "$code" -ge 400 ]]; then
        local errbody
        errbody="$(cat "$body" 2>/dev/null || true)"
        rm -f "$body"
        die "GET $desc -> HTTP $code: ${errbody:-(empty response)}"
    fi
    cat "$body"
    rm -f "$body"
}

# --- artifacts ---------------------------------------------------------------
download() { # url target
    log "downloading $2"
    curl -fL --retry 3 -o "$2.tmp" "$1" || die "download failed: $1"
    mv "$2.tmp" "$2"
}

ensure_artifacts() {
    mkdir -p "$BIN_DIR" "$ART_DIR" "$VM1_DIR" "$VM2_DIR"
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
    "$FC_BINARY" --version >/dev/null || die "firecracker binary does not run"
    [[ -f "$FC_KERNEL" ]] || download "https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin" "$FC_KERNEL"
    [[ -f "$FC_ROOTFS" ]] || download "https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/rootfs/bionic.rootfs.ext4" "$FC_ROOTFS"
    log "artifacts: $("$FC_BINARY" --version | head -1)"
}

# --- firecracker instance management -----------------------------------------
launch_fc() { # name dir
    local name="$1" dir="$2"
    local sock="$dir/api.sock" pidfile="$dir/fc.pid" logfile="$dir/fc.log"
    rm -f "$sock"
    "$FC_BINARY" --id "$name" --api-sock "$sock" >"$logfile" 2>&1 &
    echo $! >"$pidfile"
    wait_for_socket "$sock"
    sleep 0.3
    if ! kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        die "firecracker exited right after launch, log: $(cat "$logfile")"
    fi
}

start_cold() { # name dir
    local name="$1" dir="$2"
    local sock="$dir/api.sock"
    launch_fc "$name" "$dir"
    api_put "$sock" /machine-config "{\"vcpu_count\":1,\"mem_size_mib\":$MEM_MIB}" machine-config
    api_put "$sock" /boot-source "{\"kernel_image_path\":\"$FC_KERNEL\",\"boot_args\":\"$BOOT_ARGS\"}" boot-source
    api_put "$sock" /drives/root "{\"drive_id\":\"root\",\"path_on_host\":\"$FC_ROOTFS\",\"is_root_device\":true,\"is_read_only\":true}" drives/root
    api_put "$sock" /actions '{"action_type":"InstanceStart"}' InstanceStart
}

start_restore() { # name dir vmstate memfile
    local name="$1" dir="$2" vmstate="$3" memfile="$4"
    local sock="$dir/api.sock"
    launch_fc "$name" "$dir"
    api_put "$sock" /snapshot/load "{\"snapshot_path\":\"$vmstate\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$memfile\"},\"resume_vm\":true}" snapshot/load
}

wait_for_socket() { # sock
    local sock="$1"
    for _ in $(seq 1 100); do
        [[ -S "$sock" ]] && return 0
        sleep 0.1
    done
    die "firecracker API socket $sock did not appear"
}

vm_state() { # sock -> state string
    api_get "$1" / "instance info" 2>/dev/null \
        | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])' 2>/dev/null || echo unknown
}

wait_running() { # sock
    local sock="$1"
    for _ in $(seq 1 120); do
        [[ "$(vm_state "$sock")" == "Running" ]] && return 0
        sleep 0.5
    done
    die "VM did not reach Running (fc.log: $(tail -3 "$VM1_DIR/fc.log" 2>/dev/null || true))"
}

wait_paused() { # sock
    local sock="$1"
    for _ in $(seq 1 60); do
        [[ "$(vm_state "$sock")" == "Paused" ]] && return 0
        sleep 0.25
    done
    die "VM did not reach Paused (state=$(vm_state "$sock"))"
}

snapshot_create() { # sock dir
    local sock="$1" dir="$2"
    api_patch "$sock" /vm '{"state":"Paused"}' Pause
    wait_paused "$sock"
    local payload
    payload="{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$dir/vmstate.snap\",\"mem_file_path\":\"$dir/memory.snap\"}"
    api_put "$sock" /snapshot/create "$payload" snapshot/create
    log "snapshot created: $dir/vmstate.snap + $dir/memory.snap"
}

stop_vm() { # dir
    local dir="$1"
    [[ -f "$dir/fc.pid" ]] || return 0
    kill "$(cat "$dir/fc.pid")" 2>/dev/null || true
    sleep 1
}

cleanup() {
    stop_vm "$VM1_DIR"
    stop_vm "$VM2_DIR"
}
trap cleanup EXIT

# --- main flow ----------------------------------------------------------------
[[ "$(uname -s)" == "Linux" ]] || die "requires Linux"
[[ -e /dev/kvm ]] || die "/dev/kvm is missing"
ensure_artifacts

log "=== 1/4 cold-boot VM1 (1 vCPU / ${MEM_MIB} MiB) and create Full snapshot ==="
start_cold "memcheck-vm1" "$VM1_DIR"
wait_running "$VM1_DIR/api.sock"
log "VM1 Running; let it run ${MEM_WARM_SECONDS:-8}s so kernel memory gains content"
sleep "${MEM_WARM_SECONDS:-8}"
snapshot_create "$VM1_DIR/api.sock" "$VM1_DIR"
stop_vm "$VM1_DIR"

log "=== 2/4 copy memory.snap to the observable copy restore-mem.bin ==="
RESTORE_MEM="$VM2_DIR/restore-mem.bin"
cp "$VM1_DIR/memory.snap" "$RESTORE_MEM"
log "restore-mem.bin: $(stat -c%s "$RESTORE_MEM") bytes (initial content identical to memory.snap)"

log "=== 3/4 restore VM2 (mem_backend.File = restore-mem.bin) ==="
start_restore "memcheck-vm2" "$VM2_DIR" "$VM1_DIR/vmstate.snap" "$RESTORE_MEM"
wait_running "$VM2_DIR/api.sock"
log "VM2 Running"

log "=== 4/4 verdict ==="
PID="$(cat "$VM2_DIR/fc.pid")"
MAP_LINE="$(grep -F "restore-mem.bin" "/proc/$PID/maps" | head -1)"
[[ -n "$MAP_LINE" ]] || die "restore-mem.bin mapping not found in /proc/$PID/maps"
FLAGS="$(echo "$MAP_LINE" | awk '{print $2}')"
log "maps entry: $(echo "$MAP_LINE" | awk '{print $1, $2, $6}')"

SHARED="no"
if [[ "$FLAGS" == *s ]]; then
    SHARED="yes"
    log "check 1 (maps flags $FLAGS): MAP_SHARED -- write-through (guest writes land in the file)"
else
    log "check 1 (maps flags $FLAGS): MAP_PRIVATE -- COW (guest writes stay in anonymous memory)"
fi

SMAPS_LINE="$(awk -v RS='' -v pat="restore-mem.bin" '$0 ~ pat {print}' "/proc/$PID/smaps" | head -1)"
SHARED_DIRTY="$(echo "$SMAPS_LINE" | grep -E '^\s*Shared_Dirty:' | awk '{print $2}' || true)"
PRIVATE_DIRTY="$(echo "$SMAPS_LINE" | grep -E '^\s*Private_Dirty:' | awk '{print $2}' || true)"
log "check 2 (smaps): Shared_Dirty=${SHARED_DIRTY:-0} kB, Private_Dirty=${PRIVATE_DIRTY:-0} kB"
if [[ -n "$SHARED_DIRTY" && "${SHARED_DIRTY:-0}" -gt 0 ]]; then
    log "  -> Shared_Dirty pages present: write-through (MAP_SHARED) corroborated"
elif [[ -n "${PRIVATE_DIRTY:-0}" && "${PRIVATE_DIRTY:-0}" -gt 0 ]]; then
    log "  -> Private_Dirty pages present: COW (MAP_PRIVATE) corroborated"
fi

log "check 3 (data comparison): let the guest run ${MEM_WARM_SECONDS:-8}s more, then compare files"
sleep "${MEM_WARM_SECONDS:-8}"
if cmp -s "$RESTORE_MEM" "$VM1_DIR/memory.snap"; then
    log "  -> restore-mem.bin identical to memory.snap: guest writes did not reach the file (COW corroborated)"
else
    log "  -> restore-mem.bin differs from memory.snap: guest writes reached the file (write-through corroborated)"
fi

echo
log "=============================================================="
if [[ "$SHARED" == "yes" ]]; then
    log "verdict: file-backed memory is MAP_SHARED (write-through)"
    log "         runtime incremental snapshot: pause + flush + seal memory upper layer, no ptrace"
else
    log "verdict: file-backed memory is MAP_PRIVATE (COW)"
    log "         runtime incremental snapshot: dirty-range export (process_vm_readv) required,"
    log "         prefer driver-as-parent reading; agent privilege escalation is the fallback"
fi
log "=============================================================="
