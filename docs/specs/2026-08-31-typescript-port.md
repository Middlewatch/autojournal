# typescript-port

Date: 2026-08-31   Status: draft

## Problem

AutoJournal's engine is a Go binary that its only living consumer, the Pi
extension, reaches by spawning a subprocess and parsing `--json` output. The
other consumers that justified that shape — evoker's planned native import and
the claude-code/codex hooks — are gone or abandoned, so the repository carries
a second language, a wire protocol, a cross-compile matrix, and bundled
binaries for nobody. The owner ruled: one TypeScript engine, Pi-only, no
SQLite (ADRs 0001, 0002).

## Outcome

On a machine with only Node and Pi, `pi install npm:autojournal` at 2.0.0 —
no bundled binaries, no runtime dependencies — captures a completed turn into
the existing journal root, and `memory_search`/`memory_get` return ranked,
revision-verified evidence drawn from all ~4,233 pre-port episodes. The
parity gate decides it: every `testdata/golden` byte pin and
`conformance_cases.json` case green in the TS engine's tests, the extension
suite green against the in-process engine, and a one-shot sweep that parses
every episode in the live corpus and re-derives its identity and digest
byte-identically. The Go tree is deleted only after that gate passes.

## Non-goals

- **A new episode format.** `aj-episode.v1` stays byte-frozen (owner ruling
  2026-08-31). The evoker-review format fixes (identity covering scope/lane,
  injectively decodable bodies) are recorded prior art, not this spec's work.
- **The overflow sidecar.** Truncation with recorded drop is the oversize
  policy; the sidecar is the named durable escalation, built only when a real
  truncation is ever observed. Largest episode to date: 152 KiB against a
  2 MiB cap.
- **Other harnesses.** The claude-code/codex hook adapters, the Python hook
  suite, `testdata/transcripts`, and the conformance driver's multi-adapter
  role are deleted, not ported. `conformance_cases.json` survives as fixtures
  for the one engine.
- **SQLite in any form** — native, WASM, or vendored (ADR 0002).
- **The supersede path.** No Pi consumer exists: capture is once per settled
  turn and import redelivers identical bytes, which classifies as duplicate
  first. Equal recorded digest stays duplicate; anything else at an occupied
  path is a fail-visible conflict. `superseded` leaves the capture-outcome
  vocabulary at the major version.
- **Agent-writable aliases.** The thesaurus changes only through the owner's
  confirm flow in the menu, or the owner's editor.
- **Capturing thinking traces or tool arguments.** Measured 2026-08-31 (see
  the dated note): thinking text is persisted for only half of sampled
  turns and its durable facts restate into visible text; tool arguments run
  9× the size of captured text and mostly duplicate repo state. Bash command
  strings specifically stay out as a secret-exposure surface.

## Decisions

- **Package identity.** This repository, npm `autojournal` 2.0.0. The
  corpus-durable freeze is exactly what the major bump communicates: a v1
  corpus is readable by v2 with no migration.
- **Layout during and after.** The TS engine grows at `adapters/pi/engine/`
  so the existing package, tests, and `pi install` wiring keep working while
  Go remains authoritative. The deletion slice hoists the package to the repo
  root: `src/` becomes the engine (preserving the one-capability-per-module
  map and an ownership test equivalent), with the extension entry and the CLI
  beside it. Until then the Go tree is untouched and shipping.
- **The CLI survives as a Node bin** with the same verbs — `capture`,
  `status`, `catalog`, `sync`, `reseal`, `search`, `get`, `alias`, `default`
  — and the same `--json` interface contract, as thin wiring over the same
  in-process engine the extension calls. Additive changes stay minor;
  `superseded`'s removal rides the major.
- **Ranking adopts the S0-review derived-tier fixes** (owner ruling
  2026-08-31): smoothed IDF `ln(1 + (N − df + 0.5)/(df + 0.5))`, streamed
  postings with a bounded top-K, a freshness signature digesting the sorted
  (relpath, size, mtime) stat walk, and a cursor additionally bound to the
  corpus signature and the first page's clock. Scorer and index identities
  version-bump; search-result goldens are re-minted under the new scorer
  version, while `--json` shapes stay pinned.
- **Capture policy v2 keeps every visible assistant text segment** (owner
  ruling 2026-08-31): all nonempty assistant text blocks in turn order,
  joined with blank lines, replace the current last-nonempty-wins rule
  (`adapters/pi/index.ts:256`). A new `capture_policy` value carries it, and
  because `capture_policy` is hashed into episode identity, import's dedupe
  becomes policy-aware in the same slice: before publishing, import checks
  for an existing episode with the same session and turn under the prior
  policy, so re-importing an old session never double-stores its turns.
- **Oversize turns truncate instead of reject** (owner ruling 2026-08-31):
  deterministic tail truncation per side with the dropped byte count in new
  optional frontmatter fields, surfaced in the status/sync reports and the
  menu rather than vanishing. Existing episode bytes are untouched.
- **The miss log and alias curation move into Pi** (owner ruling 2026-08-31):
  a "search quality" section of the `/autojournal` menu shows weak-query
  aggregation and candidate aliases, with add/remove behind the same confirm
  pattern reseal uses. The CLI `alias` verbs drive the same engine functions.
- **Concurrency model.** Concurrent Pi sessions over one corpus are the
  normal case. Publication keeps atomic temp-write plus rename with directory
  fsync where the platform supports it; index writers serialize on an
  O_EXCL-created lock file with stale-lock recovery; snapshots replace by
  atomic rename and readers only open complete snapshots.
- **Zero runtime dependencies** stays a product property. node:crypto,
  node:fs, node:path, node:zlib, node:test cover the engine; typebox and the
  Pi SDK remain the extension's dev/peer surface. Node floor matches Pi's
  supported floor (type-stripped TS, so ≥ 22.6).
