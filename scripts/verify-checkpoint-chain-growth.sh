#!/usr/bin/env bash
# verify-checkpoint-chain-growth.sh — prove that repeated pause/snapshot
# cycles no longer grow the rootfs layer chain without bound.
#
# Runs the packRootfs chain simulation (TestPackRootfsSquashBoundsChainGrowth)
# in a Linux container: every cycle plays one pause — a dirty layer is
# committed on top of the previous cycle's published stack — and the squash
# policy collapses the configured tail once the chain reaches the cap. The
# per-cycle chain length is printed so the bounded oscillation is visible.
#
# Usage:
#   scripts/verify-checkpoint-chain-growth.sh [cycles] [max-layers] [squash-layers]
#
# Environment:
#   GO_IMAGE   container image used to run the test (default golang:1.25)
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
