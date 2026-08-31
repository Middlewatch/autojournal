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

# End-to-end retrieval smoke through the node bin: capture two episodes,
# search (exact and alias-rescued), open evidence, then prove
# stale_revision and typed no_match. Isolated root/index/thesaurus.
AJ=./bin/autojournal
SMOKE=$(mktemp -d)
trap 'rm -rf "$SMOKE"' EXIT
export AUTOJOURNAL_THESAURUS="$SMOKE/thesaurus.json"
export AUTOJOURNAL_MISS_LOG="$SMOKE/misses.jsonl"
AJ_ARGS=(--root "$SMOKE/root" --index "$SMOKE/index.v2.json" --world smokeworld)

payload() {
  printf '{"schema_version":1,"world":"smokeworld","scope":"global","lane":"conversation","harness":"verify","adapter_version":"0.0.0","session_id":"%s","turn_id":"%s","event_time_ms":1785240000000,"capture_policy":"default-v1","turn_outcome":"completed","user_content":"%s","assistant_result":"%s"}' "$1" "$2" "$3" "$4"
}

payload s1 t1 "the zephyr firmware needed a fwupd refresh" "Refreshed." \
  | "$AJ" capture --root "$SMOKE/root" --index "$SMOKE/index.v2.json" | grep -Eq '"outcome":"published"'
payload s2 t2 "reindexing the corpus took four seconds" "Done." \
  | "$AJ" capture --root "$SMOKE/root" --index "$SMOKE/index.v2.json" | grep -Eq '"outcome":"published"'

SEARCH_JSON=$("$AJ" search fwupd "${AJ_ARGS[@]}" --json)
printf '%s' "$SEARCH_JSON" | grep -Eq '"outcome":"match"'
EPISODE=$(printf '%s' "$SEARCH_JSON" | grep -Eo '"episode_id":"[^"]+"' | head -1 | cut -d'"' -f4)
REVISION=$(printf '%s' "$SEARCH_JSON" | grep -Eo '"revision":"[^"]+"' | head -1 | cut -d'"' -f4)

# Crediting boundaries: "index" occurs only inside "reindexing", which the
# word-start default refuses; substring mode still credits it, and a
# word-start prefix ("reindex") credits the same line.
"$AJ" search index "${AJ_ARGS[@]}" --json | grep -Eq '"outcome":"no_match"'
"$AJ" search index "${AJ_ARGS[@]}" --credit-mode substring --json | grep -Eq '"outcome":"match"'
"$AJ" search reindex "${AJ_ARGS[@]}" --json | grep -Eq '"outcome":"match"'

# Alias promotion rescues a vocabulary mismatch.
"$AJ" search hardware "${AJ_ARGS[@]}" --json | grep -Eq '"outcome":"no_match"'
"$AJ" alias add hardware fwupd >/dev/null
"$AJ" search hardware "${AJ_ARGS[@]}" --json | grep -Eq '"alias_terms":\["fwupd"\]'
"$AJ" alias remove hardware >/dev/null

# Evidence opening, then revision tracking (a stale get exits 1, so
# capture its output before asserting on it).
"$AJ" get --episode "$EPISODE" --revision "$REVISION" "${AJ_ARGS[@]}" --json | grep -Eq '"outcome":"match"'
EPISODE_FILE=$(find "$SMOKE/root" -name "$EPISODE.md")
sed -i.bak 's/Refreshed./Refreshed twice./' "$EPISODE_FILE" && rm -f "$EPISODE_FILE.bak"
STALE_JSON=$("$AJ" get --episode "$EPISODE" --revision "$REVISION" "${AJ_ARGS[@]}" --json || true)
printf '%s' "$STALE_JSON" | grep -Eq '"outcome":"stale_revision"'

# A typed empty result is a successful answer, not an error.
"$AJ" search zeppelin "${AJ_ARGS[@]}" --json | grep -Eq '"outcome":"no_match"'

printf 'e2e smoke: PASS\n'
printf 'AutoJournal repository verification: PASS\n'
