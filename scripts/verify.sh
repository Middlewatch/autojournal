#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

UNFORMATTED=$(gofmt -l src)
if [[ -n "$UNFORMATTED" ]]; then
  printf 'gofmt needed:\n%s\n' "$UNFORMATTED" >&2
  exit 1
fi
go vet ./...
go test -race ./...

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
# installed by the build above).
if command -v node >/dev/null && [[ -d adapters/pi/node_modules ]]; then
  (cd adapters/pi && npx tsc --noEmit && npm test --silent >/dev/null 2>&1)
  printf 'adapter gate: PASS\n'
else
  printf 'adapter gate: SKIPPED (node or adapters/pi/node_modules missing)\n' >&2
fi

DESIGN=docs/AUTOJOURNAL_1_0_DESIGN.md
for term in \
  "One completed turn is one atomic episode" \
  "Completed-turn projection" \
  'memory_search' \
  'memory_get' \
  "A future curator is a separate repository"; do
  if ! rg -Fq "$term" "$DESIGN"; then
    printf 'missing required design contract: %s\n' "$term" >&2
    exit 1
  fi
done

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
  | "$AJ" capture --root "$SMOKE/root" --index "$SMOKE/index.sqlite" | rg -q '"outcome":"published"'
payload s2 t2 "reindexing the corpus took four seconds" "Done." \
  | "$AJ" capture --root "$SMOKE/root" --index "$SMOKE/index.sqlite" | rg -q '"outcome":"published"'

SEARCH_JSON=$("$AJ" search fwupd "${AJ_ARGS[@]}" --json)
printf '%s' "$SEARCH_JSON" | rg -q '"outcome":"match"'
EPISODE=$(printf '%s' "$SEARCH_JSON" | rg -o '"episode_id":"[^"]+"' | head -1 | cut -d'"' -f4)
REVISION=$(printf '%s' "$SEARCH_JSON" | rg -o '"revision":"[^"]+"' | head -1 | cut -d'"' -f4)

# Crediting boundaries: "index" occurs only inside "reindexing", which the
# word-start default refuses; v1-parity substring mode still credits it,
# and a word-start prefix ("reindex") credits the same line.
"$AJ" search index "${AJ_ARGS[@]}" --json | rg -q '"outcome":"no_match"'
"$AJ" search index "${AJ_ARGS[@]}" --credit-mode substring --json | rg -q '"outcome":"match"'
"$AJ" search reindex "${AJ_ARGS[@]}" --json | rg -q '"outcome":"match"'

# Alias promotion rescues a vocabulary mismatch.
"$AJ" search hardware "${AJ_ARGS[@]}" --json | rg -q '"outcome":"no_match"'
"$AJ" alias add hardware fwupd >/dev/null
"$AJ" search hardware "${AJ_ARGS[@]}" --json | rg -q '"alias_terms":\["fwupd"\]'
"$AJ" alias remove hardware >/dev/null

# Evidence opening, then revision tracking (a stale get exits 1, so
# capture its output before asserting on it).
"$AJ" get --episode "$EPISODE" --revision "$REVISION" --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json \
  | rg -q '"outcome":"match"'
STALE_OUT=$("$AJ" get --episode "$EPISODE" \
  --revision sha256:0000000000000000000000000000000000000000000000000000000000000000 \
  --root "$SMOKE/root" --index "$SMOKE/index.sqlite" --json || true)
printf '%s' "$STALE_OUT" | rg -q '"outcome":"stale_revision"'

