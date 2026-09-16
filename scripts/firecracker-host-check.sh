#!/usr/bin/env bash
# firecracker-host-check.sh — standalone Firecracker host-readiness check.
#
# Runs the same checks the firecracker-runtime DaemonSet's readiness loop
# (internal/runtime/firecracker/agent/hostready) performs before it labels
# a node schedulable, but as a plain script an operator can run on any
# machine BEFORE installing anything:
#
#   ./scripts/firecracker-host-check.sh
#   ./scripts/firecracker-host-check.sh --state-root /data/fast-sandbox/firecracker
#
# Checked (hard failures block a node from being labeled ready):
#   cpu-arch               x86_64 (arm64 not supported yet)
#   kernel-version         Linux >= 5.10 (LTS, the Firecracker CI baseline)
#   kvm-device             /dev/kvm exists, is a character device, opens RW
#   net-tun                /dev/net/tun exists, opens RW
#   stateroot-dirs         the StateRoot layout is creatable
#   stateroot-filesystem   free space >= --min-free
#   fc-assets              installed firecracker/jailer --version run and
#                          vmlinux.bin is non-empty (fail when absent)
# Warned (informational, not blocking):
#   nested-virtualization  vmx/svm CPU flag (nested VMs cannot run FC without it)
#   memory-available       MemAvailable >= --min-memory
#   stateroot-filesystem   fs type + reflink (CoW) support report
#
# Exit code: 0 = every hard check passed (node would be labeled ready),
# 1 = at least one hard check failed.
set -u
set -o pipefail

STATE_ROOT="${FAST_SANDBOX_STATE_ROOT:-/var/lib/fast-sandbox/firecracker}"
ASSETS_DIR="${FAST_SANDBOX_FC_ASSETS_DIR:-/opt/fast-sandbox/firecracker}"
MIN_FREE=$((10 << 30))   # 10GiB
MIN_MEMORY=$((2 << 30))  # 2GiB

while [[ $# -gt 0 ]]; do
	case "$1" in
	--state-root)
		STATE_ROOT="$2"
		shift 2
		;;
	--assets-dir)
		ASSETS_DIR="$2"
		shift 2
		;;
	--min-free)
		MIN_FREE="$2"
		shift 2
		;;
	--min-memory)
		MIN_MEMORY="$2"
		shift 2
		;;
	-h | --help)
		sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $1 (see --help)" >&2
		exit 2
		;;
	esac
done

log() { printf '\033[1;34m[firecracker-host-check]\033[0m %s\n' "$*"; }
pass() { printf '\033[1;32m  PASS\033[0m %-24s %s\n' "$1" "$2"; }
warn() { printf '\033[1;33m  WARN\033[0m %-24s %s\n' "$1" "$2"; WARN_COUNT=$((WARN_COUNT + 1)); }
fail_check() { printf '\033[1;31m  FAIL\033[0m %-24s %s\n' "$1" "$2"; FAIL_COUNT=$((FAIL_COUNT + 1)); }

WARN_COUNT=0
FAIL_COUNT=0

if [[ "$(uname -s)" != "Linux" ]]; then
	log "this check targets Linux hosts (this machine: $(uname -s)); results may be meaningless"
fi

log "checking the host for Firecracker launch readiness"

# --- cpu-arch ------------------------------------------------------------------------
case "$(uname -m)" in
x86_64) pass "cpu-arch" "amd64 (Firecracker x86_64 assets)" ;;
aarch64 | arm64) fail_check "cpu-arch" "arm64 nodes are not supported yet (x86_64 only)" ;;
*) fail_check "cpu-arch" "unsupported architecture $(uname -m): no Firecracker assets" ;;
esac

# --- kernel-version ------------------------------------------------------------------
KERNEL_RELEASE="$(uname -r)"
KERNEL_MAJOR="$(printf '%s' "$KERNEL_RELEASE" | cut -d. -f1)"
KERNEL_MINOR="$(printf '%s' "$KERNEL_RELEASE" | cut -d. -f2)"
if [[ ! "$KERNEL_MAJOR" =~ ^[0-9]+$ || ! "$KERNEL_MINOR" =~ ^[0-9]+$ ]]; then
	fail_check "kernel-version" "unparseable release $KERNEL_RELEASE"
elif [[ "$KERNEL_MAJOR" -lt 5 || ("$KERNEL_MAJOR" -eq 5 && "$KERNEL_MINOR" -lt 10) ]]; then
	fail_check "kernel-version" "Linux $KERNEL_MAJOR.$KERNEL_MINOR: Firecracker requires >= 5.10"
else
	pass "kernel-version" "Linux $KERNEL_MAJOR.$KERNEL_MINOR ($KERNEL_RELEASE)"
fi

# --- kvm-device ----------------------------------------------------------------------
if [[ ! -e /dev/kvm ]]; then
	fail_check "kvm-device" "/dev/kvm does not exist (modprobe kvm_intel / kvm_amd, or missing device passthrough)"
elif [[ ! -c /dev/kvm ]]; then
	fail_check "kvm-device" "/dev/kvm is not a character device"
elif ! (exec 3<>/dev/kvm) 2>/dev/null; then
	fail_check "kvm-device" "/dev/kvm cannot be opened read-write (check permissions / cgroup device rules)"
else
	pass "kvm-device" "/dev/kvm opens read-write"
fi

