# Changelog

Versions are the npm package (`autojournal`) and the bundled core binary,
which share one version stamp. Stability is tiered, and `DESIGN.md` states the
tiers authoritatively: the corpus-durable contracts — episode Markdown and
frontmatter bytes, episode-id derivation, and payload-digest derivation — are
frozen within 1.x, and a change to any of them would be a major version.
Interface surfaces (the CLI `--json` output and the config file) accept
additive changes in a minor release, so consumers must tolerate unknown fields
and unknown values of a typed vocabulary. The SQLite projection is derived
state, not a contract.

`adapter_version` is recorded in episode frontmatter but is deliberately
excluded from the payload digest, so upgrading never re-identifies or
re-publishes existing episodes.

## 1.1.0 — 2026-08-10

### Added

- `scripts/repair-corpus.py`: a report-only owner script that replays
`cc-stop.v3` episodes through the v4 projection, reanchoring turns the v3 hook
started on machine input to the owner turn they belong to. With `--apply` it
publishes each replacement through `autojournal capture` and deletes only the
v3 file whose replacement published.
- `sync --json`: the sync report in machine-readable form.
- `AUTOJOURNAL_NOW_MS`: pins the CLI clock to a decimal millisecond timestamp
for reproducible ranking-parity runs. Unset, empty, or malformed values are
ignored.
- Vocabulary discovery iterates in sorted term order, so the `MaxVocabMatches`
cap truncates a stable, defined prefix rather than whatever subset an undefined
scan order happened to reach first.
- Trigram-backed vocabulary discovery: index schema 2 → 3 adds a `term_trigrams`
table over vocabulary terms, and trigram-eligible queries narrow their
candidates through it before the exact containment verification. The candidate
set is identical to the linear scan's; wholly-short queries still take the
linear scan, preserving curated short-alias reachability. The schema bump
disposes and rebuilds existing indexes on first open — one `sync`.
- Search reads each credited episode once per query, so a snippet always shows
the revision its hit was credited against; a file edited between ranking and
rendering previously produced an empty snippet.
- `digest_mismatch` files that parse as episodes but whose recorded digest
disagrees with their content. They stay indexed, but are excluded from recall.
`reseal` resolves this issue.
- `unreadable` subtrees the corpus walk could not enter. Sync still succeeds,
and the count joins the exclusion arithmetic so freshness never reports `fresh`
over content nobody can see.
- `superseded`: A redelivery of an episode's own identity whose assistant result
strictly extends the stored one — every other digest-covered field identical —
replaces the episode in place with the fuller content; anything else remains a
`conflict` and the first publication survives.
- The Claude Code Stop hook projects turns by classifying transcript entries
(`cc-stop.v4`): a slash command journals the argument the owner typed — a bare
one journals the command name — rather than the expanded command body; a
background-agent notification no longer splits a turn; bodies accumulate every
terminal response since the owner's prompt, in order. An envelope is recognized
by the head of the entry text, so a prompt that merely quotes the envelope tags
is journaled as the typed sentence.
- `event_time_ms` outside 1970 through 9999-12-31T23:59:59Z is refused as
`malformed` (`ImplausibleEventTime`): a garbage timestamp would shard the
episode into a nonsense date directory. Unknown lanes are refused by the library
itself
- The journal root is canonicalized (`filepath.Clean`) before the index identity
derives from it, so two spellings of one root share one index. Migration: a
configured root ending in a trailing slash gets a new index filename and one
extra sync rebuilds it; the corpus is untouched. A set-but-empty `HOME` now
fails loudly (`ErrMissingHome`) instead of resolving paths under `/`.
- `reseal`: re-attest owner-edited episodes. A digest-stale file gets its
`payload_digest` line rewritten to the first valid reading of its edited body,
atomically in place; a file that no longer parses is refused and left untouched;
one sync afterwards rebaselines the projection. `--preview` lists without
writing; `--json` emits `{scanned, resealed, refused, write_failures, paths}`,
and write failures exit 1 after the sweep completes. The Pi `/autojournal` menu
gains `Reseal edited episodes`.
- `folded_terms` in the search report: the additive singular variants search
folded in, reported beside `alias_terms`.
- `alias list --json` gains `merged_keys`: duplicate and case-variant alias keys
now merge on load instead of disabling the whole file. A file that had
duplicates gets a new alias digest on first load; outstanding search cursors
degrade to a typed `malformed`.