# Typed no_match is exit 0 — an answer, not an error.
"$AJ" search zzyzxplugh "${AJ_ARGS[@]}" --json | rg -q '"outcome":"no_match"'

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
printf '%s' "$ZERO_CAPTURE" | rg -q '"path":"[0-9]{4}/[0-9]{2}/[0-9]{2}/aj1-[^"]+\.md"'
ZERO_REL=$(printf '%s' "$ZERO_CAPTURE" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
DEFAULT_ROOT="$ZERO/data/autojournal/journals"
[[ -d "$DEFAULT_ROOT" ]]
[[ "$(stat -c '%a' "$DEFAULT_ROOT")" == 700 ]]
STATUS_JSON=$("${ZERO_ENV[@]}" "$AJ" status --json)
printf '%s' "$STATUS_JSON" | rg -q '"journal_root":"'"$DEFAULT_ROOT"'"'
printf '%s' "$STATUS_JSON" | rg -q '"root_source":"autojournal_default"'
printf '%s' "$STATUS_JSON" | rg -q '"episodes":1'
"${ZERO_ENV[@]}" "$AJ" catalog --json | rg -q '"world":"main","scope":"default"'
ZERO_INDEX=$(printf '%s' "$STATUS_JSON" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
[[ "$(stat -c '%a' "$ZERO_INDEX")" == 600 ]]
chmod 644 "$DEFAULT_ROOT/$ZERO_REL"
find "$DEFAULT_ROOT" -mindepth 1 -type d -exec chmod 755 {} +
"${ZERO_ENV[@]}" "$AJ" sync >/dev/null
while IFS= read -r -d '' dir; do
  [[ "$(stat -c '%a' "$dir")" == 700 ]]
done < <(find "$DEFAULT_ROOT" -type d -print0)
[[ "$(stat -c '%a' "$DEFAULT_ROOT/$ZERO_REL")" == 600 ]]
for sidecar in "$ZERO_INDEX-wal" "$ZERO_INDEX-shm" "$ZERO_INDEX-journal"; do
  [[ ! -e "$sidecar" || "$(stat -c '%a' "$sidecar")" == 600 ]]
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
printf '%s' "$MOVED_STATUS" | rg -q '"journal_root":"'"$MOVED"'"'
printf '%s' "$MOVED_STATUS" | rg -q '"root_source":"owner_config"'
"${RELOCATED_ENV[@]}" "$AJ" search relocation --world main --scope default --json |
  rg -q '"outcome":"match"'

# Vault hygiene: a manual copy of an episode dedupes (first copy stays
# served), dot-directories are invisible, and status stays fresh because
# sync's deliberate exclusions are accounted for. Removing the copy and
# rebaselining with sync clears the count.
EP_REL=$(cd "$MOVED" && find . -name 'aj1-*.md' -type f | head -1 | sed 's|^\./||')
mkdir -p "$MOVED/backup" "$MOVED/.obsidian"
cp "$MOVED/$EP_REL" "$MOVED/backup/"
cp "$MOVED/$EP_REL" "$MOVED/.obsidian/"
"${RELOCATED_ENV[@]}" "$AJ" sync | rg -q 'duplicate_ids: 1'
"${RELOCATED_ENV[@]}" "$AJ" status --json | rg -q '"freshness":"fresh"'
"${RELOCATED_ENV[@]}" "$AJ" search relocation --world main --scope default --json |
  rg -q '"outcome":"match".*"freshness":"fresh"|"freshness":"fresh".*"outcome":"match"'
rm -r "$MOVED/backup" "$MOVED/.obsidian"
"${RELOCATED_ENV[@]}" "$AJ" sync | rg -q 'duplicate_ids: 0'

# Owner defaults: `default` shows and sets the capture/search defaults with
# an atomic config rewrite that preserves the journal root. A payload that
# names no world then lands in the saved default, and no-world search
# follows it too.
"${RELOCATED_ENV[@]}" "$AJ" default --json | rg -q '"world":"main","scope":"default"'
"${RELOCATED_ENV[@]}" "$AJ" default --world team >/dev/null
"${RELOCATED_ENV[@]}" "$AJ" default --json | rg -q '"world":"team","scope":"default"'
rg -q '"journal_root": "'"$MOVED"'"' "$ZERO/config/autojournal/config.json"
"${RELOCATED_ENV[@]}" "$AJ" catalog --json | rg -q '"world":"team"'
TEAM_PAYLOAD='{"schema_version":1,"lane":"conversation","harness":"verify","adapter_version":"0.0.0","session_id":"fresh","turn_id":"team-default","event_time_ms":1785240000000,"capture_policy":"default-v1","turn_outcome":"completed","user_content":"team sentinel biscuit","assistant_result":"captured"}'
printf '%s' "$TEAM_PAYLOAD" | "${RELOCATED_ENV[@]}" "$AJ" capture | rg -q '"path":"worlds/team/'
"${RELOCATED_ENV[@]}" "$AJ" search biscuit --json | rg -q '"outcome":"match"'

# Shared-directory refusal: a journal root whose parent is world-writable
# is refused for capture and sync, with guidance.
SHARED="$SMOKE/shared"
mkdir -p "$SHARED"
chmod 777 "$SHARED"
SHARED_OUT=$(printf '%s' "$ZERO_PAYLOAD" |
  "$AJ" capture --root "$SHARED/journals" --index "$SMOKE/shared-index.sqlite" || true)
printf '%s' "$SHARED_OUT" | rg -q 'shared \(group- or world-writable\)'
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
[[ "$(stat -c '%a' "$EMPTY_XDG/home/.local/share/autojournal/journals")" == 700 ]]

printf 'AutoJournal repository verification: PASS\n'
