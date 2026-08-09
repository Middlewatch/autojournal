#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PACKAGE="$ROOT/adapters/pi"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# `npm publish --dry-run` exports npm_config_dry_run=true to lifecycle
# scripts. The outer publication must stay dry, but this nested pack has to
# materialize an archive for the layout and executable-bit checks below.
(cd "$PACKAGE" && npm_config_dry_run=false npm pack --pack-destination "$TMP" >/dev/null)
TARBALL=$(find "$TMP" -maxdepth 1 -type f -name 'autojournal-*.tgz' -print -quit)
[[ -n "$TARBALL" && -f "$TARBALL" ]]

paths=(
  package/index.ts
  package/README.md
  package/LICENSE
  package/scripts/check-package.mjs
  package/bin/linux-x64/autojournal
  package/bin/linux-arm64/autojournal
  package/bin/darwin-x64/autojournal
  package/bin/darwin-arm64/autojournal
)
for path in "${paths[@]}"; do
  tar -tzf "$TARBALL" "$path" >/dev/null
done

for path in package/bin/{linux-x64,linux-arm64,darwin-x64,darwin-arm64}/autojournal; do
  mode=$(tar -tvzf "$TARBALL" "$path" | awk '{print $1}')
  [[ "$mode" == *x* ]] || {
    printf 'packed binary is not executable: %s (%s)\n' "$path" "$mode" >&2
    exit 1
  }
done

file "$PACKAGE/bin/linux-x64/autojournal" | grep -qE 'ELF 64-bit.*x86-64'
file "$PACKAGE/bin/linux-arm64/autojournal" | grep -qE 'ELF 64-bit.*ARM aarch64'
file "$PACKAGE/bin/darwin-x64/autojournal" | grep -qE 'Mach-O 64-bit.*x86_64'
file "$PACKAGE/bin/darwin-arm64/autojournal" | grep -qE 'Mach-O 64-bit.*arm64'

tar -xzf "$TARBALL" -C "$TMP"
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) native=linux-x64 ;;
  Linux-aarch64 | Linux-arm64) native=linux-arm64 ;;
  Darwin-x86_64) native=darwin-x64 ;;
  Darwin-arm64) native=darwin-arm64 ;;
  *)
    printf 'unsupported release-verification host: %s-%s\n' "$(uname -s)" "$(uname -m)" >&2
    exit 1
    ;;
esac
manifest_version=$(node -p 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).version' "$TMP/package/package.json")
version=$("$TMP/package/bin/$native/autojournal" version)
[[ "$version" == "autojournal $manifest_version "* ]]

printf 'AutoJournal npm package verification: PASS\n'
