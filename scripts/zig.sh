#!/usr/bin/env bash
set -euo pipefail

EXPECTED=0.16.0
ZIG_BIN=${ZIG:-"$HOME/.local/share/zig/zig-x86_64-linux-$EXPECTED/zig"}

if [[ ! -x "$ZIG_BIN" ]]; then
  printf 'Zig %s not found at %s; set ZIG=/path/to/zig\n' \
    "$EXPECTED" "$ZIG_BIN" >&2
  exit 2
fi

ACTUAL=$($ZIG_BIN version)
if [[ "$ACTUAL" != "$EXPECTED" ]]; then
  printf 'expected Zig %s, got %s from %s\n' \
    "$EXPECTED" "$ACTUAL" "$ZIG_BIN" >&2
  exit 2
fi

exec "$ZIG_BIN" "$@"
