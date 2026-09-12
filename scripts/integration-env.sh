#!/usr/bin/env bash
# integration-env.sh — Kind single-host full-chain integration environment.
#
# Builds the fast-sandbox Firecracker on-demand loading chain on a bare-metal
# KVM host: SandboxTemplate golden-image build (builder Pod in-cluster) →
# publish MinIO → node runtime-agent (DaemonSet) → fastlet sandbox restore →
# execd /ping delivery verification.
#
# See docs/guides/firecracker-integration-environment.md and
# docs/design/firecracker-integration-environment-plan.md (this script is
# the "one-click" task 10 of the plan).
#
# Usage:
#   ./scripts/integration-env.sh up            # full environment + chain
#   ./scripts/integration-env.sh status        # component/template/pool health
#   ./scripts/integration-env.sh verify        # sandbox create + execd /ping
#   ./scripts/integration-env.sh verify-p2p    # DART data-plane evidence (stage 2)
#   ./scripts/integration-env.sh down          # teardown, host left clean
#   ./scripts/integration-env.sh --cleanup     # down after an interrupted run
#   ./scripts/integration-env.sh up --auto-clean   # down automatically on failure
#
# Environment overrides (all optional):
#   WORK, KIND_CLUSTER, MINIO_PORT, MINIO_AK, MINIO_SK, MINIO_IMAGE,
#   MINIO_ENDPOINT, IMAGE_<NAME> (image tags), FC_VERSION, SBX_IMAGE,
#   WARM_IMAGES (=1: restore the preheat; default 0 = on-demand pulls)
#
# Every task logs to $WORK/logs/; failures dump component logs to
# logs/failure-<task>-<ts>.txt before exiting (never silently).
#
# Output: numbered milestone banners (==> [n] ...), per-stage durations,
# a stage-timings table and an environment summary after up/verify, plus the
# per-sandbox golden-restore breakdown (total/acquire/rootfs/launch/boot)
# extracted from the fastlet driver logs during verify.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.integration-env}"
LOGS_DIR="$WORK/logs"
GEN_DIR="$REPO_ROOT/.integration-env-gen"

KIND_CLUSTER="${KIND_CLUSTER:-firecracker}"
KIND_CONFIG="$REPO_ROOT/config/dev/kind-firecracker.yaml"
# P2P is the standard topology: the kind config carries TWO nodes (one
# fastlet per node, the second warm pull is a peer hit). KIND_SINGLE=1
# strips the worker for resource-constrained hosts (cache-only, no peer).
KIND_SINGLE="${KIND_SINGLE:-0}"
NS="fast-sandbox-system"

MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:latest}"
MINIO_PORT="${MINIO_PORT:-9000}"
MINIO_AK="${MINIO_AK:-integration-env}"
MINIO_SK="${MINIO_SK:-integration-env-secret}"
MINIO_BUCKET="sandbox-images"
MINIO_CONTAINER="integration-env-minio"
MINIO_DATA="$WORK/minio-data"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-}"   # auto-derived from the Kind network
STORE_ROOT="s3://$MINIO_BUCKET/publish"

FC_VERSION="${FC_VERSION:-v1.16.1}"
# The template source image doubles as the artifact key (the builder pulls
# it from a registry; index/digest16 are keyed by sha256 of this reference).
SBX_IMAGE="${SBX_IMAGE:-alpine:3.19}"
EXECD="${EXECD:-opensandbox/execd:1.1.0}"

# Docker Hub is unreachable from many hosts. The docker.io mirror baked into
# the kind nodes (config/dev/kind-firecracker.yaml) only covers in-cluster
# containerd pulls; the SandboxTemplate builder fetches the source image with
# go-containerregistry, which talks straight to the registry named in the
# reference and ignores containerd/daemon mirrors. So the mirror host has to be
# spelled into the reference itself. REGISTRY_MIRROR is that host and it only
# rewrites implicit docker.io references (one that already names a registry is
# left as is). Set REGISTRY_MIRROR= (empty) on hosts with direct Docker Hub
# access -- same note as the kind mirror block.
REGISTRY_MIRROR="${REGISTRY_MIRROR:-docker.m.daocloud.io}"
ROOTFS_SIZE="${ROOTFS_SIZE:-2Gi}"
SBX_TEMPLATE="ai-office-sandbox"
SBX_POOL="firecracker-pool"
SBX_SANDBOX="sandbox-firecracker"

# WARM_IMAGES=1 restores the pool warmImages preheat (optional). Default 0:
# on-demand is the standard stage-2 flow — the agent cache starts empty and
# the FIRST sandbox create on each node triggers the PinImage pull through
# DART (peer distribution across the two nodes), which `verify` measures.
WARM_IMAGES="${WARM_IMAGES:-0}"

# Node labels. The KVM label key is hardcoded by the SandboxTemplate
# reconciler (sandbox.fast.io/kvm); the firecracker label selects installer/
# agent/fastlet placement.
KVM_NODE_LABEL="sandbox.fast.io/kvm"
FC_NODE_LABEL="fast-sandbox.io/firecracker-node"

INOTIFY_VALUE="${INOTIFY_VALUE:-8192}"
SYSCTL_BACKUP="$WORK/sysctl-backup"

# Image tags (env-overridable per component).
IMG_CONTROLLER="${IMAGE_CONTROLLER:-fast-sandbox/controller:dev}"
IMG_FASTLET="${IMAGE_FASTLET:-fast-sandbox/fastlet:dev}"
IMG_FASTLET_PROXY="${IMAGE_FASTLET_PROXY:-fast-sandbox/fastlet-proxy:dev}"
IMG_SANDBOX_PROXY="${IMAGE_SANDBOX_PROXY:-fast-sandbox/sandbox-proxy:dev}"
IMG_JANITOR="${IMAGE_JANITOR:-fast-sandbox/janitor:dev}"
IMG_BUILDER="${IMAGE_BUILDER:-fast-sandbox/sandboxtemplate-builder:dev}"
IMG_AGENT="${IMAGE_AGENT:-fast-sandbox/firecracker-runtime-agent:dev}"

AUTO_CLEAN=0
ACTION=""

log() { printf '\033[1;34m[firecracker-integration]\033[0m %s\n' "$*" | tee -a "$WORK/run.log" >&2; }
die() { printf '\033[1;31m[firecracker-integration] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
pass() { printf '\033[1;32m[firecracker-integration] PASS\033[0m %s\n' "$*" | tee -a "$WORK/run.log" >&2; }
fail() { printf '\033[1;31m[firecracker-integration] FAIL\033[0m %s\n' "$*" >&2; exit 1; }
# highlight() marks a key milestone in the output (bold cyan, not logged).
highlight() { printf '\033[1;36m%s\033[0m\n' "$*"; }

# --- Go environment (sudo-aware) ----------------------------------------------
# The environment needs root (kind, loop devices, XFS mount, sysctl), so the
# script is normally invoked through sudo. sudo resets the environment and root
# has its own HOME, so `go env` loses the invoking user's module configuration:
# host builds then fail with "GOPROXY list is not the empty string, but contains
# no entries", or hang on an unreachable default proxy. Re-export the caller's
# Go settings so `make images`, `go build` and `go run` behave as they do outside
# sudo, and hand the resolved proxy to every docker build too (the builder stages
# compile Go inside the image and never see the host configuration).
#
# GOMODCACHE/GOCACHE are deliberately NOT inherited: root writing into the
# caller's caches leaves root-owned directories that break their later builds.
# Root re-downloads into its own cache, which persists across runs.
GO_INHERITED_VARS=(GOPROXY GOSUMDB GOPRIVATE GOINSECURE GOFLAGS)

inherit_go_env() {
	command -v go >/dev/null || return 0
	local go_bin name value i
	local -a values=()
	go_bin="$(command -v go)"
	if [[ "$(id -u)" == 0 && -n "${SUDO_USER:-}" ]]; then
		# `go env` prints one line per requested name, so the indexes line up.
		mapfile -t values < <(sudo -u "$SUDO_USER" -H "$go_bin" env "${GO_INHERITED_VARS[@]}" 2>/dev/null || true)
		for i in "${!GO_INHERITED_VARS[@]}"; do
			name="${GO_INHERITED_VARS[$i]}"
			value="${values[$i]:-}"
			# An explicit `sudo NAME=... ./integration-env.sh` keeps priority.
			[[ -n "${!name:-}" || -z "$value" ]] && continue
			export "$name=$value"
			log "inherited $name=$value from $SUDO_USER"
		done
	fi
	value="$("$go_bin" env GOPROXY 2>/dev/null || true)"
	if [[ -n "$value" ]]; then
		# `make images` reads DOCKER_BUILD_FLAGS as well; a caller-supplied
		# --build-arg comes later on the command line and still wins.
		export DOCKER_BUILD_FLAGS="--build-arg GOPROXY=$value ${DOCKER_BUILD_FLAGS:-}"
	else
		log "warning: go env GOPROXY is empty; module downloads will fail unless every module is already cached (fix with: go env -w GOPROXY=...)"
	fi
}

# mirror_ref rewrites an implicit Docker Hub reference to go through
# $REGISTRY_MIRROR (see the REGISTRY_MIRROR comment above). A reference that
# already names a registry -- a host with a dot or colon before the first '/'
# (e.g. myreg.io/x, localhost:5000/x) -- is returned unchanged, as is anything
# when REGISTRY_MIRROR is empty.
mirror_ref() {
	local ref="$1"
	[[ -n "$REGISTRY_MIRROR" ]] || { printf '%s' "$ref"; return; }
	local first="${ref%%/*}"
	if [[ "$ref" == */* && "$first" == *[.:]* ]]; then
		printf '%s' "$ref"; return
	fi
	# Docker Hub official images ("alpine:3.19") live under the library/ path.
	[[ "$ref" == */* ]] || ref="library/$ref"
	printf '%s/%s' "$REGISTRY_MIRROR" "$ref"
}

# Route the builder's source images through the mirror. Done once, in place, so
# the rendered template spec and the published-artifact assertions (which key
# off SBX_IMAGE) stay consistent.
apply_registry_mirror() {
	local image="$SBX_IMAGE" execd="$EXECD"
	SBX_IMAGE="$(mirror_ref "$SBX_IMAGE")"
	EXECD="$(mirror_ref "$EXECD")"
	[[ "$SBX_IMAGE" == "$image" ]] || log "image via mirror: $image -> $SBX_IMAGE"
	[[ "$EXECD" == "$execd" ]] || log "execd via mirror: $execd -> $EXECD"
}

# --- milestone + timing ------------------------------------------------------
# run_stage wraps every task with a numbered milestone banner and records the
# elapsed time; up/verify print a stage-summary table at the end.
now_ms() { date +%s%N; }   # GNU date (Linux); the script targets the test node
ms2s() { awk -v ms="$1" 'BEGIN { printf "%.1f", ms / 1000 }'; }

declare -a STAGE_ORDER=()     # stage names in insertion order
declare -a STAGE_MS_LIST=()   # parallel durations in ms
STAGE_CUR=""
STAGE_START_NS=0
STAGE_N=0

stage_begin() { # description
	STAGE_N=$((STAGE_N + 1))
	STAGE_CUR="$1"
	STAGE_START_NS="$(now_ms)"
	printf '\n\033[1;36m==> [%d] %s\033[0m\n' "$STAGE_N" "$1"
}

stage_done() { # [status-detail]
	local ms
	ms=$(( ($(now_ms) - STAGE_START_NS) / 1000000 ))
	STAGE_ORDER+=("$STAGE_CUR")
	STAGE_MS_LIST+=("$ms")
	printf '\033[1;32m    OK in %ss\033[0m %s\n' "$(ms2s "$ms")" "${1:-}"
}

run_stage() { # description function [args...]
	local description="$1" func="$2"
	shift 2
	stage_begin "$description"
	"$func" "$@"
	stage_done
}

stage_summary() {
	local index name ms total=0
	highlight "== stage timings =="
	printf '  \033[1m%-40s %10s\033[0m\n' "stage" "duration"
	for index in "${!STAGE_ORDER[@]}"; do
		name="${STAGE_ORDER[$index]}"
		ms="${STAGE_MS_LIST[$index]}"
		total=$((total + ms))
		printf '  %-40s %9ss\n' "$name" "$(ms2s "$ms")"
	done
	printf '  \033[1m%-40s %9ss\033[0m\n' "TOTAL" "$(ms2s "$total")"
}

wait_for() { # description attempts command [args...]
	local description="$1" attempts="$2" attempt=0
	shift 2
	while ! "$@" >/dev/null 2>&1; do
		attempt=$((attempt + 1))
		if [[ "$attempt" -ge "$attempts" ]]; then
			fail "$description (after $attempts attempts)"
		fi
		sleep 2
	done
	pass "$description"
}

# wait_until polls at 10ms granularity up to a millisecond deadline — used
# for the delivery-baseline waits (sandbox Ready, execd /ping), where the
# 2s polling of wait_for would drown the measurement. Requires GNU sleep
# (fractional seconds; the script targets the Linux test node).
wait_until() { # description timeout-ms command [args...]
	local description="$1" timeout_ms="$2"
	shift 2
	local deadline=$(( $(now_ms) + timeout_ms ))
	while ! "$@" >/dev/null 2>&1; do
		if [[ "$(now_ms)" -ge "$deadline" ]]; then
			fail "$description (after ${timeout_ms}ms)"
		fi
		sleep 0.01
	done
	pass "$description"
}

kubectl_get() { kubectl -n "$NS" get "$1" -o jsonpath="$2"; }

# --- condition helpers (plain functions, safe for wait_for) ------------------
template_succeeded() {
	[[ "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.phase}')" == "Succeeded" ]]
}

template_failed() {
	[[ "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.phase}')" == "Failed" ]]
}

fastlet_pod_ready() {
	local pod
	pod="$(kubectl -n "$NS" get pods -l app=sandbox-fastlet -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
	[[ -n "$pod" ]] && kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -q True
}

warm_images_ready() {
	kubectl -n "$NS" get sandboxpool "$SBX_POOL" -o jsonpath='{.status.warmImages[*].cachedFastlets}' 2>/dev/null | grep -qv '^0*$'
}

agent_pod() {
	kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

agent_leases_drained() {
	local pod uid
	pod="$(agent_pod)"
	uid="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')"
	kubectl exec -n "$NS" "$pod" -- sh -c \
		"curl -fsS --unix-socket /run/fast-sandbox/firecracker/runtime.sock -H 'Content-Type: application/json' -d '{\"podUid\":\"$uid\",\"namespace\":\"$NS\"}' http://firecracker-agent/v1/list-leases" \
		| grep -q '"leases":\[\]'
}

# dart_roster_ready reports whether the node-local DART daemon has joined the
# cluster: its admin /admin/members must list all agent pods (each hostNetwork
# pod IP == a node, so every member is a peer).
dart_roster_ready() { # pod expected-members
	local pod="$1" expected="$2"
	local members
	members="$(kubectl exec -n "$NS" "$pod" -- sh -c \
		'curl -fsS --noproxy "*" http://127.0.0.1:8147/admin/members' 2>/dev/null || true)"
	[[ "$(printf '%s' "$members" | grep -o '"id":' | wc -l | tr -d ' ')" == "$expected" ]]
}

jails_cleaned() {
	local node
	node="$(kind_node)"
	# removeJailRoot removes <state>/jails/<exec>/<id> per sandbox; the
	# exec-level dir (jails/firecracker) is permanent and must be empty.
	[[ -z "$(docker exec "$node" sh -c 'ls -A /var/lib/fast-sandbox/firecracker/jails/firecracker 2>/dev/null' || true)" ]]
}

# --- failure dump --------------------------------------------------------------
on_error() { # task
	local task="$1"
	if [[ "$AUTO_CLEAN" == 1 ]]; then
		log "up failed at $task; --auto-clean: running down"
		down >/dev/null 2>&1 || true
	fi
	failure_dump "$task"
	printf '\033[1;31m[firecracker-integration] FAILED at %s; dump: %s\033[0m\n' \
		"$task" "$LOGS_DIR/failure-$task-*.txt" >&2
}

failure_dump() { # task
	# Separate declarations: bash expands every word of a `local` command
	# before assigning, so referencing $task in the same statement would be an
	# unbound variable under set -u (the dump would die instead of dumping).
	local task="$1"
	local dump="$LOGS_DIR/failure-$task-$(date +%s).txt"
	mkdir -p "$LOGS_DIR"
	{
		echo "=== integration-env failure: $task ($(date -u +%FT%TZ)) ==="
		env | grep -E '^(MINIO|KIND|FC_|SBX|IMG_|EXECD|WORK)=' || true
		echo "--- kind nodes ---"
		kind get nodes --name "$KIND_CLUSTER" 2>&1 || true
		echo "--- kind-create.log (tail) ---"
		tail -40 "$LOGS_DIR/kind-create.log" 2>&1 || true
		echo "--- pods ---"
		kubectl get pods -n "$NS" -o wide 2>&1 || true
		echo "--- controller logs (tail) ---"
		kubectl logs -n "$NS" deploy/fast-sandbox-controller --tail=80 2>&1 || true
		echo "--- agent logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime-agent --tail=80 2>&1 || true
		echo "--- installer logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime-installer --all-containers --tail=80 2>&1 || true
		echo "--- fastlet logs (tail) ---"
		kubectl logs -n "$NS" -l app=sandbox-fastlet --tail=80 2>&1 || true
		echo "--- builder pods + logs (tail) ---"
		kubectl get pods -n "$NS" -l sandbox.fast.io/sandboxtemplate --show-labels 2>&1 || true
		kubectl logs -n "$NS" -l sandbox.fast.io/sandboxtemplate --tail=80 2>&1 || true
		echo "--- template ---"
		kubectl get sandboxtemplate -n "$NS" -o yaml 2>&1 || true
		echo "--- pool ---"
		kubectl get sandboxpool -n "$NS" "$SBX_POOL" -o yaml 2>&1 || true
		echo "--- minio docker logs (tail) ---"
		docker logs "$MINIO_CONTAINER" --tail=80 2>&1 || true
		echo "--- node kvm ---"
		local node
		node="$(kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -1 || true)"
		[[ -n "$node" ]] && docker exec "$node" sh -c 'ls -l /dev/kvm; grep -c vmx /proc/cpuinfo' 2>&1 || true
	} > "$dump" 2>&1 || true
	log "failure dump: $dump"
}

# --- helpers ---------------------------------------------------------------------
kind_node() { kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -1; }

kind_network() { # name of the docker network the node container is on
	local node
	node="$(kind_node)" || return 1
	docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$node" | tr ' ' '\n' | grep -v '^$' | head -1
}

mc() { docker run --rm --network host -v "$WORK/mc-config:/root/.mc" minio/mc "$@"; }

# gen-registry: compiled registryconfig JSON for one S3 credential (same
# pattern as scripts/firecracker-chain-e2e.sh).
gen_registry() { # host username password endpoint > registry.json
	mkdir -p "$GEN_DIR"
	cat > "$GEN_DIR/gen-registry.go" <<'EOF'
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
	(cd "$REPO_ROOT" && GOTOOLCHAIN=local go run .integration-env-gen/gen-registry.go "$@")
}

# --- task 1: preflight + sysctl ----------------------------------------------------
# Missing tooling is installed automatically (kind/kubectl from official
# release binaries, jq from the package manager). Set SKIP_TOOL_INSTALL=1 to
# only verify and point at the manual install steps instead.
KIND_VERSION="${KIND_VERSION:-v0.24.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.31.0}"
SKIP_TOOL_INSTALL="${SKIP_TOOL_INSTALL:-0}"
SKIP_LEFTOVER_CLEAN="${SKIP_LEFTOVER_CLEAN:-0}"
KIND_RETAIN="${KIND_RETAIN:-0}"

sudo_() { if [[ "$(id -u)" == 0 ]]; then "$@"; else sudo "$@"; fi; }

install_release_binary() { # name version url
	local name="$1" version="$2" url="$3" tmp
	log "installing $name $version -> /usr/local/bin/$name"
	tmp="$(mktemp -d)"
	curl -fL --retry 3 -o "$tmp/$name" "$url" \
		|| die "download $name failed ($url); install it manually or retry"
	sudo_ install -m 0755 "$tmp/$name" "/usr/local/bin/$name"
	rm -rf "$tmp"
}

ensure_tool() { # name
	local name="$1"
	if command -v "$name" >/dev/null 2>&1; then
		return 0
	fi
	if [[ "$SKIP_TOOL_INSTALL" == 1 ]]; then
		die "$name is required (SKIP_TOOL_INSTALL=1: install it manually, see logs/environment.txt)"
	fi
	case "$name" in
		kind)
			install_release_binary kind "$KIND_VERSION" \
				"https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-amd64"
			;;
		kubectl)
			install_release_binary kubectl "${KUBECTL_VERSION#v}" \
				"https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl"
			;;
		jq)
			log "installing jq via package manager"
			if command -v apt-get >/dev/null 2>&1; then
				sudo_ apt-get install -y jq >/dev/null
			elif command -v yum >/dev/null 2>&1; then
				sudo_ yum install -y jq >/dev/null
			elif command -v apk >/dev/null 2>&1; then
				sudo_ apk add --no-cache jq >/dev/null
			else
				die "no supported package manager to install jq; install it manually"
			fi
			;;
		*)
			die "$name is required; install it manually"
			;;
	esac
	command -v "$name" >/dev/null 2>&1 || die "$name installation failed"
}

