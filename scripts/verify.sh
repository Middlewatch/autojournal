#!/usr/bin/env bash
# The full repository gate: one command reproduces what CI checks. CI adds
# only what one machine cannot do — running this same script on Windows,
# and the weekly long randomized property run.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

if ! command -v node >/dev/null; then
  printf 'verify: FAIL (node is required)\n' >&2
  exit 1
fi
if [[ ! -d node_modules ]]; then
  printf 'verify: FAIL (run npm ci first)\n' >&2
  exit 1
fi

npm run --silent typecheck
npm test
printf 'test gate: PASS\n'

node scripts/smoke.mjs

printf 'AutoJournal repository verification: PASS\n'
