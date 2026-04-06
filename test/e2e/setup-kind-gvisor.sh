#!/bin/bash
#
# setup-kind-gvisor.sh - Create KIND cluster with gVisor support
#
# Prerequisites:
#   - runsc installed on host
#   - containerd-shim-runsc-v1 installed on host
#
# Usage:
#   ./setup-kind-gvisor.sh              # Full setup
#   ./setup-kind-gvisor.sh --clean      # Cleanup only
#   ./setup-kind-gvisor.sh --download   # Download gVisor binaries first
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Configuration
CLUSTER_NAME="${GVISOR_KIND_CLUSTER:-gvisor-sandbox}"
KIND_IMAGE="${GVISOR_KIND_IMAGE:-kindest/node:v1.31.0}"
GVISOR_VERSION="${GVISOR_VERSION:-20251215.0}"

# Binary paths (can be overridden)
GVISOR_RUNSC_BIN="${GVISOR_RUNSC_BIN:-/usr/local/bin/runsc}"
GVISOR_SHIM_BIN="${GVISOR_SHIM_BIN:-/usr/local/bin/containerd-shim-runsc-v1}"

# Download directory if using --download
GVISOR_DOWNLOAD_DIR="${ROOT_DIR}/test/e2e/testdata/gvisor"

CLEAN_ONLY="${CLEAN_ONLY:-false}"
DOWNLOAD_FIRST="${DOWNLOAD_FIRST:-false}"

# Parse arguments
for arg in "$@"; do
    case $arg in
        --clean)
            CLEAN_ONLY=true
            ;;
        --download)
            DOWNLOAD_FIRST=true
            ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --clean      Cleanup only"
            echo "  --download   Download gVisor binaries first"
            echo "  --help       Show this help"
            echo ""
            echo "Environment variables:"
            echo "  GVISOR_KIND_CLUSTER   Cluster name (default: gvisor-sandbox)"
            echo "  GVISOR_KIND_IMAGE     Kind node image (default: kindest/node:v1.31.0)"
            echo "  GVISOR_VERSION        gVisor version for download (default: 20251215.0)"
            echo "  GVISOR_RUNSC_BIN      Path to runsc binary"
            echo "  GVISOR_SHIM_BIN       Path to containerd-shim-runsc-v1 binary"
            exit 0
            ;;
    esac
done

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

cleanup() {
    log_info "Cleaning up..."
    kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
}

# Cleanup mode
if [ "$CLEAN_ONLY" = "true" ]; then
    cleanup
    exit 0
fi

# === Download gVisor binaries if requested ===
if [ "$DOWNLOAD_FIRST" = "true" ]; then
    log_info "Downloading gVisor binaries (version: $GVISOR_VERSION)..."

    mkdir -p "$GVISOR_DOWNLOAD_DIR"

    ARCH=$(uname -m)
    BASE_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION}/${ARCH}"

    # Download runsc
    if [ ! -f "${GVISOR_DOWNLOAD_DIR}/runsc" ]; then
        log_info "Downloading runsc..."
        wget -q "${BASE_URL}/runsc" -O "${GVISOR_DOWNLOAD_DIR}/runsc"
        chmod +x "${GVISOR_DOWNLOAD_DIR}/runsc"
    fi
    cp "${GVISOR_DOWNLOAD_DIR}/runsc" "${GVISOR_RUNSC_BIN}"

    # Download containerd-shim-runsc-v1
    if [ ! -f "${GVISOR_DOWNLOAD_DIR}/containerd-shim-runsc-v1" ]; then
        log_info "Downloading containerd-shim-runsc-v1..."
        wget -q "${BASE_URL}/containerd-shim-runsc-v1" -O "${GVISOR_DOWNLOAD_DIR}/containerd-shim-runsc-v1"
        chmod +x "${GVISOR_DOWNLOAD_DIR}/containerd-shim-runsc-v1"
    fi
    cp "${GVISOR_DOWNLOAD_DIR}/containerd-shim-runsc-v1" "${GVISOR_SHIM_BIN}"


    log_info "gVisor binaries downloaded to $GVISOR_DOWNLOAD_DIR"
    exit 0
fi

# === Preflight checks ===
log_info "Preflight checks..."

# Check gVisor binaries
if [ ! -f "$GVISOR_RUNSC_BIN" ]; then
    log_error "runsc not found at $GVISOR_RUNSC_BIN"
    log_error "Install with: sudo apt-get install -y runsc"
    log_error "Or use --download flag"
    exit 1
fi

if [ ! -f "$GVISOR_SHIM_BIN" ]; then
    log_error "containerd-shim-runsc-v1 not found at $GVISOR_SHIM_BIN"
    log_error "Install with: sudo apt-get install -y containerd-shim-runsc-v1"
    log_error "Or use --download flag"
    exit 1
fi

log_info "gVisor binaries found:"
log_info "  runsc: $GVISOR_RUNSC_BIN"
log_info "  shim:  $GVISOR_SHIM_BIN"

# Verify versions
log_info "gVisor version:"
"$GVISOR_RUNSC_BIN" --version || true
"$GVISOR_SHIM_BIN" -v || true

# Check dependencies
for cmd in docker kind kubectl envsubst; do
    if ! command -v $cmd &> /dev/null; then
        log_error "Missing dependency: $cmd"
        exit 1
    fi
done

# === Create KIND cluster ===
log_info "Creating KIND cluster: $CLUSTER_NAME"

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log_warn "Cluster $CLUSTER_NAME already exists"
    read -p "Recreate? (y/N): " confirm
    if [ "$confirm" = "y" ]; then
        kind delete cluster --name "$CLUSTER_NAME"
    else
        log_info "Using existing cluster"
    fi