### Changed

- Evidence is verified against content before it is served: a body edit that
leaves the recorded `payload_digest` line untouched is excluded from search
(`edited_excluded`), and `get` against it reports `stale_revision` with a detail
naming reseal.
- A numeric config value that parses to a non-finite float (`1e999`) is refused
as malformed; a 1.0.x config carrying one is rejected until corrected.
- `--limit 0` now resolves to the default page size; previously it was clamped
to one result.
- A JSON encode failure now exits non-zero with zero bytes of stdout instead of
a silent success exit.
- A hand-edited episode whose scope no longer satisfies the scope rule is
excluded from reads as `skipped_malformed` instead of being served.
- A float-shaped unsigned config value (`1.5e7`) is accepted only when it is
exactly representable in float64: higher-precision literals were previously
accepted, then re-emitted rounded by the next config rewrite.
- A search cursor with a non-canonical offset spelling (`aj1.07.…`) is now a
typed `malformed`: only the spelling this package mints decodes.
- A hand-edited episode with a duplicated required frontmatter key (two
`payload_digest` lines) no longer parses — readers could disagree about which
line binds. Such files are `skipped_malformed` on sync and refused by reseal;
delete the extra line by hand to recover them.
- The scorer is `aj-scorer.v3`: a repeated query word keeps one weight per
repetition unconditionally. Previously any alias value or folded variant joining
the search replaced the term list with a deduplicated union, so repetition
weight depended on whether an unrelated thesaurus entry fired. Ranking can
move for repeated-term queries with an active alias or fold; the invariant is
documented in `docs/SEARCH_TUNING.md` and pinned by the `testdata/ranking`
fixture. The scorer version is part of the cursor guard, so cursors outstanding
across the upgrade decode as a typed `malformed`.
- `search` derives its reported freshness from the same verified signal `status`
uses instead of comparing file and row counts, so the two reporters can no
longer disagree about one corpus. The verdict is memoized beside a stat-only
corpus signature, so an unchanged corpus no longer pays the full content walk
on every query.
- An index path containing `%`, `?`, or `#` now names the literal file; the
SQLite URI parser previously truncated or percent-decoded it and the database
silently landed elsewhere. Migration: the old mislocated database is orphaned
and one `sync` rebuilds at the literal path.
- Newly created shard date directories are fsynced level by level, so a reported
capture success into a fresh chain survives a crash.
- The Codex adapter's pending-turn file — the owner's verbatim prompt between
hooks — and its directory are created and held owner-only (`0700`/`0600`)
instead of at the default umask.
- The alias-map digest length-frames entry keys, so two distinct thesauri can no
longer share one alias identity through a key containing the separator byte.
Every `alias_digest` value changes once; outstanding cursors decode as a typed
`malformed`, like the other identity changes in this release.

### Fixed

- Scopes can no longer start with `.`, refused by the payload contract's
`ValidScope`, the owner config, and the Pi adapter alike. A dot-led scope
published episodes into a directory the corpus walk skips: capture reported
`published`, the next sync removed the projection row, and search then reported
`no_match` over a `fresh` index while the file sat on disk, permanently
unfindable. Such a file now surfaces as a visible `skipped_malformed` exclusion
instead of vanishing silently.
- `OpenIndex` checks the foreign-root gate before the schema-disposal decision,
so an old-schema index recording another journal root's identity is rejected as
foreign instead of being emptied and re-keyed to the caller's root.
- Library `Search` with `NowMs: 0` (the documented live-clock spelling) now
applies live recency instead of reading every event time as future and silently
collapsing ordering to pure rarity. The CLI always passed a real clock and is
unaffected.

## 1.0.4 — 2026-08-08

### Fixed