preflight() {
	command -v docker >/dev/null || die "docker is required"
	command -v go >/dev/null || die "go is required (>=1.25, used by make images and gen-registry)"
	ensure_tool kind
	ensure_tool kubectl
	ensure_tool jq
	docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
	local cgdriver cgver
	cgdriver="$(docker info --format '{{.CgroupDriver}}' 2>/dev/null || true)"
	cgver="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || true)"
	log "docker cgroup driver=$cgdriver version=$cgver"
	# kind requires cgroup v2: on v1 hosts kubelet fails to create the
	# kubepods cgroup ("failed to initialize top level QOS containers")
	# regardless of the docker cgroup driver.
	if [[ "$cgver" == "1" ]]; then
		die "docker cgroup Version is 1; kind requires cgroup v2. Enable it with the kernel cmdline 'systemd.unified_cgroup_hierarchy=1' (update-grub / grub2-mkconfig) and reboot, then verify 'docker info' shows Cgroup Version: 2"
	fi
	[[ -e /dev/kvm ]] || die "/dev/kvm is missing on this host (KVM required)"
	docker pull -q "$MINIO_IMAGE" >/dev/null
	docker pull -q minio/mc >/dev/null
	pass "preflight"
}

sysctl_set() {
	local current
	current="$(sysctl -n fs.inotify.max_user_instances)"
	[[ "$current" -ge "$INOTIFY_VALUE" ]] && return 0
	echo "$current" > "$SYSCTL_BACKUP"
	log "sysctl fs.inotify.max_user_instances: $current -> $INOTIFY_VALUE"
	sudo sysctl -w fs.inotify.max_user_instances="$INOTIFY_VALUE" >/dev/null
}

sysctl_restore() {
	[[ -f "$SYSCTL_BACKUP" ]] || return 0
	local previous
	previous="$(cat "$SYSCTL_BACKUP")"
	log "restoring fs.inotify.max_user_instances -> $previous"
	sudo sysctl -w fs.inotify.max_user_instances="$previous" >/dev/null || true
	rm -f "$SYSCTL_BACKUP"
}

# --- XFS StateRoot (reflink CoW per-sandbox rootfs) ---------------------------
# The per-instance rootfs copy is a full copy on ext4 (~2.5s per sandbox,
# images.go copyReflinkOrCopy). Mounting an XFS loop image at
# /var/lib/fast-sandbox (passed into the kind node, the runtime plan's
# StateRoot lives under it) turns the copy into a CoW reflink (~ms).
# Disable with XFS_STATEROOT=0; the plain directory then works as before.
XFS_STATEROOT="${XFS_STATEROOT:-1}"
XFS_LOOP_FILE="${XFS_LOOP_FILE:-$WORK/fast-sandbox.img}"
XFS_SIZE="${XFS_SIZE:-24G}"
XFS_MOUNT_POINT="${XFS_MOUNT_POINT:-/var/lib/fast-sandbox}"

ensure_xfsprogs() {
	command -v mkfs.xfs >/dev/null 2>&1 && return 0
	log "installing xfsprogs (mkfs.xfs)"
	if command -v apt-get >/dev/null 2>&1; then
		sudo_ apt-get install -y xfsprogs >/dev/null
	elif command -v yum >/dev/null 2>&1; then
		sudo_ yum install -y xfsprogs >/dev/null
	else
		die "xfsprogs not installed and no supported package manager"
	fi
}

stateroot_xfs_up() {
	[[ "$XFS_STATEROOT" == 1 ]] || {
		log "XFS StateRoot disabled (XFS_STATEROOT=0); per-sandbox rootfs pays a full copy"
		return 0
	}
	if findmnt -no FSTYPE "$XFS_MOUNT_POINT" 2>/dev/null | grep -qx xfs; then
		log "XFS StateRoot already mounted at $XFS_MOUNT_POINT: $(findmnt -no SOURCE,FSTYPE "$XFS_MOUNT_POINT")"
		pass "XFS StateRoot ready (reflink CoW rootfs)"
		return 0
	fi
	ensure_xfsprogs
	if [[ ! -f "$XFS_LOOP_FILE" ]]; then
		log "creating sparse XFS image $XFS_LOOP_FILE (virtual $XFS_SIZE)"
		truncate -s "$XFS_SIZE" "$XFS_LOOP_FILE"
		sudo_ mkfs.xfs -f "$XFS_LOOP_FILE" >/dev/null 2>&1 || die "mkfs.xfs failed on $XFS_LOOP_FILE"
	fi
	sudo_ mkdir -p "$XFS_MOUNT_POINT"
	sudo_ mount -o noatime "$XFS_LOOP_FILE" "$XFS_MOUNT_POINT" \
		|| die "mount $XFS_LOOP_FILE at $XFS_MOUNT_POINT failed (loop support? try XFS_STATEROOT=0)"
	# probe: reflink must actually work on the resulting filesystem. The
	# fresh mount is root-owned, so the probe file is written through sudo_
	# as well (a non-root runner cannot write it directly).
	local a b
	a="$XFS_MOUNT_POINT/.reflink-a"
	b="$XFS_MOUNT_POINT/.reflink-b"
	printf 'probe' | sudo_ tee "$a" >/dev/null
	if sudo_ cp --reflink=always "$a" "$b"; then
		sudo_ rm -f "$a" "$b"
		pass "XFS StateRoot ready (reflink CoW rootfs)"
	else
		sudo_ rm -f "$a" "$b"
		die "reflink probe failed on $XFS_MOUNT_POINT (CoW rootfs would not work)"
	fi
}

stateroot_xfs_down() {
	[[ "$XFS_STATEROOT" == 1 ]] || return 0
	if findmnt -no SOURCE "$XFS_MOUNT_POINT" 2>/dev/null | grep -q "$(basename "$XFS_LOOP_FILE")"; then
		log "unmounting XFS StateRoot $XFS_MOUNT_POINT"
		sudo_ umount "$XFS_MOUNT_POINT"
	fi
	rm -f "$XFS_LOOP_FILE"
}

build_images() {
	log "building images (controller/fastlet/fastlet-proxy/sandbox-proxy/janitor/agent)"
	(cd "$REPO_ROOT" && make images COMPONENT=controller >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=fastlet >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=fastlet-proxy >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=sandbox-proxy >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=janitor >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=firecracker-runtime-agent >/dev/null)
	log "building sandboxtemplate-builder image"
	# The Dockerfile defaults GOPROXY to proxy.golang.org, which is unreachable
	# on some hosts. Inherit the host's `go env GOPROXY` (same mirror the rest of
	# the build uses) so no manual --build-arg is needed. A GOPROXY in
	# DOCKER_BUILD_FLAGS still wins because it appears later on the command line;
	# an empty host value is skipped so it never overrides the Dockerfile default.
	builder_goproxy="$(go env GOPROXY)"
	builder_goproxy_arg=()
	if [[ -n "$builder_goproxy" ]]; then
		builder_goproxy_arg=(--build-arg "GOPROXY=$builder_goproxy")
	fi
	# shellcheck disable=SC2086
	docker build "${builder_goproxy_arg[@]}" ${DOCKER_BUILD_FLAGS:-} --quiet -t "$IMG_BUILDER" \
		-f "$REPO_ROOT/build/Dockerfile.sandboxtemplate-builder" "$REPO_ROOT" >/dev/null
	log "building fastctl (host CLI)"
	mkdir -p "$WORK/bin"
	(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/bin/fastctl" ./cmd/fastctl)
	log "building gen-endpoint (proxy route helper)"
	write_gen_endpoint_source
	(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/bin/gen-endpoint" .integration-env-gen/gen-endpoint.go)
	pass "images built"
}

write_gen_endpoint_source() {
	mkdir -p "$GEN_DIR" "$WORK/bin"
	cat > "$GEN_DIR/gen-endpoint.go" <<'EOF'
// Command gen-endpoint resolves the central-proxy route of a Sandbox port.
//
// Daemon mode (--daemon <socket> <grpc-endpoint>) keeps the FastPath gRPC
// connection warm and serves resolves over a unix-socket HTTP endpoint, so
// latency measurement does not pay process spawn / connection setup per
// probe:
//
//	GET http://localhost/resolve?ns=<ns>&name=<name>&port=<port>
//	-> "<proxy endpoint path>\t<route credential>" (text/plain)
//
// One-shot mode prints the same line for the given arguments.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func resolve(client fastpathv2.FastPathServiceClient, ns, name string, port uint32) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := client.ResolveEndpoint(ctx, &fastpathv2.ResolveEndpointRequest{
		Sandbox: &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Namespace: ns, Name: name}},
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: port}},
		// DIRECT_FASTLET_PROXY: the endpoint is the assigned fastlet's
		// fastlet-proxy (:5780), resolved from the durable assignment
		// ANNOTATION — no dependency on the eventually-consistent
		// status.placement projection or the central sandbox-proxy.
		AccessMode: fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY,
	})
	if err != nil {
		return "", "", err
	}
	credential := ""
	for header, value := range resp.GetRequiredHeaders() {
		if header == "X-Fast-Sandbox-Route-Credential" {
			credential = value
		}
	}
	return resp.GetProxyEndpoint(), credential, nil
}

func dial(endpoint string) *grpc.ClientConn {
	conn, err := grpc.Dial(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return conn
}

func daemon(socketPath, endpoint string) {
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Chmod(socketPath, 0o660)
	conn := dial(endpoint)
	defer conn.Close()
	client := fastpathv2.NewFastPathServiceClient(conn)
	// Warm the connection before any measurement starts.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = client.ListSandboxes(ctx, &fastpathv2.ListSandboxesRequest{})
	cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		ns, name, portValue := query.Get("ns"), query.Get("name"), query.Get("port")
		port, err := strconv.ParseUint(portValue, 10, 32)
		if err != nil || ns == "" || name == "" {
			http.Error(w, "ns/name/port are required", http.StatusBadRequest)
			return
		}
		// The third output field is the ResolveEndpoint wall time, so the
		// probe can split control-plane resolve cost from the curl data
		// path without relying on curl header support.
		started := time.Now()
		path, credential, err := resolve(client, ns, name, uint32(port))
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "%s\t%s\t%dms\n", path, credential, time.Since(started).Milliseconds())
	})
	server := &http.Server{Handler: mux}
	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "--daemon" {
		daemon(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: gen-endpoint <grpc-endpoint> <namespace> <sandbox-name> <port> | gen-endpoint --daemon <socket> <grpc-endpoint>")
		os.Exit(1)
	}
	conn := dial(os.Args[1])
	defer conn.Close()
	port, err := strconv.ParseUint(os.Args[4], 10, 32)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path, credential, err := resolve(fastpathv2.NewFastPathServiceClient(conn), os.Args[2], os.Args[3], uint32(port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s\t%s\n", path, credential)
}
EOF
}

# --- task 2: kind cluster -------------------------------------------------------------
# KIND_NODE_IMAGE overrides the kindest/node image (e.g. a local mirror when
# registry.k8s.io is unreachable); the script pulls it explicitly first so a
# slow/blocked pull fails loudly instead of inside kind create.
# KIND_RETAIN=1 keeps the failed node container for diagnosis (kind --retain).
kind_up() {
	local create_args=() kind_config="$KIND_CONFIG"
	[[ "$KIND_RETAIN" == 1 ]] && create_args+=(--retain)
	if [[ "$KIND_SINGLE" == "1" ]]; then
		# KIND_SINGLE=1 strips the worker node (P2P becomes cache-only).
		kind_config="$WORK/kind-firecracker-single.yaml"
		sed '/^- role: worker/,$d' "$KIND_CONFIG" > "$kind_config"
		log "single-node topology (KIND_SINGLE=1: no worker, no peer traffic)"
	fi
	if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]]; then
		local node_count
		node_count="$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
		if [[ "$KIND_SINGLE" != "1" && "$node_count" -lt 2 ]]; then
			log "WARNING: existing $node_count-node cluster cannot grow a worker; run 'integration-env.sh down' first for the two-node P2P topology"
		fi
		log "cluster $KIND_CLUSTER already exists; reusing (run down first for a clean rebuild)"
	else
		if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
			log "pulling kind node image $KIND_NODE_IMAGE (this can take minutes)"
			docker pull -q "$KIND_NODE_IMAGE" || die "kind node image pull failed (KIND_NODE_IMAGE=$KIND_NODE_IMAGE)"
			log "creating cluster with node image $KIND_NODE_IMAGE"
			kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" \
				"${create_args[@]}" --config "$kind_config" > "$LOGS_DIR/kind-create.log" 2>&1 \
				|| fail "kind create failed (full log: $LOGS_DIR/kind-create.log)"
		else
			log "creating cluster (pulling kindest/node may take minutes; set KIND_NODE_IMAGE to a mirror if it fails)"
			kind create cluster --name "$KIND_CLUSTER" --config "$kind_config" \
				"${create_args[@]}" > "$LOGS_DIR/kind-create.log" 2>&1 \
				|| fail "kind create failed (full log: $LOGS_DIR/kind-create.log)"
		fi
		pass "kind cluster created"
	fi
	local node
	for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
		docker exec "$node" sh -c 'test -e /dev/kvm' || die "KVM not visible inside the kind node container $node"
		kubectl label node "$node" "$KVM_NODE_LABEL=true" --overwrite >/dev/null
		kubectl label node "$node" "$FC_NODE_LABEL=true" --overwrite >/dev/null
		log "node $node: KVM + firecracker labels applied"
	done
	if [[ "$KIND_SINGLE" != "1" ]]; then
		# Multi-node kind keeps the control-plane tainted (NoSchedule),
		# which would strand half the topology: every firecracker workload
		# (agent DaemonSet, fastlet pool, builder) must be schedulable on
		# BOTH nodes for the P2P assertions to see two peers.
		kubectl taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null 2>&1 || true
		log "control-plane taint removed (both nodes schedulable for the P2P topology)"
	fi
	pass "kind cluster ready (kvm passthrough + labels on every node)"
}

# --- task 3: MinIO + credentials -------------------------------------------------------
minio_up() {
	docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	# The MinIO container writes its object store as root, so a previous
	# run's data can only be purged through sudo_ (a non-root runner would
	# fail on every part.* file and abort the whole up).
	sudo_ rm -rf "$MINIO_DATA"
	mkdir -p "$MINIO_DATA"
	local net
	net="$(kind_network)"
	# Joining the kind network avoids docker-proxy/hairpin reachability
	# issues: pods and the node container talk to the container IP directly,
	# while 127.0.0.1 publishing keeps host-side mc/curl working.
	docker run -d --name "$MINIO_CONTAINER" --network "$net" \
		-p 127.0.0.1:"$MINIO_PORT":9000 -p 127.0.0.1:9001:9001 \
		-e MINIO_ROOT_USER="$MINIO_AK" -e MINIO_ROOT_PASSWORD="$MINIO_SK" \
		-v "$MINIO_DATA:/data" \
		"$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
	for attempt in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$MINIO_PORT/minio/health/live" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "MinIO did not become healthy"
	done
	# /health/live answers before the S3 API finishes initializing; retry the
	# mc alias until the server really accepts credentials.
	for attempt in $(seq 1 30); do
		if mc alias set chain "http://127.0.0.1:$MINIO_PORT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "MinIO S3 API not initialized (mc alias failed)"
	done
	mc mb "chain/$MINIO_BUCKET" >/dev/null
	pass "MinIO up (bucket=$MINIO_BUCKET)"
}

# resolve the endpoint pods inside the kind node container use for MinIO:
# the container's own IP on the kind network (container-to-container on the
# docker bridge needs no port publishing). Override with MINIO_ENDPOINT.
resolve_minio_endpoint() {
	if [[ -n "$MINIO_ENDPOINT" ]]; then
		log "task 3: MinIO endpoint (env): $MINIO_ENDPOINT"
	else
		local net ips ip
		net="$(kind_network)"
		ips="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}} {{end}}' "$MINIO_CONTAINER")"
		ip="$(printf '%s' "$ips" | tr ' ' '\n' | grep -A1 -x "^$net$" | tail -1)"
		[[ -n "$ip" ]] || die "could not find the MinIO IP on network $net (inspect: $ips)"
		MINIO_ENDPOINT="http://$ip:$MINIO_PORT"
		log "task 3: MinIO endpoint (kind network IP): $MINIO_ENDPOINT"
	fi
	local net
	net="$(kind_network)"
	docker run --rm --network "$net" minio/mc alias set chain \
		"$MINIO_ENDPOINT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1 \
		|| die "MinIO unreachable from the kind network at $MINIO_ENDPOINT (override MINIO_ENDPOINT; check host firewalld/iptables if the container IP also fails)"
	pass "MinIO reachable from the kind network"
}

