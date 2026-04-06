#!/bin/bash
#
# setup-kind-kata.sh - Create KIND cluster with Kata Containers support
#
# Prerequisites:
#   - KVM enabled on host (/dev/kvm exists)
#   - Nested virtualization enabled
#
# Usage:
#   ./setup-kind-kata.sh              # Full setup
#   ./setup-kind-kata.sh --clean      # Cleanup only
#   ./setup-kind-kata.sh --skip-kata  # Skip Kata installation (fast-sandbox only)
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-kata-sandbox}"
KATA_VERSION="${KATA_VERSION:-3.27.0}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.0}"
SKIP_KATA="${SKIP_KATA:-false}"
CLEAN_ONLY="${CLEAN_ONLY:-false}"

# Kata local cache directory
DATA_DIR="${DATA_DIR:-$HOME/data}"
KATA_TARBALL="kata-static-${KATA_VERSION}-amd64.tar.zst"
KATA_URL="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/${KATA_TARBALL}"

# Parse arguments
for arg in "$@"; do
    case $arg in
        --clean)
            CLEAN_ONLY=true
            ;;
        --skip-kata)
            SKIP_KATA=true
            ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --clean       Cleanup only"
            echo "  --skip-kata   Skip Kata installation"
            echo "  --help        Show this help"
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

# === Preflight checks ===
log_info "Preflight checks..."

# Check KVM
if [ ! -e /dev/kvm ]; then
    log_error "/dev/kvm not found. KVM is required for Kata Containers."
    exit 1
fi
log_info "KVM device found: $(ls -la /dev/kvm)"

# Check nested virtualization (Intel)
if [ -f /sys/module/kvm_intel/parameters/nested ]; then
    NESTED=$(cat /sys/module/kvm_intel/parameters/nested)
    if [ "$NESTED" != "Y" ]; then
        log_warn "Nested virtualization not enabled. Run: modprobe kvm_intel nested=1"
    else
        log_info "Nested virtualization enabled (Intel)"
    fi
fi

# Check dependencies
for cmd in docker kind kubectl; do
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
    log_info "Creating cluster with image: $KIND_IMAGE"
    kind create cluster --name "$CLUSTER_NAME" \
        --config "$SCRIPT_DIR/kind-config-kata.yaml" \
        --image "$KIND_IMAGE"

    log_info "Waiting for cluster ready..."
    kubectl wait --for=condition=Ready node/"${CLUSTER_NAME}-control-plane" --timeout=120s
fi

# Switch context
kubectl config use-context "kind-$CLUSTER_NAME"

# === Install Kata Containers ===
if [ "$SKIP_KATA" != "true" ]; then
    log_info "Installing Kata Containers $KATA_VERSION..."

    NODE_NAME="${CLUSTER_NAME}-control-plane"

    # Ensure local cache directory exists
    mkdir -p "$DATA_DIR"

    # Download Kata tarball to local cache if not exists
    if [ ! -f "$DATA_DIR/$KATA_TARBALL" ]; then
        log_info "Downloading Kata tarball to local cache..."
        log_info "URL: $KATA_URL"
        wget -q --show-progress "$KATA_URL" -O "$DATA_DIR/$KATA_TARBALL" || {
            log_error "Failed to download Kata tarball"
            exit 1
        }
    else
        log_info "Using cached Kata tarball: $DATA_DIR/$KATA_TARBALL"
    fi

    # Install Kata in the KIND node
    log_info "Installing Kata packages in node..."

    # Copy tarball to node and extract
    docker cp "$DATA_DIR/$KATA_TARBALL" "$NODE_NAME:/root/kata.tar.zst"

    docker exec "$NODE_NAME" bash -c "
        set -e

        # Check if already installed
        if [ -d /opt/kata ]; then
            echo 'Kata already installed'
            exit 0
        fi

        apt-get update
        apt-get install -y zstd

        # Extract to /opt/kata
        zstd -d /root/kata.tar.zst -o /root/kata.tar
        tar -xf /root/kata.tar -C /
        rm /root/kata.tar.zst /root/kata.tar

        echo 'Kata installed to /opt/kata'
        ls -la /opt/kata/bin/
    "

    # Configure containerd for Kata
    log_info "Configuring containerd for Kata..."

    docker exec "$NODE_NAME" bash -c '
        set -e

        # Create containerd config directory
        mkdir -p /etc/containerd/certs.d

        # Check current config
        if grep -q "kata-clh" /etc/containerd/config.toml 2>/dev/null; then
            echo "Kata runtime already configured"
            exit 0
        fi

        # Backup original config
        cp /etc/containerd/config.toml /etc/containerd/config.toml.bak 2>/dev/null || true

        # Append Kata runtime configuration
        cat >> /etc/containerd/config.toml <<'EOF'

