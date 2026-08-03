#!/usr/bin/env bash
# Cross-compiles the autojournal binary for every platform the npm package
# ships, into adapters/pi/bin/<node-platform>-<node-arch>/. Run before
# `npm pack` in adapters/pi; the adapter resolves its bundled binary from
# that layout at runtime. Pure-Go SQLite means CGO stays off and every
# target is a static single file.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

OUT="$ROOT/adapters/pi/bin"

# GOOS/GOARCH -> node's process.platform-process.arch
TARGETS=(
  "linux amd64 linux-x64"
  "linux arm64 linux-arm64"
  "darwin amd64 darwin-x64"
  "darwin arm64 darwin-arm64"
)

rm -rf "$OUT"
for target in "${TARGETS[@]}"; do
  read -r goos goarch node_dir <<<"$target"
  mkdir -p "$OUT/$node_dir"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags='-s -w' \
    -o "$OUT/$node_dir/autojournal" ./src/cmd/autojournal
  printf '%s/%s -> %s (%s)\n' "$goos" "$goarch" "$OUT/$node_dir/autojournal" \
    "$(du -h "$OUT/$node_dir/autojournal" | cut -f1)"
done
