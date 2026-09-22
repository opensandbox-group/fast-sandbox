#!/usr/bin/env bash
# sandboxtemplate-e2e.sh — exercise the SandboxTemplate conversion core
# (cmd/sandboxtemplate-builder) on a real KVM host, without Kubernetes.
#
# The default path builds the builder image from build/Dockerfile.sandboxtemplate-builder
# and runs the pipeline inside a container (privileged, /dev/kvm, the test
# image tarball mounted read-only, the workspace mounted as /build):
#
#   OCI image (docker save tarball) → ext4 rootfs with the OpenSandbox
#   runtime injected → cold boot → guest readiness → full snapshot →
#   restore validation → manifest + SHA256SUMS.
#
# With --local the builder binary is compiled on the host and the pipeline
# runs directly against the host toolchain instead.
#
# Requirements:
#   - Linux x86_64 with /dev/kvm (root; the script re-invokes itself with sudo)
#   - docker (to export the test image and run the builder container),
#     go toolchain (--local mode), e2fsprogs, jq (manifest display/assertions)
#
# Usage:
#   ./scripts/sandboxtemplate-e2e.sh [--image <ref|tar>] [--format native|overlaybd] [--local]

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.sandboxtemplate-e2e}"
IMAGE="${SANDBOX_TEMPLATE_IMAGE:-opensandbox/fsb-sandbox-golden:latest}"
# Formats to exercise; defaults to both. --format may be repeated or a
# comma-separated list to run a subset.
FORMATS=()
BUILDER_IMAGE="${SANDBOX_TEMPLATE_BUILDER_IMAGE:-sandboxtemplate-builder:e2e}"
KERNEL_URL="${SANDBOX_TEMPLATE_KERNEL_URL:-https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260722-38359b8055fc-0/x86_64/vmlinux-6.1.176}"
LOCAL_MODE=0

log() { printf '\033[1;34m[st-e2e]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[st-e2e] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --image) IMAGE="${2:?--image requires a value}"; shift 2 ;;
        --format)
            value="${2:?--format requires a value}"; shift 2
            IFS=',' read -r -a parts <<< "$value"
            for part in "${parts[@]}"; do
                [[ "$part" == "native" || "$part" == "overlaybd" ]] || die "--format must be native or overlaybd, got $part"
                FORMATS+=("$part")
            done ;;
        --local) LOCAL_MODE=1; shift ;;
        *) die "unknown argument: $1" ;;
    esac