# Kata Containers runtime configurations
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-clh.toml"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc]
  runtime_type = "io.containerd.kata.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.kata-containers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc.toml"
EOF

        echo "Containerd config updated"
    '

    # Restart containerd
    log_info "Restarting containerd..."
    docker exec "$NODE_NAME" systemctl restart containerd || \
    docker exec "$NODE_NAME" pkill -HUP containerd

    sleep 5

    # Tag pause image for Kata (Kata uses pause:3.8 by default)
    log_info "Setting up pause image for Kata..."
    docker exec "$NODE_NAME" bash -c '
        # Check if pause:3.8 exists, if not tag from pause:3.10
        if ! ctr -n k8s.io image ls | grep -q "pause.*3.8"; then
            if ctr -n k8s.io image ls | grep -q "pause.*3.10"; then
                ctr -n k8s.io image tag registry.k8s.io/pause:3.10 registry.k8s.io/pause:3.8
                echo "Tagged pause:3.10 as pause:3.8"
            fi
        fi
    '

    # Verify Kata installation
    log_info "Verifying Kata installation..."
    docker exec "$NODE_NAME" bash -c '
        echo "Kata binaries:"
        ls -la /opt/kata/bin/containerd-shim-kata-v2 2>/dev/null || echo "NOT FOUND"

        echo ""
        echo "Kata configs:"
        ls -la /opt/kata/share/defaults/kata-containers/ 2>/dev/null || echo "NOT FOUND"

        echo ""
        echo "Containerd runtime handlers:"
        grep -A2 "kata-clh" /etc/containerd/config.toml 2>/dev/null || echo "NOT FOUND"
    '

    # Create RuntimeClass for kata-clh
    log_info "Creating RuntimeClass..."
    kubectl apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-clh
handler: kata-clh
EOF

    # Test Kata with a simple pod
    log_info "Testing Kata runtime..."

    # Check network connectivity first
    NETWORK_OK=false
    if docker exec "$NODE_NAME" bash -c 'timeout 5 wget -q --spider https://registry-1.docker.io 2>/dev/null' 2>/dev/null; then
        NETWORK_OK=true
    fi

    if [ "$NETWORK_OK" != "true" ]; then
        log_warn "Network connectivity limited. Kata VM may not be able to pull images."
        log_warn "To test Kata manually, ensure images are pre-loaded or network is available."
    fi

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-test
spec:
  runtimeClassName: kata-clh
  containers:
  - name: test
    image: alpine:latest
    command: ["uname", "-a"]
  restartPolicy: Never
EOF

    log_info "Waiting for kata-test pod (timeout: 2 minutes)..."
    if kubectl wait --for=condition=Ready pod/kata-test --timeout=120s 2>/dev/null; then
        log_info "Kata test pod output:"
        kubectl logs kata-test 2>/dev/null || log_warn "Could not get logs"

        # Check kernel version (should be different from host)
        HOST_KERNEL=$(uname -r)
        POD_KERNEL=$(kubectl logs kata-test 2>/dev/null | awk '{print $3}' || echo "unknown")
        log_info "Host kernel: $HOST_KERNEL"
        log_info "Pod kernel:  $POD_KERNEL"

        if [ "$POD_KERNEL" != "$HOST_KERNEL" ] && [ "$POD_KERNEL" != "unknown" ]; then
            log_info "✅ Kata VM isolation verified - different kernel versions!"
        else
            log_warn "Kernel comparison inconclusive"
        fi
    else
        log_warn "Kata test pod did not become ready within timeout"
        log_warn "This may be due to network connectivity issues for VM image pulling"
    fi

    # Cleanup test pod
    kubectl delete pod kata-test --ignore-not-found=true --force --grace-period=0 2>/dev/null || true
fi

# === Deploy fast-sandbox ===
log_info "Deploying fast-sandbox..."

cd "$ROOT_DIR"

# Load base image
docker pull alpine:latest
kind load docker-image alpine:latest --name "$CLUSTER_NAME"

# Build and load fast-sandbox images
log_info "Building and loading fast-sandbox images..."

# Build images
make docker-controller docker-agent docker-janitor

# Load images into kind cluster
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
if [ "$SKIP_KATA" != "true" ]; then
    echo "Kata status:"
    docker exec "${CLUSTER_NAME}-control-plane" ls -la /opt/kata/bin/containerd-shim-kata-v2 2>/dev/null || echo "  Not installed"
fi
echo ""
echo "Next steps:"
echo "  1. Create a Kata SandboxPool:"
echo "     kubectl apply -f config/samples/pool-kata.yaml"
echo "  2. Create a Sandbox:"
echo "     kubectl apply -f config/samples/sandbox-kata.yaml"
echo ""