- The Claude Code Stop hook now waits for the separate terminal-text
  transcript record and a short quiet interval before publishing. Previously
  it could treat progress prose and compact tool-call summaries as a completed
  result, then miss the final answer appended just afterward. A turn that never
  gains terminal text is skipped rather than stored as partial memory. Claude
  episode bodies now contain only that terminal response; tool names remain in
  the structured `## Tools` list, while tool arguments and results are excluded.
- `status` now checks the path and raw-byte hash of every visible episode
  candidate instead of inferring freshness from file and row counts alone.
  Same-count owner edits therefore report `stale` until `sync` reindexes them.
  Capture records its file hash transactionally; sync also tracks readable
  malformed and duplicate exclusions, removes vanished hash entries, refuses
  to call unreadable or over-budget candidates fresh, and commits root identity
  and exclusion accounting with the rebuilt projection.
- The Codex hook now retains its pending turn when the capture process exits
  unsuccessfully. A later retry can publish the completed turn instead of the
  hook silently discarding it; successful and duplicate captures still clear
  the pending file.
- The repository gate now fails when Node, Pi adapter dependencies, or Python
  are missing instead of reporting a misleading pass with skipped suites. Its
  permission checks use the native `stat` syntax on both Linux and macOS, and
  package verification materializes its nested archive even when invoked by
  an outer `npm publish --dry-run`.

### Changed

- Living documentation was reset to an as-built baseline. It now distinguishes
  shipped integrations from embedding contracts, names the actual CLI and Pi
  menu operations, documents elapsed 24-hour recency buckets, and states the
  current revision-freshness limit: an in-place body edit is reindexed by
  `sync`, but remains under the old evidence revision if its frontmatter
  `payload_digest` is unchanged.
- Maintenance utilities no longer default to development-machine paths or a
  local timezone: corpus inspection requires an explicit root, legacy import
  requires its source directory and timezone, and the executable resolves from
  `PATH` unless supplied. Retrieval evaluation now includes `paraphrase`
  queries in its scored metrics, matching its documented judged-set format.

## 1.0.3 — 2026-08-07

No format change. Episode bytes, identity, index schema, config, and the
CLI `--json` surface are untouched. The headline is sync: it is now
incremental, and a long one is visible while it runs.

- All three adapters now record the machine a turn ran on in the payload's
  optional `host` field. One journal root can be fed by several machines — a
  laptop syncing into a server's corpus — and without the label those episodes
  are indistinguishable. A hostname the payload contract would reject is
  omitted rather than sanitized, because this field names a real machine and a
  mangled label would assert one that does not exist. No format change: `host`
  was already in the payload contract and already rendered into frontmatter;
  nothing previously captured is re-identified.
- `/autojournal sync` now runs on a ten-minute maintenance budget instead of
  the 15 s query budget, and while a sync runs the footer status shows
  `autojournal: syncing index… Ns` with a one-second ticker, cleared when it
  finishes. A timed-out sync says so and points at running `autojournal sync`
  from a shell instead of reporting "(sync produced no output)", and the
  post-import sync no longer claims "index synced" when the rebuild did not
  finish.
- Sync is incremental. Previously every sync re-derived every episode's rows,
  postings, and stats — about 9 ms per episode, so a ~4k-episode journal took
  ~36 s and grew linearly forever. Now each file's SHA-256 is compared
  against a hash stored under a `sync_sha256:` meta key at index time, and a
  byte-identical file is skipped without parsing or writing: a routine
  no-change sync on that journal takes ~0.1 s. The hash lives in meta keys,
  not a column, so the frozen index schema v2 is untouched; losing the hashes
  degrades to a full rebuild, never a wrong projection. The change test is
  the raw bytes rather than the frontmatter `payload_digest`, which capture
  writes and hand edits do not update — a body-only edit is still reindexed,
  preserving the hand-editing promise. One transaction still covers the whole
  sync, so a torn run leaves neither a half-projection nor half-updated
  hashes. The report gains an `unchanged:` line and `indexed:` now counts
  only episodes (re)written this run; the full-rebuild path (first sync,
  disposed index) also benefits from a prepared postings statement, ~10% off
  the old per-token re-parse cost.

## 1.0.2 — 2026-08-05

