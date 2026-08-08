# Changelog

Versions are the npm package (`autojournal`) and the bundled core binary,
which share one version stamp. On-disk contracts — episode Markdown and
frontmatter, episode-id and digest derivation, index schema, and the config
file — are frozen within 1.x; a change to any of them would be a major
version.

`adapter_version` is recorded in episode frontmatter but is deliberately
excluded from the payload digest, so upgrading never re-identifies or
re-publishes existing episodes.

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
