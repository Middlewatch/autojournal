# Architecture map for contributors

This is the orientation document for someone who pulled the repository and
wants to work on it. The binding product contract — laws, formats, typed
outcomes, release gates — is [`AUTOJOURNAL_1_0_DESIGN.md`](AUTOJOURNAL_1_0_DESIGN.md);
this file tells you where things live, how data flows, and which invariants
the code is organized around.

## The one-package shape

A single Zig package (`src/root.zig`) builds two ways from one source tree:

- a **module** an embedding host imports in its build (the planned Evoker
  built-in integration), and
- a **standalone static binary** (`src/main.zig`) that is simultaneously the
  owner CLI and the hook target for every other harness.

There is one scoring implementation and one storage protocol; adapters are
translation layers and are forbidden (by design, and by review) from
reimplementing storage, ranking, identity, or freshness rules.

## Module map (`src/`)

| Module | Owns |
|---|---|
| `contracts.zig` | Closed wire schemas (capture payload), typed outcome vocabularies, validation charsets, size budgets. Everything else consumes its types. |
| `identity.zig` | Episode identity: collision-resistant idempotency ID and the canonical payload digest that becomes the evidence revision. |
| `render.zig` | Episode Markdown rendering and frontmatter digest extraction. |
| `frontmatter.zig` | Frontmatter parsing at the read boundary (stored data is untrusted). |
| `store.zig` | Atomic publication: contained paths, owner-only dirs, temp-file + rename + dir sync, duplicate/conflict detection, date-only default layout with optional world/scope/lane directories. |
| `db.zig` | Vendored-SQLite binding: WAL, busy handling, typed error mapping, transient-bind discipline. |
| `index.zig` | The disposable SQLite projection: per-line postings, per-world term stats, identity metadata, root-digest foreign-index gate. Sync dedupes by episode identity (first copy stays indexed), skips dot-directories, and repairs owner-only permissions best-effort. |
| `retrieval.zig` | Pure lexical core: tokenizer, stop words, IDF scorer, recency nudge, confidence, cursor codec. Versioned as `aj-tok.v1` / `aj-scorer.v1` / `aj-conf.v1`. |
| `aliases.zig` | Owner-edited thesaurus (flat JSON, read fresh per search, digest-identified) and the opt-in weak-query miss log. |
| `search.zig` | `memory_search`/`memory_get` orchestration: discovery scan, word-start crediting, ranking, snippets, revision-verified evidence opening. |
| `config.zig` | The owner config file (XDG `config.json`): journal root, retrieval knobs, capture world/scope defaults. Every key is optional; the `default` command rewrites capture defaults atomically, preserving the rest. |
| `main.zig` | CLI wiring only: argument parsing, config resolution, JSON/text rendering. No product rules. |

Test files (`*_test.zig`) sit beside the modules and are collected through
the root test block in `root.zig` — a new test file must be added there or
it silently contributes nothing.

## Data flow

**Capture:** adapter builds a completed-turn JSON payload (identity and
lifecycle facts plus an explicit owner-selected session world/scope, when
present) → `contracts` validates the closed schema → `identity` derives
episode ID + digest → `store` publishes atomically → `index` updates in one
transaction. Source publication succeeding while indexing fails is a normal,
visible, repairable state (`sync`), never a capture failure.

**Retrieval:** query terms → additive alias expansion → vocabulary substring
scan (needles under 3 bytes skipped when longer ones exist) → postings fetch
under world/scope/lane filters → per-line crediting at word-start boundaries
against the source text → IDF ranking with day-quantized recency → span
dedup, confidence floor, cursor pagination. `memory_get` then opens one
reference, re-verifying the revision digest against the file on disk.

## The Pi adapter (`adapters/pi/`)

A deliberately thin TypeScript extension (single `index.ts`) that shells to
the binary: `agent_end` stashes the run, `agent_settled` publishes it (so a
retried run is captured once, in its final form), and the `memory_search`
and `memory_get` tools plus the interactive `/autojournal` menu wrap the
CLI's `--json` surface. Session world/scope selections are persisted as
branch-local Pi custom entries and applied symmetrically to capture and
search. It resolves its binary from `AUTOJOURNAL_BIN`, then
the bundled `bin/<platform>-<arch>/` build, then PATH.

Adapter rule worth internalizing: it **invents no memory policy**. It may
transport an explicit owner choice made in the session menu, but validation,
layout, lane semantics, indexing, and retrieval remain core rules. With no
owner config, the core resolves the host-neutral journal default from
`$XDG_DATA_HOME/autojournal/journals` (normally
`~/.local/share/autojournal/journals`). Owner configuration always wins.

## Verification workflow

```sh
./scripts/zig.sh build test        # unit suites (std.testing.allocator: leaks fail)
./scripts/verify.sh                # the full gate
```

`verify.sh` runs format check, both Zig test suites, a build, the adapter
typecheck + tests (including an end-to-end run against the freshly built
binary), design-contract presence greps, and a CLI smoke that exercises
capture → search → get → stale_revision → typed no_match in an isolated
root. A change is done when this gate is green; `zig build test` alone never
reinstalls `zig-out/bin`, so anything exercising the installed binary needs
a plain `zig build` first.

Release binaries for the npm package are cross-compiled by
`./scripts/build-adapter-binaries.sh` (linux x64/arm64 as static musl,
macOS x64/arm64; stripped via `-Dstrip=true`) into `adapters/pi/bin/`,
which is generated before pack and not committed. `release-check.sh`
rebuilds every target and inspects the actual npm tarball; package lifecycle
guards refuse to pack when any expected executable is missing.

## Toolchain

Zig 0.16.0 exactly, enforced by `scripts/zig.sh` (set `ZIG=/path/to/zig` to
point elsewhere). Node ≥ 22.6 for the adapter. SQLite is vendored under
`vendor/sqlite/` with provenance and a version-drift test.