No format change. Episode bytes, identity, index schema, config, and the
CLI `--json` surface are untouched.

### Fixed

- The Claude Code and Codex capture hooks only published when the session's
  working directory was under one hard-coded absolute path left over from the
  author's machine. Anyone else who wired in a hook got silent no-capture:
  no error, no episodes. Every session is now captured wherever it runs, and
  narrowing what enters memory is done with `autojournal default` or
  `journal_root`, which is where that decision belonged.
- Both hooks resolved the binary only at `~/.local/bin/autojournal`. They now
  check `AUTOJOURNAL_BIN`, then that path, then `PATH` — the same order the
  Pi adapter uses.
- The Codex hook discarded a stashed prompt when publication failed, losing
  the turn. The pending file now survives, so the turn is recoverable.

### Changed

- Both hooks send the turn's working directory as `workspace_root` episode
  provenance, which is how sessions from different projects stay
  distinguishable in a shared world. It is omitted rather than sent invalid
  if the path would violate the payload contract.
- `--default-root` is no longer advertised in `autojournal -h`. It still
  works, still ranks between owner config and the default root; new callers
  should use `--root` or owner config.
- `adapters/test_python_hooks.py` covers both hooks against a fake binary and
  runs in `scripts/verify.sh`. Neither hook had any test before, which is how
  the hard-coded path survived a release.

### Documentation

- Package README: dropped the `world_root` config-key migration note, which
  only ever applied to builds predating the first public release, and
  tightened the surviving `~/.pi/agent/journals` note to match the warning
  the adapter actually emits.
- Package and repository READMEs: corrected the capture claim from "every
  completed turn" to "every completed interactive turn", which is what the
  code does; headless and exec-spawned sub-agent runs are skipped.
- Repository README: rewritten. It no longer duplicates the package README's
  Pi usage sections, which had already drifted apart from them.
- `docs/ARCHITECTURE.md`: corrected the retrieval version identities to
  `aj-scorer.v2` / `aj-conf.v2`.
- `docs/AUTOJOURNAL_1_0_DESIGN.md`: recorded that scorer and confidence
  tuning to date used a private judged query set, so no ranking-quality claim
  in this repository is reproducible by a reader.

## 1.0.1 — 2026-08-04

- Default scorer moves to `aj-scorer.v2`: singular folding of plural query
  terms, and a per-episode cap on result regions so one long episode cannot
  fill a page. Ordering remains rarity × recency.
- Default confidence policy moves to `aj-conf.v2`, banding on the
  coverage-adjusted score. Ordering is unaffected; the band is display trust.
- Search discovery joins postings in memory rather than per row in SQLite.
- Experimental scorer knobs are available behind `AUTOJOURNAL_XP` for
  measurement runs; their defaults are zero-valued and reproduce v1 exactly.
- `scripts/retrieval-eval.py` runs a judged query set against any binary and
  reports ranking metrics. The judged set is not in this repository because
  it quotes journal content.

Both version identities are stamped on every search result, so a result
produced before this release remains attributable to the scorer that produced
it.

## 1.0.0 — 2026-08-03

- The core was ported from Zig to Go. Every on-disk format, identity rule,
  and CLI `--json` contract was frozen across the port and is pinned by
  `testdata/golden`, byte-compared against the archived Zig binary (git tag
  `zig-final`). Harness adapters observe no behavior change.
- `modernc.org/sqlite` (pure Go, no cgo) became the only dependency;
  everything else is stdlib. The binary is statically linked with no runtime
  dependencies.
- Fixed a bug where opening evidence with a single-component path closed the
  caller's journal root, which could fail every subsequent read in a
  long-lived process. Found in pre-release review; never shipped.

## 0.1.1 — 2026-07-30

- Menu-driven import of Pi session history predating AutoJournal.
- Capture runs a corpus-wide episode-id check, so a redelivered turn is
  recognized wherever it already lives rather than only on the date shard it
  would land on.

## 0.1.0 — 2026-07-30

First public release, on the Zig core. Completed-turn capture to Markdown,
the SQLite index projection, `memory_search` / `memory_get`, the owner CLI,
the thesaurus, and the Pi adapter.
