#!/usr/bin/env bash
# verify-checkpoint-chain-growth.sh — run the packRootfs chain simulation in
# a container and print the per-cycle chain length.
#
# Usage: scripts/verify-checkpoint-chain-growth.sh [cycles] [max-layers] [squash-layers]
set -eu
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_IMAGE="${GO_IMAGE:-golang:1.25}"
CYCLES="${1:-30}"
MAX_LAYERS="${2:-8}"
SQUASH="${3:-4}"

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }

echo "== simulating $CYCLES pause cycles (max-layers=$MAX_LAYERS squash-layers=$SQUASH) =="
exec docker run --rm \
  -v "$REPO":/src -v "${HOME}/go/pkg/mod":/go/pkg/mod \
  -w /src \
  -e GOCACHE=/tmp/gocache -e GOFLAGS=-buildvcs=false \
  -e FSB_CHAIN_CYCLES="$CYCLES" \
  -e FSB_CHAIN_MAX_LAYERS="$MAX_LAYERS" \
  -e FSB_CHAIN_SQUASH_LAYERS="$SQUASH" \
  "$GO_IMAGE" \
  go test ./cmd/fsb-vm/ -v -run TestPackRootfsSquashBoundsChainGrowth