- **Fuzzing becomes property + regression testing.** The five parse-boundary
  targets (payload, config, episode, alias map, cursor) keep their round-trip
  and containment invariants as node:test properties over a seeded in-repo
  generator; every existing fuzz seed under `src/testdata/fuzz` becomes a
  regression fixture. The weekly long-fuzz CI job becomes a long randomized
  run of the same properties.
- **Gates reshape at the deletion slice.** `verify.sh` v2: typecheck, engine
  tests (goldens, conformance, properties, seeds), extension tests, and the
  end-to-end capture → search → get → `stale_revision` → `no_match` smoke
  through the node bin in an isolated root. `release-check.sh` drops
  cross-compilation and keeps the version-stamp cross-check (package.json,
  engine version constant, dated changelog) and npm pack layout. CI runs
  verify on Linux and Windows; Windows is a tested, claimed target (owner
  ruling 2026-08-31).
- **Config is read verbatim** by the same XDG rules from the same
  `config.json`; the `default` verb keeps its atomic rewrite-preserving
  behavior.

## Seams under test

- The engine library, driven by the golden byte pins (`testdata/golden`,
  prior art `src/golden_test.go`) and the shared cases
  (`adapters/conformance_cases.json`, prior art `adapters/test_conformance.py`),
  now as native node:test suites.
- The CLI `--json` surface, driven by the verify.sh end-to-end smoke against
  the node bin (prior art: the existing e2e block in `scripts/verify.sh`).
- The extension surface — capture, tools, menu, import — through the existing
  adapter suite (`adapters/pi/test/`), rewired to the in-process engine with
  behavior assertions unchanged.
- The live-corpus sweep: a scratch script over the real journal root
  asserting byte-identical identity and digest re-derivation for every
  episode; its summary is recorded in this spec before deletion.

## Slices

- [x] S1 Format core in TS: contracts, identity, render, parse, paths,
      config. Golden byte pins and the identity/render/config conformance
      cases green; parse-boundary properties running with the fuzz seeds as
      fixtures; the Windows CI job exists from this slice.
- [x] S2 Corpus and store: containment walk, atomic publication,
      duplicate/conflict classification (supersede dropped), truncation
      policy with its frontmatter fields; `capture` and `default` on the node
      bin. (after S1)
- [x] S3 Snapshot index: build, incremental sync, lock-file writers,
      stat-walk freshness, `status`/`catalog`/`sync`/`reseal` on the bin;
      build time, RSS, and snapshot size measured on the real corpus and
      recorded here. (after S2)
- [x] S4 Retrieval: tokenizer, stop words, smoothed IDF, recency,
      confidence, corpus-bound cursor, alias expansion, streamed top-K,
      revision-verified `search`/`get` on the bin; search goldens re-minted
      under the new scorer version. (after S3)
- [ ] S5 Extension in-process: subprocess supervision replaced by direct
      engine calls; menu, session tools, import, capture toggle; capture
      policy v2 (all visible assistant text) with policy-aware import
      dedupe; the adapter suite green against the in-process engine.
      (after S4)
- [ ] S6 Search quality: miss-log capture and aggregation in the engine, the
      menu section with weak queries, candidates, and confirmed add/remove;
      `alias` verbs on the bin. (after S5)
- [ ] S7 Parity gate and deletion wave: live-corpus sweep recorded; Go tree,
      hook adapters, Python suites, transcripts, and cross-compile machinery
      deleted; package hoisted to the repo root; verify.sh, release-check,
      CI, README, and ARCHITECTURE rewritten; changelog carries 2.0.0
      unreleased. (after S6)

## Open questions

- Index residency in the Pi process: load-per-search versus a cached
  snapshot invalidated by the freshness signature. S3 measured in Node
  (below); S5 decides against Pi's responsiveness.
- ~~Snapshot encoding~~ Settled by S3 measurement: compact JSON, no gzip.
- ~~Directory fsync on Windows~~ Settled in S2: syncDir is a documented
  no-op on win32; publication degrades to write-then-rename durability.

2026-08-31 (S3 measurement) — real corpus copy, 4,208 episodes / 32 MiB,
node 22 on this machine: cold snapshot build 2.0 s, warm resync 1.17 s,
snapshot 16.8 MiB compact JSON holding 40,072 terms; load 0.13 s warm /
0.29 s in a fresh process; stat-walk freshness signature 0.02 s;
load-only process RSS 236 MiB (the Map-of-arrays postings graph — a
typed-array layout is the escalation if S5's residency decision needs a
smaller resident set). Gzip: 5.8 MiB on disk but +0.39 s per write and
0.22 s decompress+parse versus 0.13 s plain load — rejected; disk is
cheap, search latency is not. Artifacts: scratchpad measure-s3.mjs.

2026-08-31 (capture-policy study) — 50 pi episodes sampled at random from the
1,520 whose session JSONL still exists, each turn reconstructed from the log
and diffed against what capture kept. Mid-turn assistant text exists in 32 of
50 turns, adds 22% over captured bytes (39.5 KiB vs 176.5 KiB), and was 100%
novel — no segment was a substring of the final reply — carrying goal
statements, measured results, verdicts, and commit hashes. Thinking text
exists in only 26 of 50 turns (provider-dependent persistence) at 91.8 KiB,
largely process narration whose facts restate into visible text. Tool
arguments total 1.6 MiB (9× captured text), dominated by write/edit file
contents; bash command strings alone are 186.7 KiB with occasional recall
value and a secret-exposure risk. Ruling: visible text in, thinking and
arguments out. Study artifacts: scratchpad `capture-study/` (study.py,
rows.tsv, excerpts.json).