credentials_up() {
	# The secrets land in the platform namespace; make sure it exists even
	# when controller_up has not run yet (e.g. resume after a partial up).
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	local host
	host="${MINIO_ENDPOINT#http://}"
	host="${host#https://}"
	# Publish credentials: SecretKeyRef'd by the builder Pod (same namespace
	# as the template).
	kubectl -n "$NS" create secret generic sandbox-oss-credentials \
		--from-literal=accessKeyId="$MINIO_AK" \
		--from-literal=secretAccessKey="$MINIO_SK" \
		--from-literal=endpoint="$MINIO_ENDPOINT" \
		--from-literal=region=us-east-1 \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the runtime-agent (compiled registryconfig).
	gen_registry "$host" "$MINIO_AK" "$MINIO_SK" "$MINIO_ENDPOINT" > "$WORK/agent-registry.json"
	jq -e . "$WORK/agent-registry.json" >/dev/null || die "generated agent registry.json is invalid"
	kubectl -n "$NS" create secret generic fast-sandbox-agent-registry \
		--from-file=registry.json="$WORK/agent-registry.json" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Agent endpoint override (connection address for SigV4 signing).
	kubectl -n "$NS" create configmap fast-sandbox-agent-config \
		--from-literal=artifact-endpoint="$MINIO_ENDPOINT" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the fastlet (pool-compiled registry).
	kubectl -n "$NS" create secret docker-registry registry-minio \
		--docker-server="$host" --docker-username="$MINIO_AK" --docker-password="$MINIO_SK" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" create configmap fast-sandbox-registry \
		--from-literal="registries.yaml=registries:
  - host: $host
    secretRef:
      name: registry-minio
" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	pass "credentials written"
}

# --- task 4: CRDs + controller ----------------------------------------------------------
controller_up() {
	kubectl apply -k "$REPO_ROOT/config/crd" >/dev/null
	kubectl apply -k "$REPO_ROOT/config/all-in-one" >/dev/null
	for image in "$IMG_CONTROLLER" "$IMG_FASTLET" "$IMG_FASTLET_PROXY" "$IMG_SANDBOX_PROXY" "$IMG_JANITOR" "$IMG_BUILDER" "$IMG_AGENT"; do
		kind load docker-image "$image" --name "$KIND_CLUSTER" >/dev/null
	done
	wait_for "controller deployment ready" 120 \
		kubectl -n "$NS" rollout status deploy/fast-sandbox-controller --timeout=10s
	for crd in sandboxpools sandboxtemplates sandboxes; do
		kubectl get crd "$crd.sandbox.fast.io" >/dev/null 2>&1 || die "CRD $crd missing"
	done
	pass "CRDs + controller ready"
}

# --- task 5: node assets + runtime environment -------------------------------------------
installer_up() {
	kubectl apply -f "$REPO_ROOT/config/runtime-installers/firecracker.yaml" >/dev/null
	wait_for "firecracker installer ready" 60 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime-installer --timeout=15s
	pass "firecracker/jailer/kernel installed on the node"
}

# --- task 6: agent DaemonSet ----------------------------------------------------------------
agent_up() {
	kubectl apply -f "$REPO_ROOT/config/dev/dart-service.yaml" >/dev/null
	kubectl apply -f "$REPO_ROOT/config/dev/agent-daemonset.yaml" >/dev/null
	wait_for "runtime-agent DaemonSet ready" 120 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime-agent --timeout=10s

	# DART P2P daemon (stage 2): every agent pod must have its node-local
	# dart child answering on the admin plane, and agent /v1/health must
	# report dartUp=true (a missing dart only degrades pulls to direct S3,
	# so this is a positive wiring assertion, not a readiness gate).
	local pod uid node pods
	pods="$(kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[*].metadata.name}')"
	for pod in $pods; do
		uid="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')"
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart admin /healthz on $node" 30 \
			kubectl exec -n "$NS" "$pod" -- sh -c \
				"curl -fsS --noproxy '*' http://127.0.0.1:8147/healthz | grep -q ok"
		wait_for "agent health dartUp on $node" 30 \
			kubectl exec -n "$NS" "$pod" -- sh -c \
				"curl -fsS --noproxy '*' --unix-socket /run/fast-sandbox/firecracker/runtime.sock -H 'Content-Type: application/json' -d '{\"podUid\":\"$uid\",\"namespace\":\"$NS\"}' http://firecracker-agent/v1/health | grep -q '\"dartUp\":true'"
		log "dart: $node dart pid=$(kubectl exec -n "$NS" "$pod" -- sh -c 'pgrep -x dart')"
	done
	# P2P roster: every daemon must see every other agent pod as a peer
	# before any warm pull, so the second node's pull can be served by the
	# first node's dart instead of the origin.
	local expected_members
	expected_members="$(printf '%s' "$pods" | wc -w | tr -d ' ')"
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart roster full on $node ($expected_members members)" 90 \
			dart_roster_ready "$pod" "$expected_members"
	done
	pass "runtime-agent healthy (UDS /v1/health) + DART daemons up, roster=$expected_members"
}

# --- task 7: SandboxTemplate build -----------------------------------------------------------
# wait_succeeded waits for a condition while bailing out early (with a log
# dump) as soon as the probe reports the terminal Failed state.
wait_succeeded() { # description attempts probe probe_failed
	local description="$1" attempts="$2" probe="$3" probe_failed="$4" attempt=0
	while ! "$probe" >/dev/null 2>&1; do
		if "$probe_failed" >/dev/null 2>&1; then
			failure_dump "template-failed"
			fail "$description (template entered Failed)"
		fi
		attempt=$((attempt + 1))
		if [[ "$attempt" -ge "$attempts" ]]; then
			failure_dump "template-timeout"
			fail "$description (after $attempts attempts)"
		fi
		sleep 2
	done
	pass "$description"
}

template_up() {
	# Render SBX_IMAGE / EXECD into the sample so the documented overrides
	# really select what gets built (the builder pulls with
	# go-containerregistry, which ignores the docker daemon registry-mirrors,
	# so a mirror has to be spelled out in the reference itself). Defaults
	# reproduce the sample verbatim.
	local template_spec="$WORK/sandboxtemplate-firecracker.yaml"
	sed -e "s|^  image: .*|  image: $SBX_IMAGE|" \
		-e "s|^  execd: .*|  execd: $EXECD|" \
		"$REPO_ROOT/config/samples/sandboxtemplate-firecracker.yaml" > "$template_spec"
	grep -q "^  image: $SBX_IMAGE\$" "$template_spec" || die "could not render image=$SBX_IMAGE into the template spec"
	grep -q "^  execd: $EXECD\$" "$template_spec" || die "could not render execd=$EXECD into the template spec"
	kubectl apply -f "$template_spec" >/dev/null
	wait_succeeded "template phase=Succeeded" 300 template_succeeded template_failed
	local manifest_ref
	manifest_ref="$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.manifestRef}')"
	[[ -n "$manifest_ref" ]] || fail "template manifestRef is empty"
	log "template manifestRef: $manifest_ref"
	assert_publish_layout
	pass "SandboxTemplate Succeeded + artifacts published"
}

assert_publish_layout() {
	local image_sha index_key index_json manifest_ref manifest_key manifest_json build_dir_key digest_size
	image_sha="$(printf '%s' "$SBX_IMAGE" | sha256sum | awk '{print $1}')"
	index_key="publish/index/$image_sha.json"
	index_json="$(mc cat "chain/$MINIO_BUCKET/$index_key")" || fail "index object missing: $index_key"
	[[ "$(printf '%s' "$index_json" | jq -r .image)" == "$SBX_IMAGE" ]] || fail "index.image does not match $SBX_IMAGE"
	manifest_ref="$(printf '%s' "$index_json" | jq -r .manifestRef)"
	[[ "$manifest_ref" == s3://* ]] || fail "manifestRef is not an s3 URL: $manifest_ref"
	manifest_key="${manifest_ref#s3://$MINIO_BUCKET/}"
	manifest_json="$(mc cat "chain/$MINIO_BUCKET/$manifest_key")" || fail "manifest object missing: $manifest_key"
	# index.artifactDigest is the sha256 of the manifest document. Hash the
	# raw object bytes (command substitution would strip the trailing
	# newline the digest was computed over).
	digest_size="$(printf '%s' "$index_json" | jq -r .artifactDigest)"
	[[ "$(mc cat "chain/$MINIO_BUCKET/$manifest_key" | sha256sum | awk '{print $1}')" == "$digest_size" ]] \
		|| fail "index.artifactDigest does not match sha256(manifest)"
	build_dir_key="$(dirname "$manifest_key")"
	for object in rootfs.ext4 vmstate.snap memory.snap SHA256SUMS manifest.json; do
		mc stat "chain/$MINIO_BUCKET/$build_dir_key/$object" >/dev/null 2>&1 \
			|| fail "artifact missing: $object"
	done
	# The manifest declares machine + guestNetwork (baked 172.30.0.3) and a
	# per-file sha256/sizeBytes list that must agree with the stored objects.
	[[ "$(printf '%s' "$manifest_json" | jq -r .guestNetwork.ip)" == "172.30.0.3" ]] \
		|| fail "manifest.guestNetwork.ip != 172.30.0.3"
	[[ "$(printf '%s' "$manifest_json" | jq -r .machine.vcpu)" == "1" ]] \
		|| fail "manifest.machine.vcpu != 1"
	local object
	while IFS= read -r object; do
		[[ -n "$object" ]] || continue
		local want_size stored_size
		want_size="$(printf '%s' "$manifest_json" | jq -r --arg n "$object" '.files[$n].sizeBytes')"
		[[ "$want_size" != "null" && -n "$want_size" ]] || fail "manifest.files has no entry for $object"
		stored_size="$(mc stat --json "chain/$MINIO_BUCKET/$build_dir_key/$object" 2>/dev/null | jq -r .size)"
		[[ "$stored_size" == "$want_size" ]] || fail "stored $object size $stored_size != manifest $want_size"
	done < <(printf '%s' "$manifest_json" | jq -r '.files | keys[]')
	pass "publish layout complete (index + manifest + artifacts, sizes match)"
}

# --- task 8: SandboxPool -----------------------------------------------------------------------
pool_up() {
	if [[ "$WARM_IMAGES" == "1" ]]; then
		# Optional preheat mode: warmImages pull the artifact set on every
		# fastlet node during up (fast delivery baselines; the second node
		# is still served by the first node's DART peer).
		kubectl apply -f "$REPO_ROOT/config/samples/pool-firecracker.yaml" >/dev/null
		wait_for "fastlet pod running" 150 fastlet_pod_ready
		wait_for "pool warmImages Ready" 300 warm_images_ready
		p2p_evidence "warm preheat"
		pass "fastlet Running + warmImages Ready (agent PinImage closed loop, P2P evidence captured)"
	else
		# On-demand is the standard stage-2 flow: apply the pool spec
		# WITHOUT the warmImages entry (it is the last section of the
		# manifest). No preheat — the first sandbox create on each node
		# pulls the artifact set through DART; the evidence lands in the
		# verify stage (verify 5: P2P evidence).
		local cold_spec="$WORK/pool-firecracker-cold.yaml"
		sed '/^  warmImages:/,$d' "$REPO_ROOT/config/samples/pool-firecracker.yaml" > "$cold_spec"
		kubectl apply -f "$cold_spec" >/dev/null
		wait_for "fastlet pod running" 150 fastlet_pod_ready
		pass "fastlet Running, on-demand pull (default: first sandbox pulls through DART)"
	fi
}

# p2p_evidence asserts the stage-2 outcome from the DART block counters: the
# published artifact set (rootfs/vmstate/memory) is pulled once per 4MiB
# block from the origin cluster-wide, and when more than one node served
# traffic the second node must have been fed by the first node's peer
# (block_source{peer} > 0). P2P is the standard data plane, so this runs as
# part of the environment flow, not as a separate experiment.
p2p_evidence() { # description
	local description="$1"
	local pods pod manifest_ref manifest_key build_dir expected_blocks=0
	local origin_total=0 peer_total=0 cache_total=0 size value source active_nodes=0 node_total
	manifest_ref="$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.manifestRef}')"
	manifest_key="${manifest_ref#s3://$MINIO_BUCKET/}"
	build_dir="$(dirname "$manifest_key")"
	for object in rootfs.ext4 vmstate.snap memory.snap; do
		size="$(mc stat --json "chain/$MINIO_BUCKET/$build_dir/$object" 2>/dev/null | jq -r .size)"
		[[ "$size" =~ ^[0-9]+$ ]] || die "cannot stat published $object (publish incomplete?)"
		expected_blocks=$((expected_blocks + (size + 4194303) / 4194304))
	done
	pods="$(kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[*].metadata.name}')"
	for pod in $pods; do
		node_total=0
		while read -r source value; do
			case "$source" in
				origin) origin_total=$((origin_total + value)); node_total=$((node_total + value)) ;;
				peer) peer_total=$((peer_total + value)); node_total=$((node_total + value)) ;;
				cache) cache_total=$((cache_total + value)); node_total=$((node_total + value)) ;;
			esac
		done < <(dart_source_counters "$pod")
		[[ "$node_total" -gt 0 ]] && active_nodes=$((active_nodes + 1))
	done
	log "p2p evidence ($description): expected origin=$expected_blocks blocks; cluster origin=$origin_total peer=$peer_total cache=$cache_total active-nodes=$active_nodes"
	[[ "$origin_total" -ge "$expected_blocks" ]] || fail "cluster origin $origin_total < expected $expected_blocks blocks"
	[[ "$origin_total" -le $((expected_blocks + 4)) ]] \
		|| fail "origin amplified: $origin_total > $((expected_blocks + 4)): pulls were not deduplicated by DART"
	if [[ "$active_nodes" -ge 2 ]]; then
		[[ "$peer_total" -gt 0 ]] || fail "no peer traffic across $active_nodes nodes: the second node was not served by the peer"
		pass "P2P evidence ($description): origin ~1 fetch per block (cluster=$origin_total/$expected_blocks), peer=$peer_total, nodes=$active_nodes"
	else
		pass "P2P evidence ($description): origin ~1 fetch per block (cluster=$origin_total/$expected_blocks) on $active_nodes node (no peer needed)"
	fi
}

# --- task 9: sandbox + delivery ------------------------------------------------------------------
# --- fastctl: end-to-end driver through the central proxy ----------------------
# verify drives the full public surface with the host fastctl CLI: create /
# status / execd exec / delete all go through the Fast-Path gRPC API and the
# sandbox-proxy -> fastlet-proxy chain (never kubectl exec inside the
# fastlet). Port-forwards expose the in-cluster services to the host.
FASTCTL="$WORK/bin/fastctl"
GEN_ENDPOINT="$WORK/bin/gen-endpoint"
FASTPATH_LOCAL="127.0.0.1:19090"
SANDBOX_PROXY_LOCAL="http://127.0.0.1:18080"
FASTCTL_FLAGS=(--namespace "$NS" --endpoint "$FASTPATH_LOCAL" --proxy-endpoint "$SANDBOX_PROXY_LOCAL")

fastctl() { "$FASTCTL" "${FASTCTL_FLAGS[@]}" "$@"; }

PF_PIDS=()
GEN_DAEMON_PID=""
GEN_SOCKET="$WORK/gen-endpoint.sock"
FASTLET_IPS=()
FASTLET_PORTS=()
FASTLET_PIDS=()
FASTLET_ALLOCATED_PORT=""
FASTLET_RESOLVED_PORT=""
# FASTLET_NEXT_PORT hands out local forward ports; reset on every sweep and
# bumped by allocate_next_local_port so mid-verify additions never collide
# with ports that are still mapped to another pod.
FASTLET_NEXT_PORT=18081

port_forward_up() {
	local pid
	[[ -x "$FASTCTL" ]] || die "fastctl not built ($FASTCTL); run up first"
	# A stale forward from an interrupted run holds the local ports and makes
	# the new kubectl port-forward exit immediately ("address already in
	# use"); sweep only forwards for OUR ports before starting. TERM first,
	# then KILL the stragglers: a forward whose process survives the TERM
	# keeps its local listener alive and makes the recreated forward fail
	# its bind while the STALE listener keeps answering probes on that port.
	pkill -f "kubectl -n $NS port-forward .* 19090:9090" 2>/dev/null || true
	sleep 0.2
	kubectl -n "$NS" port-forward deploy/fast-sandbox-controller 19090:9090 >/dev/null 2>&1 &
	pid=$!
	PF_PIDS+=("$pid")
	rebuild_fastlet_forwards
	wait_for "fastpath reachable via port-forward" 30 fastpath_reachable
}

# rebuild_fastlet_forwards kills every local fastlet-proxy forward and
# re-establishes one per CURRENT live fastlet pod, rebuilding the IP -> port
# map from scratch. It is the deterministic recovery for any drift between
# the map and the actual listeners (a straggler kubectl holding a local port
# makes probes land on a fastlet that lacks the new sandbox routes).
rebuild_fastlet_forwards() {
	# TERM first, then KILL the stragglers: a forward whose process survives
	# the TERM keeps its local listener alive and makes the recreated
	# forward fail its bind while the STALE listener keeps answering probes
	# on that port.
	pkill -f "kubectl -n $NS port-forward .*:5780" 2>/dev/null || true
	sleep 0.5
	pkill -9 -f "kubectl -n $NS port-forward .*:5780" 2>/dev/null || true
	sleep 0.2
	# The map is REBUILT from the current pod list on every sweep: entries
	# from a replaced pod incarnation would otherwise keep resolving onto a
	# dead forward (curl http=000) or onto a port a later sweep re-pointed
	# at a DIFFERENT live pod, whose fastlet-proxy answers 404 for the uid.
	FASTLET_IPS=()
	FASTLET_PORTS=()
	FASTLET_PIDS=()
	FASTLET_NEXT_PORT=18081
	local name ip port pid
	while read -r name ip; do
		[[ -n "$name" && -n "$ip" ]] || continue
		allocate_next_local_port
		if read -r port pid < <(start_fastlet_forward "$name" "$ip" "$FASTLET_ALLOCATED_PORT"); then
			fastlet_record_forward "$ip" "$port" "$pid"
		else
			log "port-forward for fastlet pod $name ($ip) could not be established"
		fi
	done < <(fastlet_pod_ip_list)
}

# fastlet_pod_ip_list prints the current "podName podIP" pairs of every
# live fastlet pod (all pools share the app=sandbox-fastlet label). Pods
# that are terminating (deletionTimestamp set) or not Running are excluded:
# a replacing pool pod appears while the old one is still draining, and a
# forward to the old pod would keep answering on the local port with a
# proxy store that never holds the NEW sandbox routes (issue #37 404s).
fastlet_pod_ip_list() {
	kubectl -n "$NS" get pods -l app=sandbox-fastlet -o json 2>/dev/null \
		| jq -r '.items[]
			| select(.metadata.deletionTimestamp == null)
			| select(.status.phase == "Running")
			| [.metadata.name, .status.podIP] | @tsv' 2>/dev/null
}

# fastlet_pod_name_for_ip resolves a pod IP (the DIRECT endpoint authority)
# back to the live pod name so a forward can be (re)established for it.
fastlet_pod_name_for_ip() { # ip
	local ip="$1"
	fastlet_pod_ip_list | awk -v ip="$ip" '$2 == ip { print $1; exit }'
}

