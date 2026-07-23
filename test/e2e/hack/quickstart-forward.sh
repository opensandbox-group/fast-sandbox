#!/usr/bin/env bash
set -Eeuo pipefail

namespace="${QUICKSTART_NAMESPACE:-default}"
fastpath_port="${QUICKSTART_FASTPATH_PORT:-9090}"
proxy_port="${QUICKSTART_PROXY_PORT:-18080}"
pids=()

cleanup() {
  trap - EXIT INT TERM HUP
  for pid in "${pids[@]}"; do
    kill "${pid}" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "${pid}" 2>/dev/null || true
  done
}
trap cleanup EXIT INT TERM HUP

kubectl --namespace "${namespace}" port-forward \
  service/fast-sandbox-fastpath "${fastpath_port}:9090" &
pids+=("$!")

kubectl --namespace "${namespace}" port-forward \
  service/fast-sandbox-proxy "${proxy_port}:8080" &
pids+=("$!")

echo
echo "Quick Start endpoints are being forwarded:"
echo "  Fast-Path:     localhost:${fastpath_port}"
echo "  Sandbox Proxy: http://localhost:${proxy_port}"
echo
echo "Keep this terminal open. Press Ctrl-C to stop both forwards."

while true; do
  for pid in "${pids[@]}"; do
    if ! kill -0 "${pid}" 2>/dev/null; then
      wait "${pid}"
      exit $?
    fi
  done
  sleep 1
done
