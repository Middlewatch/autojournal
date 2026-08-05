# Architecture map for contributors

This is the orientation document for someone who pulled the repository and
wants to work on it. The binding product contract — laws, formats, typed
outcomes, release gates — is [`AUTOJOURNAL_1_0_DESIGN.md`](AUTOJOURNAL_1_0_DESIGN.md);
this file tells you where things live, how data flows, and which invariants
the code is organized around.

## The one-package shape

A single Go package (`src/`, import path
`github.com/Middlewatch/autojournal/src`, package `autojournal`) builds two
ways from one source tree:

- a **module** an embedding host imports in its build (the planned Evoker
  built-in integration), and
- a **standalone static binary** (`src/cmd/autojournal/`) that is
  simultaneously the owner CLI and the hook target for every other harness.

There is one scoring implementation and one storage protocol; adapters are
translation layers and are forbidden (by design, and by review) from
reimplementing storage, ranking, identity, or freshness rules.

The package was ported from Zig to Go in August 2026 with all on-disk
formats and the CLI `--json` surface frozen; the archived Zig tree (git tag
`zig-final`) remains the behavioral reference, and `testdata/golden` pins
its output byte-for-byte.

## Module map (`src/`)

| Module | Owns |
|---|---|
| `contracts.go` | Closed wire schemas (capture payload), typed outcome vocabularies, validation charsets, size budgets. Everything else consumes its types. |
| `identity.go` | Episode identity: collision-resistant idempotency ID and the canonical payload digest that becomes the evidence revision. |
| `render.go` | Episode Markdown rendering and frontmatter digest extraction. |
| `frontmatter.go` | Frontmatter parsing at the read boundary (stored data is untrusted). |
| `store.go` | Atomic publication: contained paths, owner-only dirs, temp-file + rename + dir sync, duplicate/conflict detection, date-only default layout with optional world/scope/lane directories. |
| `db.go` | SQLite driver discipline over `modernc.org/sqlite` (pure Go): WAL, busy handling, typed error mapping. |
| `index.go` | The disposable SQLite projection: per-line postings, per-world term stats, identity metadata, root-digest foreign-index gate. Sync dedupes by episode identity (first copy stays indexed), skips dot-directories, and repairs owner-only permissions best-effort. |
| `retrieval.go` | Pure lexical core: tokenizer, stop words, IDF scorer, recency nudge, confidence, cursor codec. Versioned as `aj-tok.v1` / `aj-scorer.v2` / `aj-conf.v2`. |
| `aliases.go` | Owner-edited thesaurus (flat JSON, read fresh per search, digest-identified) and the opt-in weak-query miss log. |
| `search.go` | `memory_search`/`memory_get` orchestration: discovery scan, word-start crediting, ranking, snippets, revision-verified evidence opening. |
| `config.go` | The owner config file (XDG `config.json`): journal root, retrieval knobs, capture world/scope defaults. Every key is optional; the `default` command rewrites capture defaults atomically, preserving the rest. |
| `ops.go` | Owner maintenance (`status`, `sync`) accounting and the capture-time corpus-wide redelivery check: the index answers whether an episode ID exists on any date shard, the named file's own frontmatter decides duplicate/conflict, and any index miss or mismatch falls through to normal publication. |
| `cmd/autojournal/main.go` | CLI wiring only: argument parsing, config resolution, JSON/text rendering. No product rules. |

Test files (`*_test.go`) sit beside the modules and run with the standard
`go test ./...`. `src/golden_test.go` and the CLI golden tests compare
against `testdata/golden`, the frozen output of the archived Zig binary.

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
go test -race ./...                # unit + golden suites
./scripts/verify.sh                # the full gate
```

`verify.sh` runs the format check, `go vet`, the race-enabled test suites,
a host-platform binary build into `adapters/pi/bin/`, the adapter
typecheck + tests (including an end-to-end run against the freshly built
binary), design-contract presence greps, and a CLI smoke that exercises
capture → search → get → stale_revision → typed no_match in an isolated
root. A change is done when this gate is green.

Release binaries for the npm package are cross-compiled by
`./scripts/build-adapter-binaries.sh` (linux x64/arm64, macOS x64/arm64;
`CGO_ENABLED=0`, stripped via `-ldflags '-s -w'`) into `adapters/pi/bin/`,
which is generated before pack and not committed. `release-check.sh`
rebuilds every target and inspects the actual npm tarball; package lifecycle
guards refuse to pack when any expected executable is missing.

## Toolchain

Go 1.26+ (`go.mod` pins the module's minimum). Node ≥ 22.6 for the adapter.
SQLite is `modernc.org/sqlite`, the pure-Go driver — the only dependency;
everything else is stdlib.