# tcp_listening reports whether something answers on 127.0.0.1:$1. Without nc
# (probe tool missing) it assumes the forward is alive so healthy forwards
# are never churned.
tcp_listening() { # port
	if command -v nc >/dev/null 2>&1; then
		nc -z -w 1 127.0.0.1 "$1" >/dev/null 2>&1
	else
		return 0
	fi
}

# fastlet_mapped_port looks up the local forward port recorded for a pod IP
# together with the kubectl pid that serves it.
fastlet_mapped_port() { # ip -> "port pid"; rc 1 when not mapped
	local ip="$1" i
	for i in "${!FASTLET_IPS[@]}"; do
		if [[ "${FASTLET_IPS[$i]}" == "$ip" ]]; then
			printf '%s %s' "${FASTLET_PORTS[$i]}" "${FASTLET_PIDS[$i]}"
			return 0
		fi
	done
	return 1
}

# fastlet_unmap_ip drops the mapping of a pod IP and kills the forward that
# served it so the port can be reused by the replacement pod. Must be called
# in the shell that owns the map arrays (never inside a command substitution).
fastlet_unmap_ip() { # ip
	local ip="$1" i port
	for i in "${!FASTLET_IPS[@]}"; do
		[[ "${FASTLET_IPS[$i]}" != "$ip" ]] && continue
		port="${FASTLET_PORTS[$i]}"
		pkill -f "kubectl -n $NS port-forward .* $port:5780" 2>/dev/null || true
		unset 'FASTLET_IPS[i]' 'FASTLET_PORTS[i]' 'FASTLET_PIDS[i]'
	done
}

# fastlet_record_forward records a live forward in the map arrays (owner
# shell only — the arrays are otherwise lost to a command-substitution child).
fastlet_record_forward() { # ip port pid
	FASTLET_IPS+=("$1")
	FASTLET_PORTS+=("$2")
	FASTLET_PIDS+=("$3")
	PF_PIDS+=("$3")
}

# allocate_next_local_port picks the next free local port (owner shell only:
# FASTLET_NEXT_PORT must advance in the same shell that starts the forwards).
allocate_next_local_port() { # -> FASTLET_ALLOCATED_PORT
	local port="${FASTLET_NEXT_PORT:-18081}"
	if command -v nc >/dev/null 2>&1; then
		while nc -z -w 1 127.0.0.1 "$port" >/dev/null 2>&1; do
			port=$((port + 1))
		done
	fi
	FASTLET_NEXT_PORT=$((port + 1))
	FASTLET_ALLOCATED_PORT="$port"
}

# start_fastlet_forward starts one local forward to the pod's fastlet-proxy
# on the GIVEN local port and prints "port pid" once it answers. It has no
# side effects on the map arrays (callers record the mapping in their own
# shell), so it is safe under command substitution / process substitution.
# A kubectl that exits right away (bind collision, dead pod) is detected via
# kill -0 and treated as failure — a surviving STALE listener must never be
# mistaken for this forward (it would proxy to a pod that lacks the routes).
start_fastlet_forward() { # pod-name pod-ip local-port -> stdout "port pid"
	local name="$1" ip="$2" port="$3" pid attempt errf
	errf="$WORK/pf-start-${port}.err"
	kubectl -n "$NS" port-forward "pod/$name" "$port:5780" >/dev/null 2>"$errf" &
	pid=$!
	for attempt in $(seq 1 25); do
		if kill -0 "$pid" 2>/dev/null; then
			if tcp_listening "$port"; then
				rm -f "$errf"
				# Trailing newline is REQUIRED: read returns non-zero on EOF
				# when the last line lacks one, which made every caller's
				# "read -r port pid < <(start_fastlet_forward ...)" fail even
				# though the forward had started successfully.
				printf '%s %s\n' "$port" "$pid"
				return 0
			fi
		else
			break
		fi
		sleep 0.2
	done
	{
		printf '[firecracker-integration] start_fastlet_forward: FAILED pod=%s ip=%s port=%s (kubectl exited or never answered)\n' "$name" "$ip" "$port"
		sed 's/^/    pf> /' "$errf" 2>/dev/null || true
		rm -f "$errf"
	} | tee -a "$WORK/run.log" >&2
	kill "$pid" >/dev/null 2>&1 || true
	pkill -f "kubectl -n $NS port-forward .* $port:5780" 2>/dev/null || true
	return 1
}

# ensure_fastlet_forward maps a fastlet pod IP (the DIRECT endpoint
# authority) to a LIVE local port-forward, re-establishing the forward when
# needed and recording the mapping in the OWNER shell:
#   - the pod appeared after the last sweep (pool pod recreated mid-verify):
#     open its forward on demand;
#   - the recorded forward died with its pod (nothing listens on the port):
#     drop the stale mapping and forward to the replacement pod;
#   - the port answers but the mapped kubectl is gone (a straggler forward
#     holds the port): kill the straggler and rebuild, otherwise probes would
#     land on a fastlet that lacks the new routes.
# On success FASTLET_RESOLVED_PORT holds the local port. Must be called as a
# plain function (NOT in a command substitution) so the map updates stick.
ensure_fastlet_forward() { # pod-ip
	local ip="$1" port_pid port pid name
	if port_pid="$(fastlet_mapped_port "$ip")"; then
		port="${port_pid%% *}"
		pid="${port_pid#* }"
		if kill -0 "$pid" 2>/dev/null && tcp_listening "$port"; then
			FASTLET_RESOLVED_PORT="$port"
			return 0
		fi
		log "ensure_fastlet_forward: stale mapping for $ip (port ${port:-?}); re-establishing"
		fastlet_unmap_ip "$ip"
	fi
	name="$(fastlet_pod_name_for_ip "$ip")"
	if [[ -z "$name" ]]; then
		log "ensure_fastlet_forward: no live fastlet pod has IP $ip (stale assignment?)"
		return 1
	fi
	allocate_next_local_port
	port="$FASTLET_ALLOCATED_PORT"
	if read -r port pid < <(start_fastlet_forward "$name" "$ip" "$port"); then
		fastlet_record_forward "$ip" "$port" "$pid"
		FASTLET_RESOLVED_PORT="$port"
		return 0
	fi
	return 1
}

