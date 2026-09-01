#!/usr/bin/env bash
# Release gate: the full repository gate plus the packaging self-check
# (version stamp asserted in package.json, index.ts, and cli.ts; dated
# changelog entry; npm pack layout). Fails by design while the changelog
# entry is undated — that failure is the reminder to cut the release, not
# a broken gate.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

./scripts/verify.sh
pack_json=$(mktemp)
trap 'rm -f "$pack_json"' EXIT
npm pack --dry-run --json --ignore-scripts > "$pack_json"
node scripts/check-package.mjs --pack-json "$pack_json"
printf 'release check: PASS\n'