done
if [[ ${#FORMATS[@]} -eq 0 ]]; then
    FORMATS=(native overlaybd)
fi

# --- re-invoke as root -------------------------------------------------------
if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null; then
        log "re-invoking as root (sudo -E)"
        exec sudo -E env "PATH=$PATH" "WORK=$WORK" "SANDBOX_TEMPLATE_IMAGE=$IMAGE" \
            "SANDBOX_TEMPLATE_KERNEL_URL=$KERNEL_URL" "$0" "$@"
    fi
    die "must run as root (or install sudo)"
fi

[[ "$(uname -m)" == "x86_64" ]] || die "requires x86_64"
[[ -e /dev/kvm ]] || die "/dev/kvm is missing"
command -v docker >/dev/null || die "missing required command: docker"
command -v go >/dev/null || die "missing required command: go (for --local mode and spec checks)"
command -v jq >/dev/null || die "missing required command: jq (manifest assertions)"
command -v debugfs >/dev/null || die "missing required command: debugfs (e2fsprogs, guest env file assertions)"

log "workspace: $WORK (formats=${FORMATS[*]}, local=$LOCAL_MODE)"
rm -rf "$WORK"
mkdir -p "$WORK/input"

# --- test image --------------------------------------------------------------
# The env contract is asserted end to end, so the pipeline always builds from
# a tiny image derived on top of the requested base: the derivation adds two
# known ENVs — E2E_IMAGE_ONLY (must be inherited untouched) and
# E2E_OVERRIDE_ME (shadowed by the same name in spec.envs) — plus a spec-only
# env and a console-printing entrypoint in spec.json. Tarball inputs are
# loaded into the local daemon first (the build itself stays offline: FROM
# resolves locally and there is no RUN).
IMAGE_TAR="$WORK/input/image.tar"
ENV_IMAGE="sandboxtemplate-e2e:env"
if [[ "$IMAGE" == *.tar ]]; then
    log "loading test image tarball $IMAGE"
    load_output=$(docker load -i "$IMAGE") || die "docker load failed: $IMAGE"
    base_ref=$(sed -n 's/^Loaded image: //p' <<<"$load_output" | head -1)
    if [[ -z "$base_ref" ]]; then
        base_ref=$(sed -n 's/^Loaded image ID: //p' <<<"$load_output" | head -1)
        [[ -n "$base_ref" ]] || die "docker load produced no image reference for $IMAGE"
        docker tag "$base_ref" "sandboxtemplate-e2e:env-base" >/dev/null 2>&1 || die "docker tag failed"
        base_ref="sandboxtemplate-e2e:env-base"
    fi
else
    docker pull -q "$IMAGE" >/dev/null 2>&1 || die "docker pull failed: $IMAGE"
    base_ref="$IMAGE"
fi
log "building env-verification image $ENV_IMAGE (base: $base_ref)"
ENV_CTX="$WORK/input/env-ctx"
mkdir -p "$ENV_CTX"
cat > "$ENV_CTX/Dockerfile" <<EOF
FROM $base_ref
ENV E2E_IMAGE_ONLY=from-image
ENV E2E_OVERRIDE_ME=from-image
EOF
docker build -q -t "$ENV_IMAGE" "$ENV_CTX" >/dev/null 2>&1 || die "env-verification image build failed"
docker save "$ENV_IMAGE" -o "$IMAGE_TAR" >/dev/null 2>&1 || die "docker save failed"

# --- runner ------------------------------------------------------------------
if [[ "$LOCAL_MODE" -eq 1 ]]; then
    command -v oci2rootfs >/dev/null || die "missing oci2rootfs on PATH (or use the docker mode)"
    command -v firecracker >/dev/null || die "missing firecracker on PATH (or use the docker mode)"
    if [[ "${FORMATS[*]}" == *overlaybd* ]]; then
        command -v overlaybd-import-raw >/dev/null || die "missing overlaybd-import-raw on PATH (needed for format=overlaybd; use the docker mode)"
    fi
    log "building sandboxtemplate-builder (local mode)"
    (cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/sandboxtemplate-builder" ./cmd/sandboxtemplate-builder/)
    run_pipeline() {
        local fmt_dir=$1
        env SANDBOX_TEMPLATE_SPEC="$(cat "$fmt_dir/spec.json")" \
            SANDBOX_TEMPLATE_WORKDIR="$fmt_dir/build" \
            SANDBOX_TEMPLATE_IMAGE_TAR="$IMAGE_TAR" \
            SANDBOX_TEMPLATE_ALLOW_NO_PUBLISH=1 \
            POD_NAME=e2e-pod POD_NAMESPACE=default \
            "$WORK/sandboxtemplate-builder"
    }
else
    log "building builder image from build/Dockerfile.sandboxtemplate-builder"
    docker build --progress=plain --build-arg "KERNEL_URL=$KERNEL_URL" -t "$BUILDER_IMAGE" \
        -f "$REPO_ROOT/build/Dockerfile.sandboxtemplate-builder" "$REPO_ROOT" || die "docker build failed"
    run_pipeline() {
        local fmt_dir=$1
        docker run --rm \
            --privileged \
            --device /dev/kvm:/dev/kvm \
            --device /dev/net/tun:/dev/net/tun \
            -v "$WORK/input":/input:ro \
            -v "$fmt_dir/build":/build \
            -e SANDBOX_TEMPLATE_SPEC="$(cat "$fmt_dir/spec.json")" \
            -e SANDBOX_TEMPLATE_WORKDIR=/build \
            -e SANDBOX_TEMPLATE_IMAGE_TAR=/input/image.tar \
            -e SANDBOX_TEMPLATE_ALLOW_NO_PUBLISH=1 \
            -e POD_NAME=e2e-pod -e POD_NAMESPACE=default \
            "$BUILDER_IMAGE"
    }
fi

# --- run the pipeline per format ---------------------------------------------
overall=0
for fmt in "${FORMATS[@]}"; do
    FMT_DIR="$WORK/$fmt"
    mkdir -p "$FMT_DIR/build"
    printf '{
  "image": "%s",
  "entrypoint": ["/bin/sh", "-c", "echo E2E_ENV image_only=$E2E_IMAGE_ONLY override=$E2E_OVERRIDE_ME spec_only=$E2E_SPEC_ONLY path=$PATH > /dev/console; exec tail -f /dev/null"],
  "kernel": "vmlinux.bin",
  "machine": {"vcpu": "2", "memory": "1Gi"},
  "init": "/usr/local/sbin/sandbox-init",
  "envs": [{"name": "E2E_OVERRIDE_ME", "value": "from-spec"}, {"name": "E2E_SPEC_ONLY", "value": "from-spec"}],
  "readiness": {"warmupSeconds": 15},
  "output": {"rootfsSize": "10Gi", "format": "%s"}
}
' "$ENV_IMAGE" "$fmt" > "$FMT_DIR/spec.json"
    log "running the conversion pipeline (format=$fmt)"
    set +e
    run_pipeline "$FMT_DIR" 2> "$FMT_DIR/pipeline.log"
    status=$?
    set -e
    if [[ $status -ne 0 ]]; then
        echo "=== pipeline.log (tail, format=$fmt) ===" >&2
        tail -120 "$FMT_DIR/pipeline.log" >&2
        overall=1
        continue
    fi

    BUILD="$FMT_DIR/build"
    # Assertions are recorded (not fatal) so a broken format does not abort
    # the remaining formats; the final exit code is non-zero if any failed.
    # Successes are printed too: the output is the verification evidence.
    assert() {
        if [[ $# -lt 2 ]]; then die "assert: usage <description> <command...>"; fi
        local description=$1; shift
        if "$@" >/dev/null 2>&1; then
            echo "  ok: $description (format=$fmt)"
        else
            echo "  FAIL: $description (format=$fmt)" >&2
            overall=1
        fi
    }
    assert "rootfs.ext4 exists" test -s "$BUILD/rootfs.ext4"
    assert "vmstate.snap exists" test -s "$BUILD/vmstate.snap"
    assert "memory.snap exists" test -s "$BUILD/memory.snap"
    assert "manifest.json exists" test -s "$BUILD/manifest.json"
    assert "SHA256SUMS exists" test -s "$BUILD/SHA256SUMS"
    assert "guest reached readiness" grep -q "SANDBOX_READY" "$BUILD/boot.console.log"
    assert "snapshot restore produced a guest heartbeat" grep -q "SANDBOX_HEARTBEAT" "$BUILD/restore.console.log"
    assert "manifest records the baked guest network" jq -e '.guestNetwork.iface == "eth0" and .guestNetwork.ip == "172.30.0.3" and .guestNetwork.mac == "02:00:00:00:00:01" and .guestNetwork.gateway == "172.30.0.1"' "$BUILD/manifest.json"
    assert "boot args bake the static guest IP" grep -q "ip=172.30.0.3::172.30.0.1:255.255.255.0::eth0:off" "$BUILD/boot.console.log"
    assert "manifest marks the template booted and restore-validated" jq -e '.validation.booted == true and .validation.restored == true' "$BUILD/manifest.json"

    # --- env contract --------------------------------------------------------
    # /etc/sandbox-init.env is the only env source the guest init sources.
    # Extract it from the rootfs and assert the exact merge semantics:
    # the image's Config.Env is inherited, spec.envs are attached, and a
    # spec env overrides a same-name image env (exactly one export left).
    guest_env="$BUILD/sandbox-init.env"
    debugfs -R "cat /etc/sandbox-init.env" "$BUILD/rootfs.ext4" > "$guest_env" 2>/dev/null \
        || die "debugfs could not read /etc/sandbox-init.env (format=$fmt)"
    assert "guest env file exports the inherited image PATH" grep -q '^export PATH=' "$guest_env"
    assert "guest env file inherits the image-only env" grep -qx "export E2E_IMAGE_ONLY='from-image'" "$guest_env"
    assert "guest env file keeps the spec-only env" grep -qx "export E2E_SPEC_ONLY='from-spec'" "$guest_env"
    assert "guest env file lets the spec env override the image env" grep -qx "export E2E_OVERRIDE_ME='from-spec'" "$guest_env"
    assert "guest env file drops the overridden image value" test "$(grep -cx "export E2E_OVERRIDE_ME='from-image'" "$guest_env")" -eq 0
    assert "guest env file exports the overridden name exactly once" test "$(grep -cx 'export E2E_OVERRIDE_ME=.*' "$guest_env")" -eq 1
    # The spec entrypoint echoes the live env to /dev/console before the
    # readiness marker: this proves the init actually sourced the merged
    # file inside the guest, not just that the file content looks right.
    assert "running guest sees the inherited image env" grep -q "image_only=from-image" "$BUILD/boot.console.log"
    assert "running guest sees the spec-only env" grep -q "spec_only=from-spec" "$BUILD/boot.console.log"
    assert "running guest sees the overridden env value" grep -q "override=from-spec" "$BUILD/boot.console.log"
    # fsb-sandbox-golden overrides PATH with /opt/sandbox-bin first: seeing
    # it in the live guest proves the image PATH beat the init's hardcoded one.
    assert "running guest PATH inherits the image's /opt/sandbox-bin override" grep -q "path=/opt/sandbox-bin:" "$BUILD/boot.console.log"

    # Positive evidence of the env verification (asserts above print FAIL on
    # failure; this shows what was actually verified):
    log "guest /etc/sandbox-init.env as baked into the rootfs:"
    sed 's/^/    /' "$guest_env"
    log "live guest env (entrypoint console echo):"
    grep -h "E2E_ENV" "$BUILD/boot.console.log" | sed 's/^/    /' || true
    if [[ "$fmt" == "overlaybd" ]]; then
        assert "overlaybd rootfs layer exists" test -s "$BUILD/overlaybd/rootfs/layer.lsmt"
        assert "overlaybd memory layer exists" test -s "$BUILD/overlaybd/memory/layer.lsmt"
    fi

    # Cross-check the builder's sparse-aware digests against sha256sum so a
    # hasher regression cannot pass silently.
    while IFS= read -r path; do
        [[ -f "$BUILD/$path" ]] || { echo "  FAIL: manifest file missing: $path (format=$fmt)" >&2; overall=1; continue; }
        want=$(jq -r --arg p "$path" '.files[$p].sha256' "$BUILD/manifest.json")
        got=$(sha256sum "$BUILD/$path" | awk '{print $1}')
        if [[ "$got" != "$want" ]]; then
            echo "  FAIL: digest mismatch for $path: manifest=$want sha256sum=$got (format=$fmt)" >&2
            overall=1
        fi
    done < <(jq -r '.files | keys[]' "$BUILD/manifest.json")

    log "format=$fmt OK:"
    jq . "$BUILD/manifest.json" 2>/dev/null || cat "$BUILD/manifest.json"
    du -h "$BUILD/rootfs.ext4" "$BUILD/vmstate.snap" "$BUILD/memory.snap" 2>/dev/null || true
    grep -h "SANDBOX_READY" "$BUILD/boot.console.log" | tail -1
    log "template usable (format=$fmt): guest booted to readiness, env contract verified, snapshot restored with heartbeat"
done

if [[ $overall -ne 0 ]]; then
    die "one or more formats failed"
fi
log "E2E passed — artifacts under $WORK/<format>/build"