# resolve_daemon_up starts the resident route resolver: the FastPath gRPC
# connection is established once (no process spawn / HTTP2 dial per probe),
# so the measured data-plane latency reflects the chain, not the plumbing.
# Leftover daemons from interrupted runs are swept first (they hold the
# socket path); a stale binary is rebuilt on the spot.
resolve_daemon_up() {
	# A leftover daemon from an interrupted run keeps listening on the
	# socket path and interferes with readiness; sweep all of ours.
	pkill -f "gen-endpoint --daemon" 2>/dev/null || true
	sleep 0.3
	# grep without -q: -q closes the pipe on the first match and the writer
	# dies with SIGPIPE, which pipefail turns into a false "stale" verdict.
	if [[ ! -x "$GEN_ENDPOINT" ]] || ! "$GEN_ENDPOINT" 2>&1 | grep -- "--daemon" >/dev/null; then
		log "gen-endpoint binary missing or stale; rebuilding"
		write_gen_endpoint_source
		(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/bin/gen-endpoint" .integration-env-gen/gen-endpoint.go)
	fi
	rm -f "$GEN_SOCKET"
	"$GEN_ENDPOINT" --daemon "$GEN_SOCKET" "127.0.0.1:19090" >/dev/null 2>&1 &
	GEN_DAEMON_PID=$!
	# readiness = the resolver actually answers, not just the socket node.
	wait_until "gen-endpoint daemon resolve" 10000 \
		sh -c "curl -fsS --unix-socket '$GEN_SOCKET' http://localhost/readyz >/dev/null 2>&1"
}

resolve_daemon_down() {
	if [[ -n "$GEN_DAEMON_PID" ]]; then
		kill "$GEN_DAEMON_PID" >/dev/null 2>&1 || true
		GEN_DAEMON_PID=""
	fi
	rm -f "$GEN_SOCKET"
}

# gen_endpoint_for resolves one port route through the resident daemon and
# prints "<proxy endpoint path>\t<route credential>".
gen_endpoint_for() { # sandbox-name port
	curl -fsS -m 15 --unix-socket "$GEN_SOCKET" \
		"http://localhost/resolve?ns=$NS&name=$1&port=$2" 2>/dev/null
}

port_forward_down() {
	local pid
	for pid in "${PF_PIDS[@]}"; do
		kill "$pid" >/dev/null 2>&1 || true
	done
	PF_PIDS=()
}

fastpath_reachable() {
	fastctl list >/dev/null 2>&1
}

sandbox_exists() { # sandbox-name
	fastctl get "$1" -o json >/dev/null 2>&1
}

sandbox_ready() { # sandbox-name
	# fastctl get -o json encodes the full GetSandboxResponse: the state
	# lives at .sandbox.runtime.state / .sandbox.data_plane.state (standard
	# encoding/json: underscore keys, numeric enums; READY == 4). The defs
	# also tolerate the bare SandboxInfo / protojson forms.
	fastctl get "$1" -o json 2>/dev/null | jq -e '
		def rt: (.runtime.state // .sandbox.runtime.state);
		def dp: ((.dataPlane.state // .data_plane.state) // (.sandbox.dataPlane.state // .sandbox.data_plane.state));
		(rt == 4 or rt == "RUNTIME_STATE_READY")
		and (dp == 4 or dp == "DATA_PLANE_STATE_READY")' >/dev/null 2>&1
}

fastctl_run_sandbox() { # sandbox-name
	local name="$1" attempt=0
	# Cold-start guarantee: a leftover sandbox from an interrupted run is
	# already warm and would invalidate the delivery-baseline measurement
	# (a 9ms run RPC with a 33ms restore is the tell-tale). Delete first;
	# leftovers of interrupted runs can tear down slowly, so after 90s the
	# cleanup finalizer is force-removed as a last resort.
	if sandbox_exists "$name"; then
		fastctl delete "$name" >/dev/null 2>&1 || true
		while ! sandbox_gone "$name"; do
			attempt=$((attempt + 1))
			if [[ "$attempt" -eq 90 ]]; then
				log "leftover $name still terminating after 90s; force-removing the cleanup finalizer"
				kubectl -n "$NS" patch sandbox "$name" --type=merge \
					-p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
			fi
			if [[ "$attempt" -ge 120 ]]; then
				fail "leftover $name could not be removed"
			fi
			sleep 1
		done
	fi
	# Creation can transiently fail while the pool scales out ("no eligible
	# Fastlet": the first fastlet is full and the second has not heartbeated
	# yet). Retry; a succeeded create that errored on the wire surfaces as
	# AlreadyExists on the next attempt, which is also success.
	for attempt in $(seq 1 30); do
		if fastctl run "$name" --image "$SBX_IMAGE" --pool "$SBX_POOL" >/dev/null 2>&1 \
			|| sandbox_exists "$name"; then
			return 0
		fi
		sleep 2
	done
	die "fastctl run $name failed"
}

sandbox_gone() { # sandbox-name
	! fastctl get "$1" -o json >/dev/null 2>&1
}

# probe_execd checks execd /ping END-TO-END through the central proxy: the
# resident resolver issues the sandbox PORT route (ResolveEndpoint 44772),
# then curl hits the sandbox-proxy with the credential — sandbox-proxy ->
# fastlet-proxy -> guest execd. The successful attempt records the resolve
# vs curl split in PROBE_RESOLVE_MS / PROBE_CURL_MS.
PROBE_RESOLVE_MS=""
PROBE_CURL_MS=""
PROBE_CONNECT_MS=""
PROBE_TTFB_MS=""
PROBE_LOG=()
probe_execd() { # sandbox-name
	local name="$1" out path cred uri host lport t0 timing t_abs
	t_abs=$(( $(now_ms) / 1000000 ))
	out="$(gen_endpoint_for "$name" 44772 2>/dev/null)" || {
		log "probe_execd $name: route resolve failed"
		if [[ "${DEBUG_PROBE:-0}" == 1 ]]; then
			local errbody
			errbody="$(curl -s --unix-socket "$GEN_SOCKET" \
				"http://localhost/resolve?ns=$NS&name=$name&port=44772" 2>/dev/null)"
			echo "  [DEBUG_PROBE] $name: resolve FAILED body=${errbody:0:120}" >&2
		fi
		PROBE_LOG+=("t=${t_abs}ms resolve=FAILED")
		return 1
	}
	path="$(printf '%s' "$out" | cut -f1)"
	cred="$(printf '%s' "$out" | cut -f2)"
	if [[ -z "$path" || -z "$cred" ]]; then
		log "probe_execd $name: resolve returned empty route (out='$(printf '%s' "$out" | head -c 120)')"
		return 1
	fi
	PROBE_RESOLVE_MS="$(printf '%s' "$out" | cut -f3)"
	# strip scheme://authority -> /v1/sandboxes/{uid}/ports/{port}
	uri="$(printf '%s' "$path" | sed 's|^[a-z]*://[^/]*||')"
	# the DIRECT endpoint authority is the assigned fastlet pod IP; dial the
	# matching local port-forward instead.
	host="$(printf '%s' "$path" | sed 's|^[a-z]*://\([^/:]*\).*|\1|')"
	if ! ensure_fastlet_forward "$host"; then
		log "probe_execd $name: no local forward for host=$host"
		PROBE_LOG+=("t=${t_abs}ms host=${host} NO-LOCAL-PORT")
		return 1
	fi
	lport="$FASTLET_RESOLVED_PORT"
	t0="$(now_ms)"
	timing="$(curl -fsS -m 5 -o /dev/null \
		-w '%{time_connect} %{time_starttransfer}' \
		-H "X-Fast-Sandbox-Route-Credential: $cred" \
		"http://127.0.0.1:$lport$uri/ping" 2>/dev/null)" || {
		# Surface the failure window (code) on EVERY failed probe so a
		# run.log alone shows whether the local port refused (000), the
		# reached proxy lacks the route (404) or rejects the credential
		# (401/403).
		local code
		code="$(curl -s -o /dev/null -w '%{http_code}' -m 3 \
			-H "X-Fast-Sandbox-Route-Credential: $cred" \
			"http://127.0.0.1:$lport$uri/ping" 2>/dev/null)"
		log "probe_execd $name: curl failed host=$host lport=$lport http=${code:-000}"
		PROBE_LOG+=("t=${t_abs}ms resolve=${PROBE_RESOLVE_MS} host=${host} lport=$lport curl=HTTP${code:-000}")
		return 1
	}
	PROBE_CURL_MS=$(( ($(now_ms) - t0) / 1000000 ))
	# time_connect / time_starttransfer are seconds since the request start.
	PROBE_CONNECT_MS="$(printf '%s' "$timing" | awk '{printf "%d", $1 * 1000}')"
	PROBE_TTFB_MS="$(printf '%s' "$timing" | awk '{printf "%d", $2 * 1000}')"
	PROBE_LOG+=("t=${t_abs}ms resolve=${PROBE_RESOLVE_MS} host=${host} curl=200 ttfb=${PROBE_TTFB_MS}")
}

# show_ping_latency splits the proxy-chain /ping cost: one route resolution
# through the resident resolver, then a COLD curl (fresh TCP through
# port-forward + sandbox-proxy + fastlet-proxy) and a WARM curl reusing the
# same route and connection setup path (still a new TCP per hop, but no
# resolve/sign).
show_ping_latency() { # sandbox-name
	local name="$1" out path cred uri host lport t0 cold_ms warm_ms
	out="$(gen_endpoint_for "$name" 44772 2>/dev/null)" || return 0
	path="$(printf '%s' "$out" | cut -f1)"
	cred="$(printf '%s' "$out" | cut -f2)"
	[[ -n "$path" && -n "$cred" ]] || return 0
	uri="$(printf '%s' "$path" | sed 's|^[a-z]*://[^/]*||')"
	host="$(printf '%s' "$path" | sed 's|^[a-z]*://\([^/:]*\).*|\1|')"
	ensure_fastlet_forward "$host" || return 0
	lport="$FASTLET_RESOLVED_PORT"
	t0="$(now_ms)"
	curl -fsS -m 5 -o /dev/null \
		-H "X-Fast-Sandbox-Route-Credential: $cred" \
		"http://127.0.0.1:$lport$uri/ping" 2>/dev/null || return 0
	cold_ms=$(( ($(now_ms) - t0) / 1000000 ))
	t0="$(now_ms)"
	curl -fsS -m 5 -o /dev/null \
		-H "X-Fast-Sandbox-Route-Credential: $cred" \
		"http://127.0.0.1:$lport$uri/ping" 2>/dev/null || return 0
	warm_ms=$(( ($(now_ms) - t0) / 1000000 ))
	highlight "  key node: proxy-chain /ping latency of '$name' (same route reused)"
	printf '    cold /ping = %sms   warm /ping = %sms\n' "$cold_ms" "$warm_ms" | tee -a "$WORK/run.log"
}

# klog_field extracts one key="value" (or key=value for ints) pair from a
# klog text line.
klog_field() { # line key
	local raw
	raw="$(printf '%s' "$1" | grep -o "$2=\"[^\"]*\"\|$2=[0-9a-zA-Z._-]*" | head -1)"
	[[ -n "$raw" ]] || return 0
	printf '%s' "${raw#*=}" | tr -d '"'
}

# show_restore_timings highlights the driver's per-sandbox creation breakdown
# (total/acquire/rootfs/infra/launch/configure/boot) from the fastlet logs.
show_restore_timings() { # sandbox-name
	local name="$1" fastlet line
	fastlet="$(kubectl_get "sandbox/$name" '{.status.placement.fastletName}')"
	[[ -n "$fastlet" ]] || return 0
	line="$(kubectl -n "$NS" logs --request-timeout=10s --tail=300 "$fastlet" 2>/dev/null | grep 'firecracker sandbox created' | tail -1)"
	[[ -n "$line" ]] || return 0
	highlight "  key node: golden restore of '$name'"
	printf '    total=%s  acquire=%s  rootfs=%s  infra=%s  launch=%s  configure=%s  boot=%s  vmStatePolls=%s\n' \
		"$(klog_field "$line" total)" "$(klog_field "$line" acquire)" \
		"$(klog_field "$line" rootfs)" "$(klog_field "$line" infra)" \
		"$(klog_field "$line" launch)" "$(klog_field "$line" configure)" \
		"$(klog_field "$line" boot)" "$(klog_field "$line" vmStatePolls)"
}

# report_create_tail derives the end-to-end create tail from three
# independent clocks: fastctl run RPC (client), fastpath create (controller),
# golden restore (fastlet driver). tail = everything after the VM resumed
# until the client sees READY. restore_total and fastpath_total are parsed
# from their own components' logs (no shared instrumentation).
report_create_tail() { # name t0-ns t-run-done-ns
	local name="$1" t0="$2" t_done="$3"
	local run_rpc fp_total dr_total fp_line dr_line fastlet
	run_rpc=$(( (t_done - t0) / 1000000 ))
	fp_line="$(kubectl -n "$NS" logs --request-timeout=10s --tail=500 deploy/fast-sandbox-controller 2>/dev/null | grep 'fastpath sandbox created' | grep "requestId=\"$name\"" | tail -1)"
	[[ -n "$fp_line" ]] || return 0
	fp_total="$(klog_field "$fp_line" total | tr -d 'ms')"
	fp_total="${fp_total%%.*}"
	[[ -n "$fp_total" ]] || return 0
	fastlet="$(kubectl_get "sandbox/$name" '{.status.placement.fastletName}')"
	[[ -n "$fastlet" ]] || return 0
	dr_line="$(kubectl -n "$NS" logs --request-timeout=10s --tail=300 "$fastlet" 2>/dev/null | grep 'firecracker sandbox created' | tail -1)"
	dr_total="$(klog_field "$dr_line" total | tr -d 'ms')"
	dr_total="${dr_total%%.*}"
	[[ -n "$dr_total" ]] || dr_total=0
	highlight "  key node: end-to-end create tail of '$name' (post-restore)"
	printf '    create tail = %sms   fastlet-side = %sms   client gap = %sms   (run RPC %sms + restore %sms)\n' \
		"$(( run_rpc - dr_total ))" "$(( fp_total - dr_total ))" "$(( run_rpc - fp_total ))" \
		"$run_rpc" "$dr_total" | tee -a "$WORK/run.log"
}

# show_e2e_latency reports the true end-to-end timeline of one sandbox:
# fastctl run (fastpath create + fastlet restore, completion=READY blocks
# until the runtime reports ready) -> first successful execd /ping through
# the central proxy. Availability is judged by the probe, NOT by the
# eventually-consistent CR status. Timestamps in epoch-ms, deltas in ms.
show_e2e_latency() { # sandbox-name t0-ns t-run-done-ns t-ping-ns t-probe-start-ns
	local name="$1" t0="$2" t_run_done="$3" t_ping="$4" t_probe_start="$5"
	local e0 er ep run_rpc queue first200 total
	e0=$(( t0 / 1000000 ))
	er=$(( t_run_done / 1000000 ))
	ep=$(( t_ping / 1000000 ))
	run_rpc=$(( (t_run_done - t0) / 1000000 ))
	# queue = time between the create returning and THIS sandbox's probe
	# beginning (sequential probing: earlier sandboxes' probes/reporting
	# land here, so the first-200 wait below is the honest per-sandbox cost).
	queue=$(( (t_probe_start - t_run_done) / 1000000 ))
	first200=$(( (t_ping - t_probe_start) / 1000000 ))
	total=$(( (t_ping - t0) / 1000000 ))
	highlight "  key node: end-to-end latency of '$name' (run → execd /ping)"
	printf '    t(run)=%sms  t(run-done)=%sms  t(probe)=%sms  t(ping)=%sms\n' "$e0" "$er" "$(( t_probe_start / 1000000 ))" "$ep"
	printf '    run RPC = %sms   queue-to-probe = %sms   first-200 = %sms   total = %sms\n' \
		"$run_rpc" "$queue" "$first200" "$total" | tee -a "$WORK/run.log"
	if [[ -n "$PROBE_RESOLVE_MS" ]]; then
		printf '    first probe: resolve = %sms   connect = %sms   ttfb = %sms   curl-total = %sms\n' \
			"$PROBE_RESOLVE_MS" "$PROBE_CONNECT_MS" "$PROBE_TTFB_MS" "$PROBE_CURL_MS" | tee -a "$WORK/run.log"
	fi
	if [[ "${DEBUG_PROBE:-0}" == 1 && ${#PROBE_LOG[@]} -gt 1 ]]; then
		printf '    probe attempts: %s\n' "${PROBE_LOG[*]}" >&2
	fi
}

verify() {
	local second="${SBX_SANDBOX}-2"

	port_forward_up
	resolve_daemon_up
	trap 'port_forward_down; resolve_daemon_down' EXIT
	run_stage "verify 1: sandbox create + execd /ping (fastctl)" verify_sandbox "$SBX_SANDBOX"
	run_stage "verify 2: clone sandbox (shared snapshot, per-clone netns)" verify_sandbox "$second"
	run_stage "verify 3: max concurrency (2 fastlets, 10 slots)" verify_concurrent
	run_stage "verify 4: P2P evidence (origin ~1 fetch/block via DART)" p2p_evidence "verify delivery"
	run_stage "verify 5: delete all ($((CONCURRENCY + 2))) + cleanup" verify_delete_all
	trap - EXIT
	resolve_daemon_down
	port_forward_down

	stage_summary
	highlight "== verify complete: 2 + $CONCURRENCY sandboxes delivered via fastctl (central proxy); teardown clean =="
}

# --- max-concurrency soak (part of verify) ------------------------------------
# CONCURRENCY (default 5) is the per-fastlet slot capacity
# (maxSandboxesPerPod). The batch runs while the two probe sandboxes are
# still alive: 7 sandboxes exceed one fastlet's 5 slots, so the pool keeps
# two fastlet pods (poolMin=2, 10 slots) and the batch lands on both —
# proving concurrent restore across multiple fastlets on the shared golden
# snapshot. (Dynamic scale-out cannot trigger via the fastpath create: a
# full pool is rejected with ResourceExhausted BEFORE the Sandbox CR exists,
# so the controller never sees the pending demand.)
# Names use the sbx-stress- prefix so they never collide with the verify
# sandboxes.
CONCURRENCY="${CONCURRENCY:-5}"

stress_name() { printf 'sbx-stress-%d' "$1"; }

fastlet_pod_count() { # expected-count
	local got
	got="$(kubectl -n "$NS" get pods -l app=sandbox-fastlet -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
	[[ "$got" -eq "$1" ]]
}

verify_concurrent() {
	local i name t_run_done t_probe_start t_ping
	local -a t0s=() t_dones=() t_probes=() t_pings=()
	if [[ "$CONCURRENCY" -gt 5 ]]; then
		die "CONCURRENCY=$CONCURRENCY exceeds the pool's maxSandboxesPerPod=5"
	fi
	for i in $(seq 1 "$CONCURRENCY"); do
		name="$(stress_name "$i")"
		t0s+=("$(now_ms)")
		fastctl_run_sandbox "$name"
		t_dones+=("$(now_ms)")
	done
	log "created $CONCURRENCY sandboxes on top of 2 running sandboxes"
	wait_for "two fastlet pods ready (poolMin=2 topology)" 180 fastlet_pod_count 2
	pass "2 fastlet pods serving $((CONCURRENCY + 2)) sandboxes (2 x 5 slots)"
	# Sequential probing with probe-start-relative timing: every sandbox
	# records its own first-200 wait from the moment ITS probe began, so the
	# reported value is never polluted by the earlier sandboxes' probes or
	# reporting. (Parallel subshell probing was dropped: it kept hanging on
	# the shared daemon/port-forward background jobs.)
	for i in $(seq 1 "$CONCURRENCY"); do
		name="$(stress_name "$i")"
		t_probe_start="$(now_ms)"
		wait_until "execd /ping $name" 120000 probe_execd "$name"
		t_ping="$(now_ms)"
		show_e2e_latency "$name" "${t0s[$((i - 1))]}" "${t_dones[$((i - 1))]}" "$t_ping" "$t_probe_start"
		show_restore_timings "$name"
		report_create_tail "$name" "${t0s[$((i - 1))]}" "${t_dones[$((i - 1))]}"
		show_ping_latency "$name"
	done
	pass "$CONCURRENCY sandboxes Ready (concurrent VMs from the shared snapshot)"
	pass "execd /ping reachable on all $CONCURRENCY sandboxes (via central proxy)"
}

verify_sandbox() { # sandbox-name
	local name="$1" t0 t_run_done t_probe_start t_ping
	t0="$(now_ms)"
	fastctl_run_sandbox "$name"
	t_run_done="$(now_ms)"
	# Availability is judged END-TO-END: the first successful execd /ping
	# through the central proxy, not the eventually-consistent CR status
	# (fastpath create already blocked on completion=READY, so the route is
	# resolvable as soon as run returns).
	t_probe_start="$(now_ms)"
	wait_until "execd /ping" 120000 probe_execd "$name"
	t_ping="$(now_ms)"
	show_e2e_latency "$name" "$t0" "$t_run_done" "$t_ping" "$t_probe_start"
	show_restore_timings "$name"
	report_create_tail "$name" "$t0" "$t_run_done"
	show_ping_latency "$name"
	if sandbox_ready "$name"; then
		log "    CR status Ready confirmed (eventual consistency)"
	fi
	pass "execd /ping reachable on $name (via central proxy)"
}

verify_delete_all() {
	local i name
	for name in "$SBX_SANDBOX" "${SBX_SANDBOX}-2"; do
		fastctl delete "$name" >/dev/null 2>&1 || true
	done
	for i in $(seq 1 "$CONCURRENCY"); do
		name="$(stress_name "$i")"
		fastctl delete "$name" >/dev/null 2>&1 || true
	done
	wait_for "agent leases drained" 120 agent_leases_drained
	wait_for "jail dirs cleaned" 120 jails_cleaned
	pass "delete cleaned leases + jails ($((CONCURRENCY + 2)) sandboxes)"
}

# --- verify-execd-api: execd HTTP API usability in the Firecracker guest ----
# Dedicated execd protocol battery for the golden snapshot (OpenSandbox issue
# #1695): POST /command reportedly hangs in the Firecracker guest (connection
# up, SSE never streams, curl times out with 0 bytes) while GET /ping on the
# same listener answers instantly and a malformed body returns 400 — proving
# the request reaches execd and the stall sits in the runCommand path
# (stdlog descriptor/tail -> getShell -> launchManaged).
#
# The battery drives the execd HTTP API over the SAME DIRECT_FASTLET_PROXY
# route the /ping probes use (no central sandbox-proxy), and adds a
# no-proxy-at-all control that curls the guest execd (172.30.0.3:44772)
# straight from the sandbox's slot netns inside the fastlet pod.
#
# Root-cause signal (not just red/green): cases that write NOTHING to stdout
# (/bin/false, a missing binary) complete whenever the synchronous init
# sequence does not stall, while output-producing cases (echo / pipe / sleep)
# additionally exercise the stdout-file + tail pipeline. So:
#   everything HANGs                    -> init-sequence stall (stdlog/getShell)
#   only output-producing cases HANG    -> stdout file/tail pipeline suspect
#   everything completes                -> execd API usable (not reproduced)
EXECD_API_SBX="${EXECD_API_SBX:-sandbox-execd-api}"
# EXECD_API_KEEP_SANDBOX=1 keeps the sandbox (and its jail) after the battery
# so the guest serial console — execd logs included — can be read from
# firecracker.log before the jail is removed.
EXECD_API_KEEP_SANDBOX="${EXECD_API_KEEP_SANDBOX:-0}"
EXECD_API_CASES=()
EXECD_API_URI=""
EXECD_API_CRED=""
EXECD_API_LPORT=""
EXECD_API_HOST=""

# execd_route_resolve resolves the execd port route of a sandbox
# (DIRECT_FASTLET_PROXY, the durable-assignment fastlet) and maps the fastlet
# authority to the local port-forward — the same plumbing probe_execd uses.
# Sets EXECD_API_URI / EXECD_API_CRED / EXECD_API_LPORT / EXECD_API_HOST.
execd_route_resolve() { # sandbox-name
	local name="$1" out path cred uri host
	out="$(gen_endpoint_for "$name" 44772 2>/dev/null)" || return 1
	path="$(printf '%s' "$out" | cut -f1)"
	cred="$(printf '%s' "$out" | cut -f2)"
	[[ -n "$path" && -n "$cred" ]] || return 1
	uri="$(printf '%s' "$path" | sed 's|^[a-z]*://[^/]*||')"
	host="$(printf '%s' "$path" | sed 's|^[a-z]*://\([^/:]*\).*|\1|')"
	EXECD_API_URI="$uri"
	EXECD_API_CRED="$cred"
	EXECD_API_HOST="$host"
	ensure_fastlet_forward "$host" || return 1
	EXECD_API_LPORT="$FASTLET_RESOLVED_PORT"
}

# execd_base_url prints the base curl URL of the resolved execd route
# (http://127.0.0.1:<local-forward><route-path>).
execd_base_url() {
	printf 'http://127.0.0.1:%s%s' "$EXECD_API_LPORT" "$EXECD_API_URI"
}

record_api_row() { # name verdict detail
	EXECD_API_CASES+=("$1|$2|$3")
	printf '  %-16s %-9s %s\n' "$1" "$2" "$3" | tee -a "$WORK/run.log"
}

# execd_ping_case: GET /ping control on the resolved route (must answer
# instantly with 200; the issue's baseline).
execd_ping_case() {
	local meta rc=0 http elapsed
	meta="$(curl -sS -m 5 -H "X-Fast-Sandbox-Route-Credential: $EXECD_API_CRED" \
		-o /dev/null -w '%{http_code} %{time_total}' "$(execd_base_url)/ping" 2>/dev/null)" \
		|| rc=$?
	http="$(awk '{print $1}' <<<"$meta")"
	elapsed="$(awk '{print $2}' <<<"$meta")"
	if [[ "$http" == "200" ]]; then
		record_api_row "ping" PASS "HTTP 200 in ${elapsed}s"
	else
		record_api_row "ping" FAIL "http=${http:-000} rc=$rc"
	fi
}

# execd_command_case posts one JSON /command body over the resolved route and
# classifies the outcome. Row verdicts:
#   PASS       expected outcome arrived: execution_complete SSE (exit 0),
#              OR the structured error SSE execd sends for non-zero exits
#              (expect=error-event), OR the fast 400 (expect=error400)
#   RESPONDED  HTTP 200 with SSE bytes but neither completion nor error event
#   HANG       nothing received until the curl timeout (issue #1695 symptom)
#   FAIL       outcome mismatched the expectation / other transport error
execd_command_case() { # name timeout-s expect body
	local name="$1" timeout_s="$2" expect="$3" body="$4"
	local meta rc=0 http bytes elapsed snippet has_complete has_error
	meta="$(curl -sS -N -m "$timeout_s" \
		-H "X-Fast-Sandbox-Route-Credential: $EXECD_API_CRED" \
		-H 'Content-Type: application/json' -H 'Accept: text/event-stream' \
		--data-binary "$body" \
		-o "$WORK/execd-api-$name.out" \
		-w '%{http_code} %{size_download} %{time_total}' \
		"$(execd_base_url)/command" 2>"$WORK/execd-api-$name.err")" \
		|| rc=$?
	http="$(awk '{print $1}' <<<"$meta")"; http="${http:-000}"
	bytes="$(awk '{print $2}' <<<"$meta")"; bytes="${bytes:-0}"
	elapsed="$(awk '{print $3}' <<<"$meta")"
	# Keep the RAW execd response for every case in the run log (assertions
	# alone would hide what execd actually answered).
	{
		printf 'execd-api case %-9s raw response (http=%s rc=%s):\n' "$name" "$http" "$rc"
		sed 's/^/    execd> /' "$WORK/execd-api-$name.out" 2>/dev/null
		sed 's/^/    execd> /' "$WORK/execd-api-$name.err" 2>/dev/null
	} >> "$WORK/run.log" 2>/dev/null || true
	if [[ "$expect" == "error400" ]]; then
		if [[ "$http" == "400" ]]; then
			record_api_row "$name" PASS "malformed JSON -> HTTP 400 in ${elapsed}s (request reaches execd)"
		elif [[ "$http" == "000" && "$rc" == "28" ]]; then
			record_api_row "$name" HANG "malformed JSON ALSO hangs (${timeout_s}s, no response)"
		else
			record_api_row "$name" FAIL "expected HTTP 400, got $http (rc=$rc)"
		fi
		return 0
	fi
	has_complete="$(grep -c "execution_complete" "$WORK/execd-api-$name.out" 2>/dev/null || true)"
	has_error="$(grep -c '"type":"error"' "$WORK/execd-api-$name.out" 2>/dev/null || true)"
	if [[ "$has_complete" -gt 0 ]]; then
		# execd streams execution_complete only for exit code 0.
		if [[ "$expect" == "error-event" ]]; then
			record_api_row "$name" FAIL "expected an error SSE for a non-zero exit, got execution_complete"
		else
			record_api_row "$name" PASS "execution_complete in ${elapsed}s (http=$http, $bytes bytes)"
		fi
	elif [[ "$has_error" -gt 0 ]]; then
		# execd reports non-zero exits / launch failures via a structured
		# SSE error event (no execution_complete) — correct protocol.
		if [[ "$expect" == "error-event" ]]; then
			record_api_row "$name" PASS "structured error SSE in ${elapsed}s (non-zero exit, http=$http, $bytes bytes)"
		else
			snippet="$(head -c 160 "$WORK/execd-api-$name.out" 2>/dev/null | tr '\n|' ' ;' | cut -c1-160)"
			record_api_row "$name" FAIL "error SSE where completion was expected: $snippet"
		fi
	elif [[ "$http" == "200" && "$bytes" -gt 0 ]]; then
		snippet="$(head -c 160 "$WORK/execd-api-$name.out" 2>/dev/null | tr '\n|' ' ;' | cut -c1-160)"
		record_api_row "$name" RESPONDED "SSE bytes but no completion/error event: $snippet"
	elif [[ "$rc" == "28" ]]; then
		record_api_row "$name" HANG "curl timeout ${timeout_s}s, $bytes bytes (http=$http)"
	else
		snippet="$(head -c 160 "$WORK/execd-api-$name.err" 2>/dev/null | tr '\n|' ' ;' | cut -c1-160)"
		record_api_row "$name" FAIL "http=$http rc=$rc bytes=$bytes err: $snippet"
	fi
}

# execd_slot_netns finds a live per-sandbox slot netns inside the sandbox's
# fastlet pod (the netns whose VM tap vmtap0 is UP). Prints "pod netns".
execd_slot_netns() { # sandbox-name
	local sbx="$1" pod lines ns
	pod="$(kubectl -n "$NS" get sandbox "$sbx" -o jsonpath='{.status.placement.fastletName}' 2>/dev/null)"
	[[ -n "$pod" ]] || return 1
	lines="$(kubectl -n "$NS" exec -c fastlet "pod/$pod" -- ip netns list 2>/dev/null)" || return 1
	while read -r ns _; do
		[[ -n "$ns" ]] || continue
		if kubectl -n "$NS" exec -c fastlet "pod/$pod" -- ip netns exec "$ns" ip link show vmtap0 2>/dev/null | grep -q "state UP"; then
			printf '%s %s\n' "$pod" "$ns"
			return 0
		fi
	done <<<"$lines"
	return 1
}

# execd_netns_control probes the guest execd straight from the slot netns
# (guest 172.30.0.3:44772 — zero proxies) with the fastlet image's busybox
# wget. The /ping control runs FIRST and gates the /command probe: when the
# guest is not reachable from the netns at all (netns/ping also fails) the
# rows are SKIP — that is a data-plane/path issue, not execd, and must not
# pollute the API verdict. Skipped entirely when the applet is unavailable.
execd_netns_control() { # sandbox-name
	local sbx="$1" pod ns ping_out cmd_out rc=0
	read -r pod ns < <(execd_slot_netns "$sbx") || {
		record_api_row "netns" SKIP "no live slot netns found in the fastlet pod"
		return 0
	}
	if ! kubectl -n "$NS" exec -c fastlet "pod/$pod" -- busybox wget --help 2>&1 | grep -q -- "--post-data"; then
		record_api_row "netns" SKIP "busybox wget (--post-data) unavailable in the fastlet image"
		return 0
	fi
	ping_out="$(kubectl -n "$NS" exec -c fastlet "pod/$pod" -- ip netns exec "$ns" \
		busybox wget -S -T 8 -qO - \
		--header 'Content-Type: application/json' \
		"http://172.30.0.3:44772/ping" 2>&1)" || rc=$?
	if ! grep -q "HTTP/1.1 200\|HTTP/1.0 200" <<<"$ping_out"; then
		record_api_row "netns" SKIP "guest not reachable from slot netns $ns (netns/ping fails: data-plane path, not execd)"
		return 0
	fi
	record_api_row "netns/ping" PASS "guest execd answered 200 directly in netns $ns"
	cmd_out="$(kubectl -n "$NS" exec -c fastlet "pod/$pod" -- ip netns exec "$ns" \
		busybox wget -S -T 8 -qO - \
		--header 'Content-Type: application/json' \
		--header 'Accept: text/event-stream' \
		--post-data '{"command":"echo probe-ok"}' \
		"http://172.30.0.3:44772/command" 2>&1)" || rc=$?
	if grep -q "execution_complete" <<<"$cmd_out"; then
		record_api_row "netns/command" PASS "guest execd completed directly in netns $ns"
	elif grep -q '"type":"error"' <<<"$cmd_out"; then
		record_api_row "netns/command" PASS "guest execd answered with a structured error SSE in netns $ns"
	else
		record_api_row "netns/command" HANG "no guest /command response in netns $ns (rc=$rc): $(head -c 120 <<<"$cmd_out")"
	fi
}

# execd_api_dump_console captures execd's own log lines from the guest
# serial console (firecracker.log) BEFORE the sandbox is deleted (cleanup
# removes the jail). execd logs to /dev/console and the jailer stores the
# firecracker stdout (the console) under
# <StateRoot>/jails/firecracker/<uid[:32]>/root/firecracker.log, readable
# from the fastlet pod. Best effort — a missing/unreadable log only warns.
execd_api_dump_console() { # sandbox-name
	local name="$1" pod uid32 logfile
	pod="$(kubectl -n "$NS" get sandbox "$name" -o jsonpath='{.status.placement.fastletName}' 2>/dev/null)" || return 0
	uid32="$(kubectl -n "$NS" get sandbox "$name" -o jsonpath='{.metadata.uid}' 2>/dev/null | cut -c1-32)"
	[[ -n "$pod" && -n "$uid32" ]] || return 0
	logfile="/var/lib/fast-sandbox/firecracker/jails/firecracker/$uid32/root/firecracker.log"
	if ! kubectl -n "$NS" exec -c fastlet "pod/$pod" -- sh -c "test -s '$logfile'" >/dev/null 2>&1; then
		log "execd-api: guest console $logfile not found on $pod (evidence capture skipped)"
		return 0
	fi
	highlight "  key node: guest console evidence for '$name' (execd logs from firecracker.log)"
	log "execd-api: execd-relevant console lines ($logfile):"
	kubectl -n "$NS" exec -c fastlet "pod/$pod" -- sh -c \
		"grep -aE 'Requested: (POST|GET)|crypto/rand|received command|StreamEvent|CommandExecError|starting OpenSandbox| error' '$logfile' | tail -60" \
		| sed 's/^/    /' | tee -a "$WORK/run.log" || true
	log "execd-api: raw console tail (last 25 lines):"
	kubectl -n "$NS" exec -c fastlet "pod/$pod" -- sh -c "tail -25 '$logfile'" \
		| sed 's/^/    /' | tee -a "$WORK/run.log" || true
	pass "guest console evidence dumped before teardown"
}

verify_execd_api_sandbox() { # sandbox-name
	local name="$1"
	fastctl_run_sandbox "$name"
	wait_until "execd /ping on $name" 120000 probe_execd "$name"
	execd_route_resolve "$name" || fail "execd route resolve failed for $name"
	log "execd-api: route $(execd_base_url) (DIRECT_FASTLET_PROXY, credential route)"
	pass "sandbox $name running, execd /ping reachable end-to-end"
}

verify_execd_api_battery() {
	local body_pipe
	EXECD_API_CASES=()
	execd_ping_case
	execd_command_case "bad-json" 5 error400 '{not-json'
	execd_command_case "echo" 8 completed '{"command":"echo probe-ok"}'
	body_pipe="{\"command\":\"printf 'a b c\\n' | wc -w\"}"
	execd_command_case "pipe" 8 completed "$body_pipe"
	execd_command_case "sleep" 10 completed '{"command":"sleep 1 && echo done-after-sleep"}'
	execd_command_case "false" 8 error-event '{"command":"/bin/false"}'
	execd_command_case "missing" 8 error-event '{"command":"definitely-not-a-real-binary-xyz"}'
	pass "route battery complete (see rows above)"
}

verify_execd_api_netns() { # sandbox-name
	execd_netns_control "$1"
	pass "slot-netns controls complete (no proxy; SKIP when the guest is unreachable from the netns)"
}

verify_execd_api_cleanup() { # sandbox-name
	local name="$1" pod uid32
	if [[ "$EXECD_API_KEEP_SANDBOX" == "1" ]]; then
		# The battery hung (issue #1695). Keep the VM alive so the execd
		# side can be inspected: execd logs to /dev/console, the jailer
		# captures firecracker stdout (the serial console) in
		# <StateRoot>/jails/firecracker/<sandbox-id[:32]>/root/firecracker.log.
		# The battery's own hang attempts are ALREADY in that log — read it
		# first (look for "received command", "StreamEvent.OnExecuteInit",
		# "CommandExecError"); for a live replay, tail BEFORE re-running the
		# battery (the next run's leftover removal recycles this sandbox).
		pod="$(kubectl -n "$NS" get sandbox "$name" -o jsonpath='{.status.placement.fastletName}' 2>/dev/null)"
		uid32="$(kubectl -n "$NS" get sandbox "$name" -o jsonpath='{.metadata.uid}' 2>/dev/null | cut -c1-32)"
		log "diagnosis: sandbox $name KEPT (EXECD_API_KEEP_SANDBOX=1)"
		log "diagnosis: this battery's execd log lines are in firecracker.log on pod $pod:"
		log "  kubectl -n $NS exec -c fastlet pod/$pod -- sh -c 'grep -aE \"starting OpenSandbox|received command|StreamEvent|CommandExecError|error\" /var/lib/fast-sandbox/firecracker/jails/firecracker/$uid32/root/firecracker.log | tail -80'"
		log "diagnosis: for a LIVE replay, run this tail in one terminal BEFORE the next battery:"
		log "  kubectl -n $NS exec -c fastlet pod/$pod -- sh -c 'tail -f /var/lib/fast-sandbox/firecracker/jails/firecracker/*/root/firecracker.log'"
		log "  then: EXECD_API_KEEP_SANDBOX=1 ./scripts/integration-env.sh verify-execd-api"
		log "diagnosis: teardown afterwards with:  fastctl delete $name  (or re-run verify-execd-api)"
		rm -f "$WORK"/execd-api-*.out "$WORK"/execd-api-*.err
		return 0
	fi
	fastctl delete "$name" >/dev/null 2>&1 || true
	wait_for "execd-api sandbox gone" 120 sandbox_gone "$name"
	rm -f "$WORK"/execd-api-*.out "$WORK"/execd-api-*.err
	pass "sandbox $name deleted; workspace clean"
}

# --- verify-p2p: DART data-plane evidence (stage 2) ---------------------------
# Proves the wiring end to end on the real store: agent-signed presigned URL
# -> node-local DART prefix route -> origin fetch (first read) -> DART block
# cache (second read, zero new origin blocks). Cross-node peer hits need a
# second worker; on the single-node topology the cache-hit half is asserted
# and the origin counter is recorded as the baseline.
dart_source_counters() { # pod  (stdout: "cache <n>"; "peer <n>"; "origin <n>")
	local pod="$1" metrics
	metrics="$(kubectl exec -n "$NS" "$pod" -- sh -c 'curl -fsS --noproxy "*" http://127.0.0.1:8147/metrics' 2>/dev/null || true)"
	printf '%s\n' "$metrics" | grep -E '^dart_block_source_total' \
		| sed -E 's/^dart_block_source_total\{source="([a-z]+)"\} ([0-9]+)$/\1 \2/' || true
}

dart_source_delta() { # before-file after-file -> "cache <n> peer <n> origin <n>"
	local source delta line_before line_after value_before value_after
	for source in cache peer origin; do
		line_before="$(grep "^$source " "$1" || true)"
		line_after="$(grep "^$source " "$2" || true)"
		value_before="${line_before##* }"; value_after="${line_after##* }"
		delta=$(( ${value_after:-0} - ${value_before:-0} ))
		printf '%s %d\n' "$source" "$delta"
	done
}

verify_p2p() {
	local manifest_ref manifest_key build_key probe_key presigned probe_url
	local pods pod node before after cache_delta origin_delta peer_delta
	manifest_ref="$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.manifestRef}')"
	[[ "$manifest_ref" == s3://* ]] || die "manifestRef is not an s3 URL: $manifest_ref"
	manifest_key="${manifest_ref#s3://$MINIO_BUCKET/}"
	build_key="$(dirname "$manifest_key")"
	probe_key="$build_key/rootfs.ext4"

	# The presigned URL must name the MinIO container IP on the kind network
	# (the node containers' view of the store), not the host loopback the
	# dev alias signs for.
	local endpoint_host
	endpoint_host="${MINIO_ENDPOINT#http://}"
	[[ -n "$endpoint_host" ]] || die "MINIO_ENDPOINT is empty (up must run first)"
	mc alias set chain-net "$MINIO_ENDPOINT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1 || true
	presigned="$(mc presign --expiry 1h "chain-net/$MINIO_BUCKET/$probe_key")" \
		|| die "mc presign failed for $probe_key"
	log "verify-p2p probe object: $probe_key ($(mc stat --json "chain-net/$MINIO_BUCKET/$probe_key" 2>/dev/null | jq -r '.size' 2>/dev/null || echo '?' ) bytes)"

	pods="$(kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[*].metadata.name}')"
	[[ -n "$pods" ]] || die "no agent pods"
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		before="$(mktemp)"; after="$(mktemp)"
		dart_source_counters "$pod" > "$before"
		# First read: cold blocks must come from the origin.
		log "p2p $node: first read (cold, origin expected)"
		kubectl exec -n "$NS" "$pod" -- sh -c \
			"curl -fsS --noproxy '*' -o /dev/null 'http://127.0.0.1:8145/dart/$presigned'" \
			|| die "first DART read failed on $node"
		# Second read: served from the DART block cache, origin delta = 0.
		log "p2p $node: second read (warm, cache expected)"
		kubectl exec -n "$NS" "$pod" -- sh -c \
			"curl -fsS --noproxy '*' -o /dev/null 'http://127.0.0.1:8145/dart/$presigned'" \
			|| die "second DART read failed on $node"
		dart_source_counters "$pod" > "$after"
		while read -r source delta; do
			case "$source" in
				cache) cache_delta="$delta" ;;
				peer) peer_delta="$delta" ;;
				origin) origin_delta="$delta" ;;
			esac
		done < <(dart_source_delta "$before" "$after")
		log "p2p $node: block_source deltas cache=+$cache_delta peer=+$peer_delta origin=+$origin_delta"
		if [[ "$cache_delta" -gt 0 && "$origin_delta" -eq 0 ]]; then
			pass "p2p $node: warm read served by the DART block cache (origin delta = 0)"
		else
			fail "p2p $node: warm read did not hit the cache (cache=+$cache_delta origin=+$origin_delta)"
		fi
		rm -f "$before" "$after"
	done
	highlight "== verify-p2p complete: presign -> DART -> origin (cold) -> block cache (warm) =="
	highlight "   cross-node peer hits require a 2-worker cluster (see docs/guides/firecracker-integration-env.md §8)"
}

verify_execd_api() {
	local name="$EXECD_API_SBX" row verdict
	local api_hang=0 api_pass=0 api_other=0 api_skip=0 api_total=0
	port_forward_up
	resolve_daemon_up
	trap 'port_forward_down; resolve_daemon_down' EXIT
	run_stage "execd-api 1: sandbox create + execd /ping (route plumbing)" \
		verify_execd_api_sandbox "$name"
	run_stage "execd-api 2: API battery over the fastlet-proxy route" \
		verify_execd_api_battery
	run_stage "execd-api 3: no-proxy controls (slot netns -> guest 172.30.0.3)" \
		verify_execd_api_netns "$name"
	for row in "${EXECD_API_CASES[@]}"; do
		verdict="${row#*|}"
		verdict="${verdict%%|*}"
		case "$verdict" in
			HANG) api_hang=$((api_hang + 1)) ;;
			PASS) api_pass=$((api_pass + 1)) ;;
			SKIP) api_skip=$((api_skip + 1)) ;;
			*) api_other=$((api_other + 1)) ;;
		esac
		api_total=$((api_total + 1))
	done
	highlight "  key node: execd API verdict on '$name' (execd protocol, issue #1695)"
	if [[ "$api_hang" -gt 0 ]]; then
		printf '    REPRODUCED #1695: %d/%d API cases hang (0 bytes) while /ping stays up\n' \
			"$api_hang" "$api_total" | tee -a "$WORK/run.log"
		printf '    root-cause read: all command cases HANG -> synchronous init stall (stdlog\n    descriptor / getShell / launchManaged); only echo/pipe/sleep HANG -> stdout\n    file/tail pipeline\n' | tee -a "$WORK/run.log" >/dev/null
	elif [[ "$api_other" -eq 0 ]]; then
		printf '    NOT REPRODUCED: execd API fully usable (%d/%d PASS, %d SKIP control rows)\n' \
			"$api_pass" "$api_total" "$api_skip" | tee -a "$WORK/run.log"
	else
		printf '    PARTIAL: %d PASS, %d HANG, %d other, %d SKIP of %d — inspect rows above\n' \
			"$api_pass" "$api_hang" "$api_other" "$api_skip" "$api_total" | tee -a "$WORK/run.log"
	fi
	run_stage "execd-api 4: guest console evidence (firecracker.log, auto-captured)" \
		execd_api_dump_console "$name"
	run_stage "execd-api 5: sandbox delete + cleanup" verify_execd_api_cleanup "$name"
	trap - EXIT
	resolve_daemon_down
	port_forward_down
	stage_summary
	highlight "== verify-execd-api complete: execd protocol battery + no-proxy controls =="
}

