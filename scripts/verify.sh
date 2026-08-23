#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

file_mode() {
  case "$(uname -s)" in
    Darwin) stat -f '%Lp' "$1" ;;
    *) stat -c '%a' "$1" ;;
  esac
}

UNFORMATTED=$(gofmt -l src)
if [[ -n "$UNFORMATTED" ]]; then
  printf 'gofmt needed:\n%s\n' "$UNFORMATTED" >&2
  exit 1
fi
go vet ./...
go test -race ./...

# Parse-boundary fuzz targets, bounded so green stays a fixed-cost command
# — an unbounded fuzz in the gate is how gates get skipped. The
# weekly CI job runs the same five targets for ten minutes each.
FUZZ_LOG=$(mktemp)
for FUZZ_TARGET in FuzzParsePayload FuzzParseConfig FuzzParseEpisode \
  FuzzLoadAliasMapFromBytes FuzzCursorDecode; do
  printf 'fuzz %s (10s)\n' "$FUZZ_TARGET"
  if ! go test -count=1 -run "^${FUZZ_TARGET}\$" -fuzz "^${FUZZ_TARGET}\$" -fuzztime=10s ./src/ > "$FUZZ_LOG" 2>&1; then
    cat "$FUZZ_LOG" >&2
    rm -f "$FUZZ_LOG"
    exit 1
  fi
done
rm -f "$FUZZ_LOG"

# Host binary for the smoke and adapter tests, in the same layout the npm
# package ships (adapters/pi/bin/<platform>-<arch>/).
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) HOST_DIR=linux-x64 ;;
  Linux-aarch64 | Linux-arm64) HOST_DIR=linux-arm64 ;;
  Darwin-x86_64) HOST_DIR=darwin-x64 ;;
  Darwin-arm64) HOST_DIR=darwin-arm64 ;;
  *)
    printf 'unsupported verification host: %s-%s\n' "$(uname -s)" "$(uname -m)" >&2
    exit 1
    ;;
esac
AJ="adapters/pi/bin/$HOST_DIR/autojournal"
mkdir -p "$(dirname "$AJ")"
CGO_ENABLED=0 go build -trimpath -o "$AJ" ./src/cmd/autojournal

# Pi adapter gate: typecheck + tests (the e2e case runs against the binary
# installed by the build above). A complete gate must not silently pass
# without its adapter dependencies.
if ! command -v node >/dev/null; then
  printf 'adapter gate: FAIL (node is required)\n' >&2
  exit 1
fi
if [[ ! -d adapters/pi/node_modules ]]; then
  printf 'adapter gate: FAIL (run npm ci in adapters/pi first)\n' >&2
  exit 1
fi
(cd adapters/pi && npm run typecheck && npm test)
printf 'adapter gate: PASS\n'

# Python hook gate: the Claude Code and Codex hooks run against a fake
# binary, so they never touch a real journal.
if ! command -v python3 >/dev/null; then
  printf 'python hook gate: FAIL (python3 is required)\n' >&2
  exit 1
fi
python3 adapters/test_python_hooks.py
printf 'python hook gate: PASS\n'

# Cross-adapter conformance: the token, workspace-root and origin-host rules
# decide identically in every adapter implementing them. Runs the Pi
# implementation through node --experimental-strip-types, so it sits after
# the adapter gate above, which guarantees node and adapters/pi/node_modules.
python3 adapters/test_conformance.py
printf 'conformance gate: PASS\n'

# End-to-end retrieval smoke against the installed binary: capture two
# episodes, search (exact and alias-rescued), open evidence, then prove
# stale_revision and typed no_match. Isolated root/index/thesaurus.
SMOKE=$(mktemp -d)
trap 'rm -rf "$SMOKE"' EXIT
export AUTOJOURNAL_THESAURUS="$SMOKE/thesaurus.json"
export AUTOJOURNAL_MISS_LOG="$SMOKE/misses.jsonl"
AJ_ARGS=(--root "$SMOKE/root" --index "$SMOKE/index.sqlite" --world smokeworld)

payload() {
  printf '{"schema_version":1,"world":"smokeworld","scope":"global","lane":"conversation","harness":"verify","adapter_version":"0.0.0","session_id":"%s","turn_id":"%s","event_time_ms":1785240000000,"capture_policy":"default-v1","turn_outcome":"completed","user_content":"%s","assistant_result":"%s"}' "$1" "$2" "$3" "$4"
}

