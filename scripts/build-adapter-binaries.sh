#!/usr/bin/env bash
# Cross-compiles the autojournal binary for every platform the npm package
# ships, into adapters/pi/bin/<node-platform>-<node-arch>/. Run before
# `npm pack` in adapters/pi; the adapter resolves its bundled binary from
# that layout at runtime.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

OPTIMIZE=${OPTIMIZE:-ReleaseSafe}
OUT="$ROOT/adapters/pi/bin"

# zig target -> node's process.platform-process.arch
declare -A TARGETS=(
  ["x86_64-linux-musl"]="linux-x64"
  ["aarch64-linux-musl"]="linux-arm64"
  ["x86_64-macos"]="darwin-x64"
  ["aarch64-macos"]="darwin-arm64"
)

rm -rf "$OUT"
for zig_target in "${!TARGETS[@]}"; do
  node_dir=${TARGETS[$zig_target]}
  prefix=$(mktemp -d)
  ./scripts/zig.sh build -Dtarget="$zig_target" -Doptimize="$OPTIMIZE" -Dstrip=true --prefix "$prefix"
  mkdir -p "$OUT/$node_dir"
  cp "$prefix/bin/autojournal" "$OUT/$node_dir/autojournal"
  rm -rf "$prefix"
  printf '%s -> %s (%s)\n' "$zig_target" "$OUT/$node_dir/autojournal" \
    "$(du -h "$OUT/$node_dir/autojournal" | cut -f1)"
done