# --- verify-egress: egress Actions-channel integration -----------------------
# Network policy is delivered over the Sandbox Actions channel only this
# phase (SET_BINDING binding.input / LIFECYCLE_HOOK / REMOVE_BINDING); the
# credential channel (proxy route / UID / vault) is deferred. Requires the
# requirement-owned egress image loaded into Kind.
# See docs/design/egress-integration-plan.md / egress-integration-plan-tasks.md.
EGRESS_POOL="firecracker-egress-pool"
EGRESS_IMAGE="docker.io/opensandbox/egress:latest"
EGRESS_SBX="sandbox-egress"

egress_image_ready() {
	# docker images reports repositories without the docker.io registry
	# prefix (docker pull opensandbox/egress:latest lands as
	# opensandbox/egress:latest), so match either spelling.
	local local_ref
	local_ref="$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -E '(^|/)opensandbox/egress:latest$' | head -n 1)" || {
		fail "egress image opensandbox/egress:latest is not present in local docker; load it first (docker pull opensandbox/egress:latest)"
		return 1
	}
	# Skip the (slow) re-load when the image is already inside the Kind
	# nodes; kind load is otherwise repeated on every verify-egress run.
	if kind_node_image_present "$EGRESS_IMAGE"; then
		log "egress image already loaded into Kind (skipping kind load)"
		return 0
	fi
	log "loading egress image into Kind (this can take minutes for large images)..."
	timeout 600 kind load docker-image "$local_ref" --name "$KIND_CLUSTER"
}

kind_node_image_present() { # image
	local node images image="$1" bare="${1#docker.io/}"
	node="$(kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -n 1)"
	[[ -n "$node" ]] || return 1
	images="$(docker exec "$node" crictl images --output json 2>/dev/null)"
	grep -q "\"$image\"" <<< "$images" || grep -q "\"$bare\"" <<< "$images"
}