fi

if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log_info "Creating cluster with gVisor support..."

    # Export variables for envsubst
    export GVISOR_KIND_CLUSTER="$CLUSTER_NAME"
    export GVISOR_KIND_IMAGE="$KIND_IMAGE"
    export GVISOR_RUNSC_BIN="$GVISOR_RUNSC_BIN"
    export GVISOR_SHIM_BIN="$GVISOR_SHIM_BIN"
    export PWD="$ROOT_DIR"

    # Render template and create cluster
    envsubst < "$SCRIPT_DIR/kind-config-gvisor.yaml" | kind create cluster --config -

    log_info "Waiting for cluster ready..."
    kubectl wait --for=condition=Ready node/"${CLUSTER_NAME}-control-plane" --timeout=120s
fi

# Switch context
kubectl config use-context "kind-$CLUSTER_NAME"

# === Configure runsc.toml on nodes ===
log_info "Creating runsc.toml on Kind nodes..."

for node in $(docker ps --filter "name=${CLUSTER_NAME}-" --format "{{.Names}}"); do
    docker exec "$node" sh -c '
        mkdir -p /etc/containerd
        cat > /etc/containerd/runsc.toml <<EOF
log_path = "/var/log/runsc/%ID%/shim.log"
log_level = "debug"

[runsc_config]
platform = "ptrace"
network = "host"
debug = "true"
debug-log = "/var/log/runsc/%ID%/gvisor.%COMMAND%.log"
EOF
    '
    log_info "Configured runsc.toml on $node"
done

# Restart containerd to pick up the config
log_info "Restarting containerd on nodes..."
for node in $(docker ps --filter "name=${CLUSTER_NAME}-" --format "{{.Names}}"); do
    docker exec "$node" pkill -HUP containerd || true
done

# 等待所有组件完全启动
kubectl wait --for=condition=Available --all deployments -n kube-system --timeout=120s

# 检查 API 版本是否可用
kubectl api-versions | grep node.k8s.io

# === Create RuntimeClass ===
log_info "Creating RuntimeClass..."

kubectl apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
scheduling:
  nodeSelector:
    kubernetes.io/arch: amd64
EOF

log_info "load kind images"

kind load docker-image alpine:latest --name "$CLUSTER_NAME"

# === Test gVisor ===
log_info "Testing gVisor runtime..."

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gvisor-test
spec:
  runtimeClassName: gvisor
  containers:
  - name: test
    image: alpine:latest
    command: ["sh", "-c", "uname -a && sleep 5"]
    imagePullPolicy: IfNotPresent
  restartPolicy: Never
EOF

log_info "Waiting for gvisor-test pod..."
kubectl wait --for=condition=Ready pod/gvisor-test --timeout=60s || true

log_info "gVisor test pod output:"
kubectl logs gvisor-test 2>/dev/null || log_warn "Could not get logs"

# Check if output contains gVisor marker
KERNEL_INFO=$(kubectl logs gvisor-test 2>/dev/null || echo "")
if echo "$KERNEL_INFO" | grep -qi "gvisor"; then
    log_info "✅ gVisor isolation verified - kernel shows gVisor marker!"
else
    log_warn "Kernel info: $KERNEL_INFO"
fi

# Cleanup test pod
kubectl delete pod gvisor-test --ignore-not-found=true

# === Deploy fast-sandbox ===
log_info "Deploying fast-sandbox..."

cd "$ROOT_DIR"

# Load base image
docker pull alpine:latest 2>/dev/null || true
kind load docker-image alpine:latest --name "$CLUSTER_NAME"

# Build and load fast-sandbox images
log_info "Building and loading fast-sandbox images..."

make docker-controller docker-agent docker-janitor

kind load docker-image fast-sandbox/controller:dev --name "$CLUSTER_NAME"
kind load docker-image fast-sandbox/agent:dev --name "$CLUSTER_NAME"
kind load docker-image fast-sandbox/janitor:dev --name "$CLUSTER_NAME"

# Deploy CRDs
log_info "Deploying CRDs..."
kubectl apply -f config/crd/
kubectl wait --for=condition=Established crd/sandboxes.sandbox.fast.io --timeout=30s
kubectl wait --for=condition=Established crd/sandboxpools.sandbox.fast.io --timeout=30s

# Deploy RBAC
kubectl apply -f config/rbac/base.yaml

# Deploy controller
kubectl apply -f config/manager/controller.yaml
kubectl rollout status deployment/fast-sandbox-controller --timeout=120s

# Deploy janitor
log_info "Deploying janitor..."
kubectl delete ds fast-sandbox-janitor --ignore-not-found=true
cat config/janitor/janitor.yaml | sed "s/imagePullPolicy:.*/imagePullPolicy: IfNotPresent/" | kubectl apply -f -
kubectl rollout status ds/fast-sandbox-janitor --timeout=60s

# === Summary ===
echo ""
echo "=========================================="
echo "       Setup Complete!"
echo "=========================================="
echo ""
echo "Cluster: $CLUSTER_NAME"
echo ""
echo "Nodes:"
kubectl get nodes
echo ""
echo "Components:"
kubectl get pods -l app=fast-sandbox-controller
kubectl get pods -l app=fast-sandbox-janitor
echo ""
echo "RuntimeClasses:"
kubectl get runtimeclass
echo ""
echo "Next steps:"
echo "  1. Run gVisor E2E tests:"
echo "     go test -v ./test/e2e/suites/secureruntime/... -run TestGVisor"
echo ""