payload s1 t1 "the zephyr firmware needed a fwupd refresh" "Refreshed." \
  | "$AJ" capture --root "$SMOKE/root" --index "$SMOKE/index.sqlite" | grep -Eq '"outcome":"published"'
payload s2 t2 "reindexing the corpus took four seconds" "Done." \
  | "$AJ" capture --root "$SMOKE/root" --index "$SMOKE/index.sqlite" | grep -Eq '"outcome":"published"'

SEARCH_JSON=$("$AJ" search fwupd "${AJ_ARGS[@]}" --json)
printf '%s' "$SEARCH_JSON" | grep -Eq '"outcome":"match"'
EPISODE=$(printf '%s' "$SEARCH_JSON" | grep -Eo '"episode_id":"[^"]+"' | head -1 | cut -d'"' -f4)
REVISION=$(printf '%s' "$SEARCH_JSON" | grep -Eo '"revision":"[^"]+"' | head -1 | cut -d'"' -f4)

# Crediting boundaries: "index" occurs only inside "reindexing", which the
# word-start default refuses; substring mode still credits it,
# and a word-start prefix ("reindex") credits the same line.
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
"$AJ" get --episode "$EPISODE" --revision "$REVISION" --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json \
  | grep -Eq '"outcome":"match"'
STALE_OUT=$("$AJ" get --episode "$EPISODE" \
  --revision sha256:0000000000000000000000000000000000000000000000000000000000000000 \
  --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json || true)
printf '%s' "$STALE_OUT" | grep -Eq '"outcome":"stale_revision"'

# Typed no_match is exit 0 — an answer, not an error.
"$AJ" search zzyzxplugh "${AJ_ARGS[@]}" --json | grep -Eq '"outcome":"no_match"'

# One freshness signal: status and search may not disagree about the
# same corpus. Delete the projection — search first, since opening it is
# what recreates the empty database both then describe — and the two
# reports must carry the same freshness string; one sync brings both to
# fresh. Search and status over a stale projection both exit non-zero by
# design, hence the guards.
rm -f "$SMOKE/index.sqlite" "$SMOKE/index.sqlite-wal" "$SMOKE/index.sqlite-shm"
SEARCH_FRESHNESS=$("$AJ" search fwupd "${AJ_ARGS[@]}" --json | grep -Eo '"freshness":"[^"]+"' | head -1 || true)
STATUS_FRESHNESS=$("$AJ" status --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json | grep -Eo '"freshness":"[^"]+"' | head -1 || true)
[[ -n "$SEARCH_FRESHNESS" && "$SEARCH_FRESHNESS" == "$STATUS_FRESHNESS" ]]
"$AJ" sync --root "$SMOKE/root" --index "$SMOKE/index.sqlite" >/dev/null
"$AJ" search fwupd "${AJ_ARGS[@]}" --json | grep -Eq '"freshness":"fresh"'
"$AJ" status --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json | grep -Eq '"freshness":"fresh"'

# The disagreement case: an in-place edit leaves file and row counts equal,
# which is exactly the state where a count-based reporter would still say
# fresh while the authoritative check says stale. Both must say stale, and
# the same string, before a sync repairs both to fresh.
SMOKE_EPISODE_FILE=$(find "$SMOKE/root" -name 'aj1-*.md' -type f -exec grep -El 'Refreshed\.' {} + | head -1)
sed -i 's/Refreshed\./Refreshed twice./' "$SMOKE_EPISODE_FILE"
SEARCH_FRESHNESS=$("$AJ" search fwupd "${AJ_ARGS[@]}" --json | grep -Eo '"freshness":"[^"]+"' | head -1 || true)
STATUS_FRESHNESS=$("$AJ" status --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json | grep -Eo '"freshness":"[^"]+"' | head -1 || true)
[[ "$STATUS_FRESHNESS" == '"freshness":"stale"' ]]
[[ "$SEARCH_FRESHNESS" == "$STATUS_FRESHNESS" ]]
"$AJ" sync --root "$SMOKE/root" --index "$SMOKE/index.sqlite" >/dev/null
"$AJ" search fwupd "${AJ_ARGS[@]}" --json | grep -Eq '"freshness":"fresh"'
"$AJ" status --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json | grep -Eq '"freshness":"fresh"'

# Fresh-install and relocation contract: no owner config resolves the
# host-neutral XDG data journal, default classifications produce date-only
# paths, status exposes root provenance, and moving the corpus plus owner
# config + sync preserves recall.
ZERO="$SMOKE/zero"
mkdir -p "$ZERO/home" "$ZERO/data" "$ZERO/config" "$ZERO/state"
if env HOME="$ZERO/home" AUTOJOURNAL_CONFIG="$ZERO/does-not-exist.json" "$AJ" status --json >/dev/null 2>&1; then
  printf 'explicit missing config unexpectedly fell back to defaults\n' >&2
  exit 1