egress_fastlet_pod() {
	# Fallback used before the sandbox exists (pool readiness): pick any
	# pod of this pool — every fastlet pod carries an egress sidecar.
	kubectl -n "$NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$EGRESS_POOL" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# egress_sandbox_pod resolves the fastlet pod a sandbox is placed on (the
# pool runs multiple pods; e.g. the restart stage must kill the egress
# sidecar the sandbox actually uses so the Fastlet replay happens there).
egress_sandbox_pod() { # sandbox
	kubectl -n "$NS" get sandbox "$1" -o jsonpath='{.status.placement.fastletName}' 2>/dev/null
}

# egress_pool_pods lists every fastlet pod of the egress pool. The pool runs
# poolMin=2 pods and a sandbox can land on either, so log/count assertions
# must scan all of them instead of pinning one pod.
egress_pool_pods() {
	local out attempt
	for attempt in 1 2 3; do
		out="$(kubectl -n "$NS" get pods \
			-l "app=sandbox-fastlet,fast-sandbox.io/pool=$EGRESS_POOL" \
			-o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
		if [[ -n "$out" ]]; then
			echo "$out"
			return 0
		fi
		sleep 1
	done
	return 1
}

egress_container_ready() {
	local pod
	for pod in $(egress_pool_pods); do
		if kubectl -n "$NS" get pod "$pod" -o jsonpath='{range .status.containerStatuses[*]}{.name}{"="}{.ready}{" "}{end}' 2>/dev/null | grep -q 'egress=true'; then
			return 0
		fi
	done
	return 1
}

# egress_warm_ready reports whether the recreated egress pool has cached its
# warm image (the fastlet can only accept Sandboxes after the agent pull +
# capacity heartbeat; creating earlier races into ResourceExhausted).
egress_warm_ready() {
	kubectl -n "$NS" get sandboxpool "$EGRESS_POOL" -o jsonpath='{.status.warmImages[*].cachedFastlets}' 2>/dev/null | grep -qv '^0*$'
}

# Protocol cross-verification (live): GET /_fastlet/v1/actions/status must
# echo the shared apiVersion, ready=true, and a non-empty instanceId. The
# egress Handler binds Pod loopback only (127.0.0.1:18080), so the probe
# runs inside the egress container (the image ships curl + nftables).
egress_status_ready() {
	local pod out
	pod="$(egress_fastlet_pod)"
	[[ -n "$pod" ]] || return 1
	out="$(kubectl -n "$NS" exec "pod/$pod" -c egress -- \
		curl -fsS -m 5 "http://127.0.0.1:18080/_fastlet/v1/actions/status" 2>/dev/null)"
	[[ "$out" == *'"apiVersion":"sandbox.fast.io/actions/v1"'* && "$out" == *'"ready":true'* && "$out" == *'"instanceId":'* ]]
}

egress_log_has() { # pattern
	local pod found="" lines
	for pod in $(egress_pool_pods); do
		lines="$(kubectl -n "$NS" logs "pod/$pod" -c egress --tail=300 2>/dev/null)"
		log "egress log check $pod: lines=$(grep -c . <<< "$lines") match=$(grep -c "$1" <<< "$lines")"
		if grep -q "$1" <<< "$lines"; then
			found=1
			break
		fi
	done
	if [[ -z "$found" ]]; then
		log "egress log pattern '$1' not found (pool pods: $(egress_pool_pods 2>/dev/null | tr '\n' ' '))"
	fi
	[[ -n "$found" ]]
}

# Count of SET_BINDING-applied lines across every pool pod's egress log.
# Used to distinguish a policy-update SET_BINDING from the initial one.
egress_set_binding_count() {
	local total=0 pod n
	for pod in $(egress_pool_pods); do
		n="$(kubectl -n "$NS" logs "pod/$pod" -c egress --tail=2000 2>/dev/null | grep -c "SET_BINDING applied")"
		total=$((total + n))
	done
	echo "$total"
}

# Update the Sandbox's egress ActionBinding input (policy update) via the
# fastpath UpdateSandbox RPC: the Fastlet only reacts to RPCs, so a direct
# kubectl patch of the Sandbox CR would never reach it. The RPC re-delivers
# SET_BINDING with the new binding.input; an already-active subject applies
# the policy in place (no Hook replay).
egress_patch_policy() { # sandbox policy-json
	# Output is kept: a silent update failure otherwise stalls the policy
	# wait below for no reason.
	fastctl update "$1" --action "egress=$2"
}

# Egress policy probes run INSIDE the sandbox guest through execd POST
# /command (full end-to-end: user command -> guest kernel -> slot gateway ->
# egress DNS proxy + per-subject nft enforcement) — no more emitting traffic
# from the fastlet pod's slot netns. For egress pools the driver injects the
# guest resolver (nameserver <slot gateway>), so DNS queries reach the
# egress DNS proxy via the gateway redirect; execd /command itself only
# became usable on the firecracker snapshot after the golden kernel switched
# to the 6.1 microvm kernel (VMGenID reseeds the guest CRNG at every
# restore — see verify-execd-api and OpenSandbox #1695).
EGRESS_PROBE_URL="http://example.com/"
EGRESS_DENY_POLICY='{"defaultAction":"deny"}'
# allow example.com (domain, resolved IPs land in the dyn_v4 set) plus a
# static IP (1.1.1.1, allow_v4) so IP-direct probing has a stable,
# reliably reachable target.
EGRESS_ALLOW_POLICY='{"egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"1.1.1.1"}],"defaultAction":"deny"}'

# --- egress policy assertions run IN-GUEST (end-to-end) -------------------
# DNS deny/allow, dyn_v4 probe IP and IP-direct assertions execute commands
# INSIDE the guest via execd /command (pod->guest direct at the current slot
# IP), so they traverse the guest -> egress data plane end-to-end — the
# delivery semantics users get. The subject netns helpers are kept ONLY as
# the plane-liveness readiness gate (a netns query exercises the egress
# plane without touching execd).
# execd control-path checks under an active egress policy (issue #1704
# discriminator): /ping + /command echo from the pod DIRECTLY into the guest
# execd (slot IP DNAT). If these succeed while guest-originated DNS replies
# are lost, the failure is the egress reply path — NOT egress blocking the
# execd control connection. Single-shot (no hammering).
# egress_print_raw prints the raw execd SSE response line-by-line (one
# frame per output line, blank frames preserved) inside a labeled fence so
# nothing is flattened or truncated. Emits on stderr (console + run.log);
# stdout is reserved for the decoded probe data.
egress_print_raw() { # label (reads lines on stdin)
	local label="$1"
	{
		printf '  --- execd raw: %s ---\n' "$label"
		cat
		printf '  --- end %s ---\n' "$label"
	} | tee -a "$WORK/run.log" >&2
}

egress_execd_run() { # sandbox-name command timeout-s -> command stdout; rc 0 on execution_complete
	local sbx="$1" body="$2" timeout_s="$3" url out rc http body_out attempt
	attempt=0
	while :; do
		attempt=$((attempt + 1))
		if ! execd_route_resolve "$sbx"; then
			log "egress proxy probe for $sbx: route resolve failed (attempt $attempt)"
			[[ "$attempt" -ge 8 ]] && return 1
			sleep 3
			continue
		fi
		url="$(execd_base_url)/command"
		out="$(curl -sS -m "$timeout_s" -N \
			-H "X-Fast-Sandbox-Route-Credential: $EXECD_API_CRED" \
			-H 'Content-Type: application/json' -H 'Accept: text/event-stream' \
			--data-binary "$body" -w $'\nHTTP=%{http_code}' \
			"$url" 2>/dev/null)"
		rc=$?
		http="$(sed -n 's/^HTTP=//p' <<<"$out" | tail -1)"
		body_out="$(sed '$d' <<<"$out")"
		log "egress proxy probe for $sbx (POST $url): rc=$rc http=${http:-000} cmd=$body"
		printf '%s\n' "$body_out" | egress_print_raw "POST $url cmd=$body http=$http rc=$rc" || true
		if [[ "$rc" -eq 0 && "$http" == "200" ]]; then
			break
		fi
		# Transient route/hang window after subject activation; poll.
		if [[ "$attempt" -ge 8 ]]; then
			log "egress proxy probe for $sbx: still failing after $attempt attempts"
			return 1
		fi
		sleep 3
	done
	jq -r 'select(.type == "stdout") | .text' <<<"$body_out" 2>/dev/null
	grep -q "execution_complete" <<<"$body_out"
}

egress_execd_ping() { # sandbox -> rc 0 when /ping answers 200 (through fastlet-proxy)
	local sbx="$1" url http no_cred attempt
	attempt=0
	while :; do
		attempt=$((attempt + 1))
		if ! execd_route_resolve "$sbx"; then
			log "egress proxy ping for $sbx: route resolve failed (attempt $attempt)"
			[[ "$attempt" -ge 8 ]] && return 1
			sleep 3
			continue
		fi
		url="$(execd_base_url)/ping"
		http="$(curl -sS -m 8 -o /dev/null -w '%{http_code}' \
			-H "X-Fast-Sandbox-Route-Credential: $EXECD_API_CRED" "$url" 2>/dev/null)"
		log "execd control path for $sbx (GET $url): http=${http:-000}"
		if [[ "$http" == "200" ]]; then
			return 0
		fi
		# Route-absent vs wrong-target discriminator: the fastlet-proxy
		# looks the route up BEFORE validating the credential, so an
		# unauthenticated probe answers 401/403 only when the route exists
		# in the store of the proxy actually reached — and 404 (route not
		# found) when it does not. A 401/403 here while the credential
		# probe 404s means the local forward lands on a DIFFERENT proxy
		# than the sandbox's fastlet (issue #37 local-forward drift), not a
		# missing route.
		no_cred="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null)"
		log "egress proxy ping for $sbx: diag no-credential on same local port -> http=${no_cred:-000} (401/403=route present here, 404=route absent here)"
		# Resolve-host vs forward-target vs placement cross-check: the resolve
		# host comes from the controller Registry entry of the sandbox's
		# ASSIGNMENT ANNOTATION, which can lag or disagree with the pod the
		# sandbox actually runs on (status.placement). Print who the probe
		# intended to reach, who the local forward actually lands on, and who
		# the sandbox reports as its placement.
		log "egress proxy ping for $sbx: diag resolve-host=$EXECD_API_HOST ($(fastlet_pod_name_for_ip "$EXECD_API_HOST" 2>/dev/null || echo 'no-such-pod')) placement=$(egress_sandbox_pod "$sbx" 2>/dev/null || echo '?') fwd=$(ps -eo args 2>/dev/null | grep -E "[k]ubectl -n $NS port-forward .*$EXECD_API_LPORT:5780" | head -1 | sed 's/^/[/;s/$/]/' || echo 'no-forward')"
		# Full map state: index-aligned IP/port/pid triples vs the real
		# kubectl port-forward processes. If the pid recorded for the
		# resolve host is alive but the port is served by a DIFFERENT
		# process, the map has drifted from the listener it created.
		local -i diag_i
		for diag_i in "${!FASTLET_IPS[@]}"; do
			local fwd_line
			fwd_line="$(ps -eo pid,args 2>/dev/null | grep -E "[k]ubectl -n $NS port-forward .*${FASTLET_PORTS[$diag_i]}:5780" | head -1 || true)"
			log "egress proxy ping for $sbx: diag map[$diag_i] ip=${FASTLET_IPS[$diag_i]} port=${FASTLET_PORTS[$diag_i]} pid=${FASTLET_PIDS[$diag_i]} alive=$(kill -0 "${FASTLET_PIDS[$diag_i]}" 2>/dev/null && echo yes || echo no) port-listening=$(tcp_listening "${FASTLET_PORTS[$diag_i]}" && echo yes || echo no) fwd=${fwd_line:-none}"
		done
		if [[ "$attempt" -ge 8 ]]; then
			log "egress proxy ping for $sbx: still failing after $attempt attempts"
			ps -eo args 2>/dev/null | grep -E "[k]ubectl -n $NS port-forward .*:5780" | sed 's/^/    forward> /' >&2 || true
			return 1
		fi
		if [[ "$attempt" -eq 2 ]]; then
			# A single failed probe can be a stale/foreign forward owning the
			# local port (the probe reached the WRONG fastlet). Deterministic
			# recovery: kill every forward and rebuild the map from the live
			# pod list, then keep polling on the fresh mapping.
			log "egress proxy ping for $sbx: rebuilding fastlet port-forwards after attempt $attempt"
			rebuild_fastlet_forwards
		fi
		sleep 3
	done
}

egress_execd_control() { # sandbox
	egress_execd_ping "$1" && egress_execd_run "$1" '{"command":"echo execd-control-ok"}' 12 >/dev/null
}

# egress_subject_netns() etc. — the slot netns name and gateway are read
# from the driver state meta.json.
egress_subject_netns() { # sandbox -> "netns gateway"
	local sbx="$1" pod uid meta ns gw
	pod="$(egress_sandbox_pod "$sbx")" || return 1
	uid="$(kubectl -n "$NS" get sandbox "$sbx" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$pod" && -n "$uid" ]] || return 1
	meta="/var/lib/fast-sandbox/firecracker/sandboxes/$uid/meta.json"
	ns="$(kubectl -n "$NS" exec -c fastlet "pod/$pod" -- sh -c \
		"grep -m1 -oE '\"namespacePath\"[: ]*\"[^\"]+' $meta | sed 's#.*/##'" 2>/dev/null)"
	gw="$(kubectl -n "$NS" exec -c fastlet "pod/$pod" -- sh -c \
		"grep -m1 -oE '\"gateway\"[: ]*\"[0-9.]+' $meta | grep -oE '[0-9.]+$'" 2>/dev/null)"
	[[ -n "$ns" && -n "$gw" ]] || return 1
	printf '%s %s\n' "$ns" "$gw"
}

egress_subject_dns() { # sandbox dnsname -> rc (0 = resolved)
	local sbx="$1" dnsname="$2" ns gw out rc
	read -r ns gw < <(egress_subject_netns "$sbx") || return 1
	out="$(kubectl -n "$NS" exec -c fastlet "pod/$(egress_sandbox_pod "$sbx")" -- \
		ip netns exec "$ns" timeout 6 nslookup "$dnsname" "$gw" 2>&1)"
	rc=$?
	log "egress netns probe for $sbx (netns $ns, gw $gw, nslookup $dnsname): rc=$rc $(tr '\n' ' ' <<<"$out")"
	[[ "$rc" -eq 0 ]] && grep -q "Name:" <<<"$out"
}

egress_plane_ready() { # sandbox  (netns liveness; gate only, never assert policy through it)
	egress_subject_dns "$1" "example.com"
}

# --- IN-GUEST (end-to-end) policy assertions via execd /command ----------
# DNS deny/allow, dyn_v4 probe IP and IP-direct probes execute INSIDE the
# guest through execd (pod->guest direct at the current slot IP), so they
# traverse the guest -> egress data plane end-to-end. The netns helpers
# above remain only as the plane-liveness readiness gate.

egress_guest_reachable() { # sandbox  (IN-GUEST end-to-end)
	local out rc=0
	if out="$(egress_execd_run "$1" \
		'{"command":"timeout 6 nslookup example.com 2>&1"}' 12)"; then
		rc=0
	else
		rc=1
	fi
	log "egress in-guest probe for $1 (execd /command nslookup example.com): rc=$rc $(tr '\n' ' ' <<<"$out")"
	[[ "$rc" -eq 0 ]] && grep -q "Name:" <<<"$out"
}

# egress_probe_ip extracts the first IPv4 A record resolved IN-GUEST (the
# proxy's Server header line carries ':53' and is skipped by the bare-IP
# match).
egress_probe_ip() { # sandbox -> ip
	local out
	if ! out="$(egress_execd_run "$1" '{"command":"timeout 6 nslookup example.com 2>&1"}' 12)"; then
		return 1
	fi
	awk '/^Address:/ { if ($2 ~ /^[0-9.]+$/) { print $2; exit } }' <<<"$out"
}

# egress_ip_reachable asserts IP-direct (DNS-free) TCP from the guest is
# accepted: only the subj_ chain governs (dyn/allow sets accept, final drop
# otherwise). A deny drops the SYN silently, so nc -w bounds the probe.
egress_ip_reachable() { # sandbox ip
	egress_execd_run "$1" "{\"command\":\"nc -w 6 $2 80 </dev/null >/dev/null 2>&1\"}" 20 >/dev/null
}

# egress_dyn_v4_has asserts the DNS-resolved IP landed in the subject's
# dynamic nft set (dyn_v4), which is what makes the follow-up TCP accepted.
egress_dyn_v4_has() { # sandbox ip
	local pod uid set
	pod="$(egress_sandbox_pod "$1")" || return 1
	uid="$(kubectl -n "$NS" get sandbox "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$uid" ]] || return 1
	set="subj_s_${uid//-/_}_dyn_v4"
	kubectl -n "$NS" exec "pod/$pod" -c egress -- nft list set inet opensandbox-fast-sandbox "$set" 2>/dev/null | grep -q "$2"
}

# EGRESS_IP is a well-known, reliably reachable public address used for
# IP-direct probing; deny-first drops it via the subj_ chain regardless
# of DNS.
EGRESS_IP="1.1.1.1"

egress_binding_state() { # sandbox
	kubectl -n "$NS" get sandbox "$1" -o jsonpath="{.status.actionBindings[?(@.handler=='egress')].state}" 2>/dev/null
}

egress_binding_ready() { # sandbox
	[[ "$(egress_binding_state "$1")" == "Ready" ]]
}

# egress_nft_subjects_clean asserts the per-subject chains are really gone
# from the host data plane on every pool pod. The egress image implements
# enforcement with nft (table opensandbox-fast-sandbox, per-subject chain
# subj_<id>), so the CLI must be present; a missing CLI fails the stage
# instead of skipping the assertion.
egress_nft_subjects_clean() {
	local pod rules any
	any=""
	for pod in $(egress_pool_pods); do
		rules="$(kubectl -n "$NS" exec "pod/$pod" -c egress -- nft list ruleset 2>/dev/null)"
		[[ -n "$rules" ]] || { fail "nft CLI unavailable in the egress container; cannot verify rule unload"; return 1; }
		any+="$rules"
	done
	! grep -q 'subj_' <<< "$any"
}

# Create the egress sandbox after removing any leftover of the same name.
# A stale Sandbox keeps its placement pointing at a replaced Fastlet pod
# (assignedPodLost), so the controller never reconciles its bindings —
# policy updates via fastctl update would silently not reach the Fastlet.
# Deletion uses kubectl directly: fastctl delete goes through fastpath,
# which consults the (possibly dead) assigned Fastlet and can wedge.
egress_run_sandbox() { # sandbox policy-json
	local name="$1" attempt=0
	if kubectl -n "$NS" get sandbox "$name" >/dev/null 2>&1; then
		fastctl delete "$name" >/dev/null 2>&1 \
			|| kubectl -n "$NS" delete sandbox "$name" --ignore-not-found >/dev/null 2>&1 \
			|| true
		while kubectl -n "$NS" get sandbox "$name" >/dev/null 2>&1; do
			attempt=$((attempt + 1))
			if [[ "$attempt" -ge 30 ]]; then
				# Last resort: drop the cleanup finalizer so a dead
				# assigned Fastlet cannot wedge the delete.
				kubectl -n "$NS" patch sandbox "$name" --type=merge \
					-p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
			fi
			if [[ "$attempt" -ge 60 ]]; then
				fail "leftover $name could not be removed"
			fi
			sleep 1
		done
	fi
	# The pool was just (re)created: the controller may not have the new
	# fastlet's capacity/heartbeat yet ("no eligible Fastlet"), so retry —
	# mirrors fastctl_run_sandbox. Output is kept for diagnosis.
	attempt=0
	for attempt in $(seq 1 30); do
		if out="$(fastctl run "$name" --image "$SBX_IMAGE" --pool "$EGRESS_POOL" --action "egress=$2" 2>&1)"; then
			[[ -n "$out" ]] && printf '%s\n' "$out"
			return 0
		fi
		if sandbox_exists "$name"; then
			# A create that succeeded but errored on the wire: already exists.
			printf '%s\n' "$out"
			return 0
		fi
		printf '%s\n' "$out" >&2
		log "egress_run_sandbox $name: attempt $attempt failed (pool recreated?); retrying"
		sleep 2
	done
	fail "fastctl run $name failed after 30 attempts"
}

# egress_restart_worker kills the egress worker process so the Fastlet
# detects the instance change and replays SET_BINDING. Killing the egress
# main process takes the container (and the exec session) down with it, so
# the kubectl exec exit code is not meaningful — the replay assertions
# follow separately.
egress_restart_worker() { # pod
	kubectl -n "$NS" exec "pod/$1" -c egress -- \
		sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = "egress" ] && kill ${p#/proc/}; done' \
		>/dev/null 2>&1 || true
}

