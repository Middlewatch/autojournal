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

# Typecheck plus the whole suite: golden byte pins, conformance cases,
# parse-boundary properties over the fuzz seeds, store/index/retrieval
# behavior, the CLI wire shapes, and the extension in-process.
npm run --silent typecheck
npm test
printf 'test gate: PASS\n'

# End-to-end retrieval smoke through the node bin, in a portable node
# script so the same e2e runs on every CI platform.
node scripts/smoke.mjs

printf 'AutoJournal repository verification: PASS\n'