fi
ZERO_ENV=(
  env
  HOME="$ZERO/home"
  XDG_DATA_HOME="$ZERO/data"
  XDG_CONFIG_HOME="$ZERO/config"
  XDG_STATE_HOME="$ZERO/state"
  AUTOJOURNAL_THESAURUS="$SMOKE/thesaurus.json"
  AUTOJOURNAL_MISS_LOG="$ZERO/state/misses.jsonl"
)
ZERO_PAYLOAD='{"schema_version":1,"world":"main","scope":"default","lane":"conversation","harness":"verify","adapter_version":"0.0.0","session_id":"fresh","turn_id":"first","event_time_ms":1785240000000,"capture_policy":"default-v1","turn_outcome":"completed","user_content":"relocation sentinel phrase","assistant_result":"captured"}'
mkdir -p "$ZERO/data/autojournal/journals"
chmod 755 "$ZERO/data/autojournal/journals"
ZERO_CAPTURE=$(printf '%s' "$ZERO_PAYLOAD" | "${ZERO_ENV[@]}" "$AJ" capture)
printf '%s' "$ZERO_CAPTURE" | grep -Eq '"path":"[0-9]{4}/[0-9]{2}/[0-9]{2}/aj1-[^"]+\.md"'
ZERO_REL=$(printf '%s' "$ZERO_CAPTURE" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
DEFAULT_ROOT="$ZERO/data/autojournal/journals"
[[ -d "$DEFAULT_ROOT" ]]
[[ "$(file_mode "$DEFAULT_ROOT")" == 700 ]]
STATUS_JSON=$("${ZERO_ENV[@]}" "$AJ" status --json)
printf '%s' "$STATUS_JSON" | grep -Eq '"journal_root":"'"$DEFAULT_ROOT"'"'
printf '%s' "$STATUS_JSON" | grep -Eq '"root_source":"autojournal_default"'
printf '%s' "$STATUS_JSON" | grep -Eq '"episodes":1'
"${ZERO_ENV[@]}" "$AJ" catalog --json | grep -Eq '"world":"main","scope":"default"'
ZERO_INDEX=$(printf '%s' "$STATUS_JSON" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
[[ "$(file_mode "$ZERO_INDEX")" == 600 ]]
chmod 644 "$DEFAULT_ROOT/$ZERO_REL"
find "$DEFAULT_ROOT" -mindepth 1 -type d -exec chmod 755 {} +
"${ZERO_ENV[@]}" "$AJ" sync >/dev/null
while IFS= read -r -d '' dir; do
  [[ "$(file_mode "$dir")" == 700 ]]
done < <(find "$DEFAULT_ROOT" -type d -print0)
[[ "$(file_mode "$DEFAULT_ROOT/$ZERO_REL")" == 600 ]]
for sidecar in "$ZERO_INDEX-wal" "$ZERO_INDEX-shm" "$ZERO_INDEX-journal"; do
  [[ ! -e "$sidecar" || "$(file_mode "$sidecar")" == 600 ]]
done

MOVED="$ZERO/moved-journal"
mv "$DEFAULT_ROOT" "$MOVED"
mkdir -p "$ZERO/config/autojournal"
printf '{"journal_root":"%s"}\n' "$MOVED" >"$ZERO/config/autojournal/config.json"
RELOCATED_ENV=(
  env
  HOME="$ZERO/home"
  XDG_DATA_HOME="$ZERO/data"
  XDG_CONFIG_HOME="$ZERO/config"
  XDG_STATE_HOME="$ZERO/state"
  AUTOJOURNAL_THESAURUS="$SMOKE/thesaurus.json"
  AUTOJOURNAL_MISS_LOG="$ZERO/state/misses.jsonl"
)
"${RELOCATED_ENV[@]}" "$AJ" sync >/dev/null
MOVED_STATUS=$("${RELOCATED_ENV[@]}" "$AJ" status --json)
printf '%s' "$MOVED_STATUS" | grep -Eq '"journal_root":"'"$MOVED"'"'
printf '%s' "$MOVED_STATUS" | grep -Eq '"root_source":"owner_config"'
"${RELOCATED_ENV[@]}" "$AJ" search relocation --world main --scope default --json |
  grep -Eq '"outcome":"match"'

# Vault hygiene: a manual copy of an episode dedupes (first copy stays
# served), dot-directories are invisible, and status stays fresh because
# sync's deliberate exclusions are accounted for. Removing the copy and
# rebaselining with sync clears the count.
EP_REL=$(cd "$MOVED" && find . -name 'aj1-*.md' -type f | head -1 | sed 's|^\./||')
mkdir -p "$MOVED/backup" "$MOVED/.obsidian"
cp "$MOVED/$EP_REL" "$MOVED/backup/"
cp "$MOVED/$EP_REL" "$MOVED/.obsidian/"
"${RELOCATED_ENV[@]}" "$AJ" sync | grep -Eq 'duplicate_ids: 1'
"${RELOCATED_ENV[@]}" "$AJ" status --json | grep -Eq '"freshness":"fresh"'
"${RELOCATED_ENV[@]}" "$AJ" search relocation --world main --scope default --json |
  grep -Eq '"outcome":"match".*"freshness":"fresh"|"freshness":"fresh".*"outcome":"match"'
rm -r "$MOVED/backup" "$MOVED/.obsidian"
"${RELOCATED_ENV[@]}" "$AJ" sync | grep -Eq 'duplicate_ids: 0'

# Owner defaults: `default` shows and sets the capture/search defaults with
# an atomic config rewrite that preserves the journal root. A payload that
# names no world then lands in the saved default, and no-world search
# follows it too.
"${RELOCATED_ENV[@]}" "$AJ" default --json | grep -Eq '"world":"main","scope":"default"'
"${RELOCATED_ENV[@]}" "$AJ" default --world team >/dev/null
"${RELOCATED_ENV[@]}" "$AJ" default --json | grep -Eq '"world":"team","scope":"default"'
grep -Eq '"journal_root": "'"$MOVED"'"' "$ZERO/config/autojournal/config.json"
"${RELOCATED_ENV[@]}" "$AJ" catalog --json | grep -Eq '"world":"team"'
TEAM_PAYLOAD='{"schema_version":1,"lane":"conversation","harness":"verify","adapter_version":"0.0.0","session_id":"fresh","turn_id":"team-default","event_time_ms":1785240000000,"capture_policy":"default-v1","turn_outcome":"completed","user_content":"team sentinel biscuit","assistant_result":"captured"}'
printf '%s' "$TEAM_PAYLOAD" | "${RELOCATED_ENV[@]}" "$AJ" capture | grep -Eq '"path":"worlds/team/'
"${RELOCATED_ENV[@]}" "$AJ" search biscuit --json | grep -Eq '"outcome":"match"'

# Shared-directory refusal: a journal root whose parent is world-writable
# is refused for capture and sync, with guidance.
SHARED="$SMOKE/shared"
mkdir -p "$SHARED"
chmod 777 "$SHARED"
SHARED_OUT=$(printf '%s' "$ZERO_PAYLOAD" |
  "$AJ" capture --root "$SHARED/journals" --index "$SMOKE/shared-index.sqlite" || true)
printf '%s' "$SHARED_OUT" | grep -Eq 'shared \(group- or world-writable\)'
[[ ! -e "$SHARED/journals" ]]
if "$AJ" sync --root "$SHARED/journals" --index "$SMOKE/shared-index.sqlite" >/dev/null 2>&1; then
  printf 'sync in a shared directory unexpectedly succeeded\n' >&2
  exit 1
fi
GROUPW="$SMOKE/groupw"
mkdir -p "$GROUPW"
chmod 770 "$GROUPW"
if printf '%s' "$ZERO_PAYLOAD" |
  "$AJ" capture --root "$GROUPW/journals" --index "$SMOKE/groupw-index.sqlite" >/dev/null 2>&1; then
  printf 'capture under a group-writable directory unexpectedly succeeded\n' >&2
  exit 1
fi
[[ ! -e "$GROUPW/journals" ]]

EMPTY_XDG="$SMOKE/empty-xdg"
mkdir -p "$EMPTY_XDG/home"
EMPTY_PAYLOAD=${ZERO_PAYLOAD/"turn_id":"first"/"turn_id":"empty-xdg"}
printf '%s' "$EMPTY_PAYLOAD" |
  env HOME="$EMPTY_XDG/home" XDG_DATA_HOME= XDG_CONFIG_HOME= XDG_STATE_HOME= "$AJ" capture >/dev/null
[[ -d "$EMPTY_XDG/home/.local/share/autojournal/journals" ]]
[[ "$(file_mode "$EMPTY_XDG/home/.local/share/autojournal/journals")" == 700 ]]

# Hand-edit case: evidence is verified against content. In its own
# isolated root under $SMOKE (the existing EXIT trap cleans it up), capture
# one episode, edit its body with the payload_digest line untouched, then
# assert search excludes it, get reports stale_revision, and sync counts it.
EDITCASE="$SMOKE/editcase"
mkdir -p "$EDITCASE"
EDIT_ARGS=(--root "$EDITCASE/root" --index "$EDITCASE/index.sqlite" --world smokeworld)
payload s9 t9 "the heliotrope ledger was reconciled" "Reconciled." \
  | "$AJ" capture --root "$EDITCASE/root" --index "$EDITCASE/index.sqlite" | grep -Eq '"outcome":"published"'
EDIT_SEARCH=$("$AJ" search heliotrope "${EDIT_ARGS[@]}" --json)
printf '%s' "$EDIT_SEARCH" | grep -Eq '"outcome":"match"'
EDIT_REVISION=$(printf '%s' "$EDIT_SEARCH" | grep -Eo '"revision":"[^"]+"' | head -1 | cut -d'"' -f4)
EDIT_EPISODE=$(printf '%s' "$EDIT_SEARCH" | grep -Eo '"episode_id":"[^"]+"' | head -1 | cut -d'"' -f4)
EDIT_FILE=$(find "$EDITCASE/root" -name 'aj1-*.md' | head -1)
sed -i 's/Reconciled./Reconciled by hand./' "$EDIT_FILE"
EDIT_AFTER=$("$AJ" search heliotrope "${EDIT_ARGS[@]}" --json || true)
if printf '%s' "$EDIT_AFTER" | grep -Eq "$EDIT_EPISODE"; then
  printf 'digest-stale episode still served by search\n' >&2
  exit 1
fi
printf '%s' "$EDIT_AFTER" | grep -Eq '"edited_excluded":[1-9]'
EDIT_GET=$("$AJ" get --episode "$EDIT_EPISODE" --revision "$EDIT_REVISION" \
  --root "$EDITCASE/root" --index "$EDITCASE/index.sqlite" --json || true)
printf '%s' "$EDIT_GET" | grep -Eq '"outcome":"stale_revision"'
"$AJ" sync --root "$EDITCASE/root" --index "$EDITCASE/index.sqlite" --json | grep -Eq '"digest_mismatch":1'

# Reseal closes the round trip: the owner's edit is re-attested in
# place, search serves the episode again, and the mismatch count returns
# to zero — the digest-stale episode is resolved in the root that made it.
"$AJ" reseal --root "$EDITCASE/root" --index "$EDITCASE/index.sqlite" --json | grep -Eq '"resealed":1'
"$AJ" search heliotrope "${EDIT_ARGS[@]}" --json | grep -Eq "$EDIT_EPISODE"
"$AJ" sync --root "$EDITCASE/root" --index "$EDITCASE/index.sqlite" --json | grep -Eq '"digest_mismatch":0'

# Supersede on proven containment: in its own isolated root, a strict
# extension of a settled turn replaces in place with outcome superseded and
# exit 0 and the file carries the fuller body; a divergent redelivery is a
# conflict with exit 3 and the first publication survives.
SUPERSEDE="$SMOKE/supersede"
mkdir -p "$SUPERSEDE"
SUP_ARGS=(--root "$SUPERSEDE/root" --index "$SUPERSEDE/index.sqlite")
payload s7 t7 "the falcon deploy settled" "First half." \
  | "$AJ" capture "${SUP_ARGS[@]}" | grep -Eq '"outcome":"published"'
payload s7 t7 "the falcon deploy settled" 'First half.\n\nSecond half arrived.' \
  | "$AJ" capture "${SUP_ARGS[@]}" | grep -Eq '"outcome":"superseded"'
SUP_FILE=$(find "$SUPERSEDE/root" -name 'aj1-*.md' | head -1)
grep -Eq "Second half arrived." "$SUP_FILE"
set +e
payload s7 t7 "the falcon deploy settled" "A divergent rewrite." \
  | "$AJ" capture "${SUP_ARGS[@]}" >"$SUPERSEDE/conflict.json"
SUP_CODE=$?
set -e
[[ "$SUP_CODE" == 3 ]]
grep -Eq '"outcome":"conflict"' "$SUPERSEDE/conflict.json"
grep -Eq "Second half arrived." "$SUP_FILE"

printf 'AutoJournal repository verification: PASS\n'