verify_egress() {
	local sbx_a="$EGRESS_SBX" sbx_b="$EGRESS_SBX-2" sbx_pod probe_ip binding_n

	port_forward_up
	resolve_daemon_up

	run_stage "egress 0: egress image available + loaded into Kind" egress_image_ready

	# A fresh single-fastlet pool guarantees BOTH sandboxes land on the SAME
	# egress instance — the core scenario under test is multiple subjects
	# with different policies on one egress control plane.
	run_stage "egress 1: reset egress pool (single fastlet) + egress container ready" \
		kubectl -n "$NS" delete sandboxpool "$EGRESS_POOL" --ignore-not-found
	kubectl -n "$NS" apply -f config/samples/pool-firecracker-egress.yaml
	wait_for "exactly one egress fastlet pod" 180 egress_single_pod
	wait_for "egress container ready in the fastlet pod" 180 egress_container_ready
	wait_for "egress pool warm image cached (fastlet can accept sandboxes)" \
		300 egress_warm_ready
	pass "egress (host-process) container running in a single fastlet pod; pool warm"

	# The initial forward sweep ran before this pool existed; the execd
	# probes deliver through the fastlet-proxy PORT route, so sweep again
	# now that the egress fastlet pod is up and warm.
	log "egress: re-sweeping port-forwards to cover the egress pool fastlet"
	port_forward_up

	trap 'port_forward_down; resolve_daemon_down' EXIT
	run_stage "egress 2: actions/status protocol cross-verification" egress_status_ready
	pass "GET /_fastlet/v1/actions/status: apiVersion + ready + instanceId (protocol agreement)"

	run_stage "egress 3: create A (deny) and B (allow) on the same fastlet" \
		egress_run_sandbox "$sbx_a" "$EGRESS_DENY_POLICY"
	wait_for "egress binding Ready on A" 120 egress_binding_ready "$sbx_a"
	wait_for "egress log: SET_BINDING applied" 15 egress_log_has "SET_BINDING applied"
	run_stage "egress 3: create B" \
		egress_run_sandbox "$sbx_b" "$EGRESS_ALLOW_POLICY"
	wait_for "egress binding Ready on B" 120 egress_binding_ready "$sbx_b"
	sbx_pod="$(egress_sandbox_pod "$sbx_a")"
	[[ -n "$sbx_pod" && "$sbx_pod" == "$(egress_sandbox_pod "$sbx_b")" ]] || {
		fail "A and B landed on different fastlet pods ($sbx_pod vs $(egress_sandbox_pod "$sbx_b")) — single-pool assumption broken"
	}
	pass "A and B co-located on fastlet $sbx_pod (one egress instance)"

	# READINESS GATE: the egress DNS plane must demonstrably serve before any
	# policy assertion is meaningful (the plane intermittently does not
	# answer DNS for a stretch right after pool/subject activation). Wait
	# until B(allow) genuinely resolves from the subject netns with a long
	# deadline; everything below (A deny included) runs only after this gate.
	wait_for "egress DNS plane serving (netns liveness): B resolves" \
		300 egress_plane_ready "$sbx_b"
	pass "egress readiness gate: DNS plane serving (netns liveness)"

	# Control (AFTER the plane gate: pod->guest connections are reset while
	# the plane is still settling, so in-guest checks must not run before
	# it): is execd itself reachable in the guest while an egress policy is
	# ACTIVE? /ping + /command echo from the pod directly into the guest
	# discriminate "egress blocks the execd control path" from "egress
	# replies to guest-originated traffic are lost" (issue #1704).
	run_stage "egress 3c: execd control path intact under egress policy (ping + echo)" \
		egress_execd_control "$sbx_b"
	pass "execd reachable in the guest under an active egress policy"

	# Default state: two subjects, different policies, on the same egress.
	run_stage "egress 4: A deny (DNS + IP blocked)" test_egress_denied "$sbx_a"
	run_stage "egress 4: A IP-direct blocked" test_egress_ip_denied "$sbx_a" "$EGRESS_IP"
	pass "subject A: example.com blocked and $EGRESS_IP dropped (deny)"
	wait_for "egress 4: B allow (DNS resolves)" 60 egress_guest_reachable "$sbx_b"
	pass "subject B: example.com allowed (DNS) on the same egress"
	probe_ip="$(egress_probe_ip "$sbx_b")"
	[[ -n "$probe_ip" ]] || fail "could not resolve example.com for B"
	wait_for "resolved IP $probe_ip in B's dyn_v4 set" 15 egress_dyn_v4_has "$sbx_b" "$probe_ip"
	pass "B: DNS-resolved IP $probe_ip entered dyn_v4"
	wait_for "B: IP-direct $EGRESS_IP accepted" 30 egress_ip_reachable "$sbx_b" "$EGRESS_IP"
	pass "per-subject isolation on one egress: A denied while B allowed"

	# Switch A deny -> allow; B must stay unaffected.
	binding_n="$(egress_set_binding_count)"
	run_stage "egress 5: A -> allow policy" egress_patch_policy "$sbx_a" "$EGRESS_ALLOW_POLICY"
	wait_for "A binding Ready after policy switch" 120 egress_binding_ready "$sbx_a"
	wait_for "A: SET_BINDING re-applied" 30 egress_count_gt "$binding_n"
	wait_for "A: egress allowed after switch" 90 egress_guest_reachable "$sbx_a"
	wait_for "A: IP-direct $EGRESS_IP accepted after switch" 90 egress_ip_reachable "$sbx_a" "$EGRESS_IP"
	wait_for "B: still allowed while A switched" 90 egress_guest_reachable "$sbx_b"
	pass "A allow applied; B unaffected on the same egress"

	# Switch B allow -> deny; A must stay unaffected.
	binding_n="$(egress_set_binding_count)"
	run_stage "egress 6: B -> deny policy" egress_patch_policy "$sbx_b" "$EGRESS_DENY_POLICY"
	wait_for "B binding Ready after deny switch" 120 egress_binding_ready "$sbx_b"
	wait_for "B: SET_BINDING re-applied" 30 egress_count_gt "$binding_n"
	run_stage "egress 6: B blocked after switch" test_egress_denied "$sbx_b"
	run_stage "egress 6: B IP-direct blocked after switch" test_egress_ip_denied "$sbx_b" "$EGRESS_IP"
	wait_for "A: still allowed while B switched to deny" 90 egress_guest_reachable "$sbx_a"
	pass "B deny applied; A unaffected (per-subject policy isolation)"

	# Restart the egress worker: the Fastlet replays BOTH subjects; each
	# keeps its own policy.
	binding_n="$(egress_set_binding_count)"
	run_stage "egress 7: egress worker restart -> Fastlet replays A and B" \
		egress_restart_worker "$sbx_pod"
	wait_for "egress log: SET_BINDING replayed after restart" 30 egress_count_gt "$binding_n"
	pass "Handler restart replay observed (new instanceId; A and B re-bound)"
	wait_for "A: still allowed after replay" 90 egress_guest_reachable "$sbx_a"
	run_stage "egress 7: B still denied after replay" test_egress_denied "$sbx_b"
	pass "per-subject policies survive the egress restart"

	# Delete A: its subject rules unload; B's must stay until B is deleted.
	run_stage "egress 8: delete A; A rules unload while B's remain" \
		fastctl delete "$sbx_a"
	wait_for "A gone" 60 egress_sandbox_gone "$sbx_a"
	wait_for "egress log: REMOVE_BINDING complete" 15 egress_log_has "REMOVE_BINDING complete"
	wait_for "A subject chain unloaded" 30 egress_subject_absent "$sbx_a" "$sbx_pod"
	wait_for "B subject chain still present" 15 egress_subject_present "$sbx_b" "$sbx_pod"
	pass "deleting A unloaded A's subj_ chain only (B untouched)"

	run_stage "egress 9: delete B; all subject rules unloaded" \
		fastctl delete "$sbx_b"
	wait_for "B gone" 60 egress_sandbox_gone "$sbx_b"
	wait_for "no subject chains left in nft" 60 egress_nft_subjects_clean
	pass "deleting B unloaded the remaining per-subject nft rules"

	run_stage "egress 10: cleanup — egress pool removed" \
		kubectl -n "$NS" delete sandboxpool "$EGRESS_POOL" --ignore-not-found

	trap - EXIT
	port_forward_down
	resolve_daemon_down
	stage_summary
	highlight "== verify-egress complete: multi-subject policies on one egress green =="
}
# --- verify-egress helpers ---------------------------------------------------

egress_sandboxes_gone() { # a b
	! sandbox_exists "$1" && ! sandbox_exists "$2"
}

egress_sandbox_gone() { # sandbox
	! sandbox_exists "$1"
}

egress_single_pod() {
	[[ "$(kubectl -n "$NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$EGRESS_POOL" -o name 2>/dev/null | wc -l | tr -d ' ')" -eq 1 ]]
}

# egress_subject_present / egress_subject_absent assert whether the sandbox's
# per-subject nft chain exists on the (single) egress fastlet pod. Used for
# the per-sandbox rule-unload checks during staged deletion.
egress_subject_present() { # sandbox pod
	local uid
	uid="$(kubectl -n "$NS" get sandbox "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$uid" ]] || return 1
	kubectl -n "$NS" exec "pod/$2" -c egress -- \
		nft list chain inet opensandbox-fast-sandbox "subj_s_${uid//-/_}" 2>/dev/null | grep -q "chain subj_s_"
}

egress_subject_absent() { # sandbox pod
	local uid
	uid="$(kubectl -n "$NS" get sandbox "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$uid" ]] || return 0
	! kubectl -n "$NS" exec "pod/$2" -c egress -- \
		nft list chain inet opensandbox-fast-sandbox "subj_s_${uid//-/_}" 2>/dev/null | grep -q "chain subj_s_"
}

egress_count_gt() { # count
	[[ "$(egress_set_binding_count)" -gt "$1" ]]
}

# test_egress_denied fails the stage if the guest unexpectedly reaches the
# probe URL (policy leak), passes when egress is really blocked.
test_egress_denied() { # sandbox
	if egress_guest_reachable "$1"; then
		fail "guest egress to $EGRESS_PROBE_URL was NOT blocked (policy leak)"
	fi
	pass "guest egress to $EGRESS_PROBE_URL blocked"
}

# test_egress_ip_denied asserts the per-subject nft chain drops IP-direct
# traffic under a deny policy.
test_egress_ip_denied() { # sandbox ip
	if egress_ip_reachable "$1" "$2"; then
		fail "IP-direct egress to $2 was NOT blocked (nft rule leak)"
	fi
	pass "IP-direct egress to $2 blocked"
}

# --- status --------------------------------------------------------------------------------------
status() {
	log "status: components"
	kubectl -n "$NS" get pods -o wide 2>/dev/null || true
	echo
	log "status: SandboxTemplate"
	kubectl -n "$NS" get sandboxtemplate -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,MANIFEST:.status.manifestRef' 2>/dev/null || true
	echo
	log "status: SandboxPool"
	kubectl -n "$NS" get sandboxpool -o custom-columns='NAME:.metadata.name,RUNTIME:.spec.runtime,WARM_IMAGES:.status.warmImages' 2>/dev/null || true
	echo
	log "status: Sandboxes"
	kubectl -n "$NS" get sandbox -o wide 2>/dev/null || true
	echo
	log "status: DART P2P (block_source/cache/peer/origin per node)"
	dart_metrics_summary || true
	echo
	log "status: MinIO"
	docker ps --filter "name=$MINIO_CONTAINER" --format '{{.Names}} {{.Status}}' 2>/dev/null || true
	if kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null; then
		log "kind cluster: $KIND_CLUSTER (up)"
	else
		log "kind cluster: down"
	fi
}

# dart_metrics_summary prints the DART block-source counters and member count
# per agent node — the stage-2 acceptance evidence (origin amplification:
# N nodes pulling the same image should show origin fetches ~once, peers
# serving the rest).
dart_metrics_summary() {
	local pods pod node metrics
	pods="$(kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
	[[ -n "$pods" ]] || { echo "  (no agent pods)"; return 0; }
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
		metrics="$(kubectl exec -n "$NS" "$pod" -- sh -c 'curl -fsS --noproxy "*" http://127.0.0.1:8147/metrics' 2>/dev/null || true)"
		echo "  $node:"
		if [[ -z "$metrics" ]]; then
			echo "    (DART metrics unreachable)"
			continue
		fi
		printf '%s\n' "$metrics" | grep -E '^dart_block_source_total\{source="(cache|peer|origin)"\}' \
			| sed 's/^/    /' || true
	done
}

# --- down ---------------------------------------------------------------------------------------
down() {
	log "down: teardown"
	if kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null; then
		kind delete cluster --name "$KIND_CLUSTER" > "$LOGS_DIR/kind-delete.log" 2>&1 || true
	fi
	[[ -z "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
		|| fail "kind cluster $KIND_CLUSTER still exists after delete"
	docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	[[ -z "$(docker ps -a --filter "name=$MINIO_CONTAINER" --format '{{.Names}}' || true)" ]] \
		|| fail "MinIO container still present"
	rm -f "$WORK/agent-registry.json" "$WORK/registry.json"
	rm -rf "$GEN_DIR"
	# Root-owned MinIO object store (written by the container); leaving it
	# behind pollutes the repo and breaks docker build contexts.
	sudo_ rm -rf "$MINIO_DATA"
	sysctl_restore
	stateroot_xfs_down
	# Purge the runtime cache the environment owns. The pull layer treats a
	# committed cache as FINAL (idempotent, never refreshed), so a rebuilt
	# SandboxTemplate would otherwise keep being ignored on the next up when
	# the StateRoot survives teardown (e.g. XFS_STATEROOT=0 plain directory).
	# The XFS loop-image case is already covered by stateroot_xfs_down.
	if [[ -d "$XFS_MOUNT_POINT/firecracker" ]]; then
		log "down: purging node runtime cache under $XFS_MOUNT_POINT/firecracker"
		sudo_ rm -rf "$XFS_MOUNT_POINT/firecracker/images" \
			"$XFS_MOUNT_POINT/firecracker/agent" \
			"$XFS_MOUNT_POINT/firecracker/jails" \
			"$XFS_MOUNT_POINT/firecracker/cache" 2>/dev/null || true
	fi
	if docker network ls --format '{{.Name}}' | grep -qx 'kind'; then
		log "note: docker network 'kind' remains (kind-wide, reused on next up)"
	fi
	pass "host cleanup complete"
}

# --- main ------------------------------------------------------------------------------------------
# env_summary prints the reachable facts of the environment for hand-off.
env_summary() {
	highlight "== environment summary =="
	printf '  %-20s %s\n' "MinIO endpoint" "$MINIO_ENDPOINT"
	printf '  %-20s %s (execd=%s)\n' "image" "$SBX_IMAGE" "$EXECD"
	printf '  %-20s %s\n' "manifestRef" "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.manifestRef}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "template phase" "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.phase}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "fastlet pod" "$(kubectl -n "$NS" get pods -l app=sandbox-fastlet -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "warmImages" "$(kubectl -n "$NS" get sandboxpool "$SBX_POOL" -o jsonpath='{.status.warmImages[*].image}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "StateRoot fs" "$(findmnt -no FSTYPE "$XFS_MOUNT_POINT" 2>/dev/null || echo 'ext4 (full copy per sandbox)')"
	printf '  %-20s %s\n' "logs" "$LOGS_DIR"
}

usage() {
	cat <<'EOF'
usage: integration-env.sh [--cleanup|--auto-clean] {up|down|status|verify}

  up       build the whole environment (tasks 1-9) and report status
  down     teardown: kind cluster + MinIO container + credentials + sysctl
  status   component / template / pool / sandbox health
  verify   create 2 sandboxes, probe execd /ping, then a max-concurrency
           batch (CONCURRENCY=5 at pool capacity), delete all, assert cleanup
  verify-p2p
           DART data-plane evidence (stage 2): presigned URL -> node-local
           DART -> origin (cold) -> block cache (warm, origin delta 0);
           cross-node peer hits need a 2-worker cluster (guide §8)
  verify-execd-api
           execd HTTP API usability battery in the Firecracker guest
           (OpenSandbox #1695): /ping, POST /command SSE (echo/pipe/sleep/
           false/missing command classes), malformed-JSON 400, plus
           no-proxy slot-netns controls; verdict: hang reproduced or not;
           auto-captures the guest console (execd logs from firecracker.log)
           before teardown
  verify-egress
           egress Actions-channel integration: pool apply, protocol
           cross-verification, SET_BINDING -> hooks -> REMOVE_BINDING
           lifecycle, restart replay, teardown (requires the egress image)

  --cleanup     down after an interrupted run (same recovery as down)
  --auto-clean  on up failure, run down automatically before dumping logs
EOF
	exit 1
}

for arg in "$@"; do
	case "$arg" in
		--cleanup) ACTION="down" ;;
		--auto-clean) AUTO_CLEAN=1 ;;
		up|down|status|verify|verify-p2p|verify-execd-api|verify-egress) ACTION="$arg" ;;
		*) usage ;;
	esac
done
[[ -n "$ACTION" ]] || usage

mkdir -p "$WORK" "$LOGS_DIR"

# Before any go/make/docker invocation, and after $WORK exists so log() works.
inherit_go_env

# Route the builder's implicit Docker Hub images through REGISTRY_MIRROR.
apply_registry_mirror

case "$ACTION" in
	up)
		exec > >(tee -a "$WORK/run.log") 2>&1
		log "=== integration-env up ($(date -u +%FT%TZ)) ==="
		{
			echo "environment snapshot ($(date -u +%FT%TZ))"
			command -v kind >/dev/null && kind --version
			kubectl version --client 2>/dev/null | head -1
			go version
			docker --version
			echo "minio=$MINIO_IMAGE minioPort=$MINIO_PORT bucket=$MINIO_BUCKET"
			echo "fcVersion=$FC_VERSION sbxImage=$SBX_IMAGE execd=$EXECD warmImages=$WARM_IMAGES"
			echo "images: controller=$IMG_CONTROLLER agent=$IMG_AGENT builder=$IMG_BUILDER"
		} > "$LOGS_DIR/environment.txt" 2>&1 || true
		if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
			|| docker ps -a --format '{{.Names}}' | grep -qx "$MINIO_CONTAINER"; then
			if [[ "$SKIP_LEFTOVER_CLEAN" == 1 ]]; then
				log "leftover resources detected; aborting (SKIP_LEFTOVER_CLEAN=1). Run 'integration-env.sh down' first"
				exit 1
			fi
			log "leftover resources detected; cleaning and rebuilding"
			down
		fi
		trap 'on_error up' ERR
		run_stage "task 1: preflight + tooling" preflight
		run_stage "task 1: sysctl (fs.inotify)" sysctl_set
		run_stage "task 1: build images (7)" build_images
		run_stage "task 1: XFS StateRoot (reflink)" stateroot_xfs_up
		run_stage "task 2: kind cluster (KVM passthrough)" kind_up
		run_stage "task 3: MinIO + bucket" minio_up
		run_stage "task 3: MinIO endpoint (kind network)" resolve_minio_endpoint
		run_stage "task 4: CRDs + controller" controller_up
		run_stage "task 3: credentials (publish/pull)" credentials_up
		run_stage "task 5: firecracker node assets" installer_up
		run_stage "task 6: runtime-agent DaemonSet" agent_up
		run_stage "task 7: SandboxTemplate build" template_up
		run_stage "task 8: SandboxPool + warmImages" pool_up
		trap - ERR
		stage_summary
		env_summary
		highlight "== up complete: run 'integration-env.sh verify' (or 'status') =="
		;;
	verify)
		trap 'on_error verify' ERR
		verify
		trap - ERR
		;;
	verify-p2p)
		trap 'on_error verify-p2p' ERR
		verify_p2p
		trap - ERR
		;;
	verify-execd-api)
		trap 'on_error verify-execd-api' ERR
		verify_execd_api
		trap - ERR
		;;
	verify-egress)
		trap 'on_error verify_egress' ERR
		verify_egress
		trap - ERR
		;;
	status)
		status
		;;
	down)
		down
		;;
esac
