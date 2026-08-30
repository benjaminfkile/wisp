#!/usr/bin/env sh
# Run wisp locally for development.
#
#   1. builds the example `wisp-base` image so the default image allow-list
#      (examples/wisp.config.json) resolves to a real, pullable-free local image
#   2. starts wispd with that config
#
# Usage:
#   scripts/run-local.sh
#
# Environment overrides:
#   WISP_ADDR    listen address           (default 127.0.0.1:8090)
#   WISP_CONFIG  image allow-list + limits (default examples/wisp.config.json)
#   WISP_APP_TOKEN  app-level bearer token (default unset - open, localhost only)
#
# Requires: Docker running, Go toolchain. Once wispd is up:
#   curl -s "http://$WISP_ADDR/healthz"   # {"status":"ok"}
#   curl -s "http://$WISP_ADDR/images"    # {"images":["wisp-base"],...}
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
ADDR="${WISP_ADDR:-127.0.0.1:8090}"
CONFIG="${WISP_CONFIG:-examples/wisp.config.json}"

echo "==> building wisp-base image (docker build -t wisp-base examples/wisp-base)"
docker build -t wisp-base "$ROOT/examples/wisp-base"

echo "==> starting wispd on $ADDR (config: $CONFIG)"
cd "$ROOT"
WISP_ADDR="$ADDR" WISP_CONFIG="$CONFIG" exec go run ./cmd/wispd