# --- nested-virtualization (warning) --------------------------------------------------
if grep -qE '^(flags|Features)' /proc/cpuinfo 2>/dev/null; then
	if grep -qE '^flags.*\bvmx\b' /proc/cpuinfo; then
		pass "nested-virtualization" "vmx (Intel VT-x)"
	elif grep -qE '^flags.*\bsvm\b' /proc/cpuinfo; then
		pass "nested-virtualization" "svm (AMD-V)"
	elif [[ "$(uname -m)" == "aarch64" ]]; then
		pass "nested-virtualization" "n/a on arm64"
	else
		warn "nested-virtualization" "no vmx/svm flag: bare metal is fine, nested VMs cannot run Firecracker"
	fi
else
	warn "nested-virtualization" "cpu flags unavailable"
fi

# --- net-tun --------------------------------------------------------------------------
if [[ ! -e /dev/net/tun ]]; then
	fail_check "net-tun" "/dev/net/tun does not exist (modprobe tun, or missing device passthrough)"
elif ! (exec 3<>/dev/net/tun) 2>/dev/null; then
	fail_check "net-tun" "/dev/net/tun cannot be opened read-write"
else
	pass "net-tun" "/dev/net/tun opens read-write"
fi

# --- memory-available (warning) --------------------------------------------------------
MEM_KIB="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null)"
[[ -z "$MEM_KIB" ]] && MEM_KIB="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
if [[ -z "$MEM_KIB" ]]; then
	warn "memory-available" "meminfo unavailable"
elif [[ $((MEM_KIB << 10)) -lt "$MIN_MEMORY" ]]; then
	warn "memory-available" "$((MEM_KIB >> 10)) MiB available (minimum $((MIN_MEMORY >> 20)) MiB)"
else
	pass "memory-available" "$((MEM_KIB >> 10)) MiB available (minimum $((MIN_MEMORY >> 20)) MiB)"
fi

# --- stateroot-dirs --------------------------------------------------------------------
if mkdir -p "$STATE_ROOT" "$STATE_ROOT/images" "$STATE_ROOT/cache" "$STATE_ROOT/jails" 2>/dev/null; then
	pass "stateroot-dirs" "$STATE_ROOT {images,cache,jails} ready"
else
	fail_check "stateroot-dirs" "cannot create the $STATE_ROOT layout"
fi

# --- stateroot-filesystem ---------------------------------------------------------------
FS_FREE=""
FS_TYPE=""
if STAT_OUT="$(stat -f -c '%T %f %S' "$STATE_ROOT" 2>/dev/null)"; then
	FS_TYPE="${STAT_OUT%% *}"
	STAT_REST="${STAT_OUT#* }"
	FS_BLOCKS="${STAT_REST%% *}"
	FS_BSIZE="${STAT_REST##* }"
	FS_FREE=$((FS_BLOCKS * FS_BSIZE))
elif DF_OUT="$(df -B1 -P "$STATE_ROOT" 2>/dev/null | tail -1)"; then
	FS_TYPE="unknown"
	FS_FREE="$(printf '%s' "$DF_OUT" | awk '{print $4}')"
fi
if [[ -z "$FS_FREE" ]]; then
	fail_check "stateroot-filesystem" "cannot stat the filesystem of $STATE_ROOT"
elif [[ "$FS_FREE" -lt "$MIN_FREE" ]]; then
	fail_check "stateroot-filesystem" "type $FS_TYPE, $((FS_FREE >> 30)) GiB free (minimum $((MIN_FREE >> 30)) GiB)"
else
	# Reflink support: xfs/btrfs CoW-clone the sandbox rootfs in ~ms; on
	# other filesystems every copy is a full write.
	case "$FS_TYPE" in
	xfs | btrfs)
		pass "stateroot-filesystem" "type $FS_TYPE, $((FS_FREE >> 30)) GiB free; reflink CoW supported"
		;;
	ext4 | tmpfs | overlay | unknown)
		warn "stateroot-filesystem" "type $FS_TYPE, $((FS_FREE >> 30)) GiB free; no reflink (rootfs copies fall back to full writes)"
		;;
	*)
		warn "stateroot-filesystem" "type $FS_TYPE, $((FS_FREE >> 30)) GiB free; reflink support unknown"
		;;
	esac
fi

# --- fc-assets ---------------------------------------------------------------------------
if [[ -x "$ASSETS_DIR/firecracker" && -x "$ASSETS_DIR/jailer" ]] &&
	"$ASSETS_DIR/firecracker" --version >/dev/null 2>&1 &&
	"$ASSETS_DIR/jailer" --version >/dev/null 2>&1 &&
	[[ -s "$ASSETS_DIR/vmlinux.bin" ]]; then
	pass "fc-assets" "firecracker, jailer and vmlinux.bin verified in $ASSETS_DIR"
else
	fail_check "fc-assets" "missing or broken assets in $ASSETS_DIR (the agent readiness loop installs them; a fully ready host needs them)"
fi

# --- summary -------------------------------------------------------------------------------
log "done: $FAIL_COUNT hard failure(s), $WARN_COUNT warning(s)"
if [[ "$FAIL_COUNT" -gt 0 ]]; then
	log "the host would NOT be labeled fast-sandbox.io/firecracker-node=true"
	exit 1
fi
log "the host meets the Firecracker launch requirements"
exit 0
