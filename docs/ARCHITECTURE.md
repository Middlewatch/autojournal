# Architecture map for contributors

This is the orientation document for someone who pulled the repository and
wants to work on it. It tells you where things live, how data flows, and
which invariants the code is organized around.

## The one-package shape

A single Go package (`src/`, import path
`github.com/Middlewatch/autojournal/src`, package `autojournal`) builds two
ways from one source tree:

- a **module** available for an embedding host to import in its build, and
- a **standalone static binary** (`src/cmd/autojournal/`) that is
  simultaneously the owner CLI and the hook target for every other harness.

There is one scoring implementation and one storage protocol. Adapters remain
translation layers; storage, ranking, identity, and freshness rules belong in
the Go package.

The package was ported to Go in August 2026 from an earlier
systems-language build, with all on-disk formats and the CLI `--json`
surface frozen across the move. `testdata/golden` dates from that freeze
and is now the behavioral authority on its own; the predecessor is not
consulted for anything.

## Module map (`src/`)

One capability owns each file; a file serves exactly one capability.
`src/ownership_test.go` holds every top-level declaration to this map. A
symbol in the wrong file, or in no file the manifest claims, is a failing
test rather than a review note.

| Module | Capability | Owns |
|---|---|---|
| `contracts.go` | 1 Contracts | Closed wire schemas (capture payload), typed outcome vocabularies, validation charsets, size budgets. Everything else consumes its types. |
| `identity.go` | 2 Identity and rendering | Episode identity: collision-resistant idempotency ID and the canonical payload digest that becomes the evidence revision. |
| `render.go` | 2 | Episode Markdown rendering and frontmatter digest extraction. |
| `episode.go` | 2 | Episode parsing at the read boundary (stored data is untrusted). |
| `paths.go` | 3 Paths and containment | Where things live: root resolution, `HOME`/XDG rules, root digest, index path, thesaurus path, miss-log path. No filesystem descent. |
| `corpus.go` | 3 | How the corpus is entered: sharded layout components, the symlink-refusing owner-only descent, atomic temp-write and directory fsync, the containment vocabulary, both contained readers, and `WalkCorpus`, the one visibility rule every corpus traversal shares. |
| `config.go` | 4 Configuration | The owner config file (XDG `config.json`): journal root, retrieval knobs, capture world/scope defaults. Every key is optional; the `default` command rewrites capture defaults atomically, preserving the rest. |
| `doc.go` | 4 | The shared version stamp. |
| `store.go` | 5 Store | The capture transaction's decisions: atomic publication, duplicate/conflict classification, the corpus-wide redelivery check. |
| `db.go` | 6 Index | SQLite driver discipline over `modernc.org/sqlite` (pure Go): WAL, busy handling, typed error mapping. |
| `index.go` | 6 | The disposable SQLite projection: per-line postings, per-world term stats and their trigram side-table for vocabulary discovery, identity metadata, the memoized freshness verdict, root-digest foreign-index gate, sync accounting. |
| `retrieval.go` | 7 Retrieval | Pure lexical core: tokenizer, stop words, IDF scorer, recency nudge, confidence, cursor codec. Versioned identities. |
| `aliases.go` | 7 | The thesaurus read path: load, merge, digest, lookup. Read fresh per search, digest-identified. |
| `search.go` | 8 Search | `memory_search`/`memory_get` orchestration: discovery scan, word-start crediting, ranking, snippets, revision-verified evidence opening. |
| `ops.go` | 9 Operations | Owner maintenance accounting: `status`, `sync`, `catalog`, `reseal`, episode counting. |
| `ops_alias.go` | 9 | Alias maintenance and the weak-query miss log: the thesaurus write path and its aggregation. |
| `cmd/autojournal/main.go` | 10 CLI wiring | Argument parsing, config and root resolution, command dispatch, exit codes. No product rules. |
| `cmd/autojournal/report.go` | 10 | Every `--json` shape and every text renderer, in one file, because the `--json` surface is the Interface-tier contract. |

Test files (`*_test.go`) sit beside the modules and run with the standard
`go test ./...`. `src/golden_test.go` and the CLI golden tests compare
against `testdata/golden`, the frozen byte-level pins on stored formats and
wire shapes.

## Data flow

**Capture:** adapter builds a completed-turn JSON payload (identity and
lifecycle facts plus an explicit owner-selected session world/scope, when
present) → `contracts` validates the closed schema → `identity` derives
episode ID + digest → `store` publishes atomically → `index` updates in one
transaction. Source publication succeeding while indexing fails is a normal,
visible, repairable state (`sync`) rather than a capture failure.

**Retrieval:** query terms → additive alias expansion → trigram-backed
vocabulary discovery in sorted term order (every trigram candidate is verified
as a real substring match; a query whose needles are all under 3 bytes takes
the linear scan whole, and otherwise short needles stay excluded so they
cannot flood discovery) → postings fetch under world/scope/lane filters → per-line crediting at
word-start boundaries against the source text, one read per episode per
query → IDF ranking with day-quantized recency → span dedup, confidence
floor, cursor pagination. `memory_get` then opens one reference, recomputing
the episode's digest from its content before serving anything under the
requested revision.

## The Pi adapter (`adapters/pi/`)

A deliberately thin TypeScript extension (single `index.ts`) that shells to
the binary: `agent_end` stashes the run, `agent_settled` publishes it (so a
retried run is captured once, in its final form), and the `memory_search`
and `memory_get` tools plus the interactive `/autojournal` menu wrap the
CLI's `--json` surface. Session world/scope selections are persisted as
branch-local Pi custom entries and applied symmetrically to capture and
search. Subagent sessions publish only when the owner turns on Subagent
capture in the menu; the lever lives in `pi-adapter.json` beside the
resolved owner config and is read fresh at settle, because a branch-local
entry would never reach an exec-spawned subagent's process. It resolves
its binary from `AUTOJOURNAL_BIN`, then
the bundled `bin/<platform>-<arch>/` build, then PATH.

The adapter invents no memory policy. It may
transport an explicit owner choice made in the session menu, but validation,
layout, lane semantics, indexing, and retrieval remain core rules. With no
owner config, the core resolves the host-neutral journal default from
`$XDG_DATA_HOME/autojournal/journals` (normally
`~/.local/share/autojournal/journals`). Owner configuration always wins.

## Verification workflow

```sh
(cd adapters/pi && npm ci)          # install adapter development dependencies
go test -race ./...                # unit + golden suites
./scripts/verify.sh                # the full gate
```

`verify.sh` runs the format check, `go vet`, the race-enabled test suites,
five bounded parse-boundary fuzz steps, a host-platform binary build into
`adapters/pi/bin/`, the adapter typecheck + tests (including an end-to-end
run against the freshly built binary), the Python hook suite with its
recorded-transcript replays, the cross-adapter conformance suite,
and a CLI smoke that exercises
capture → search → get → stale_revision → typed no_match in an isolated
root. A change is done when this gate is green.

Release binaries for the npm package are cross-compiled by
`./scripts/build-adapter-binaries.sh` (linux x64/arm64, macOS x64/arm64;
`CGO_ENABLED=0`, stripped via `-ldflags '-s -w'`) into `adapters/pi/bin/`,
which is generated before pack and not committed. `release-check.sh`
rebuilds every target and inspects the actual npm tarball; package lifecycle
guards refuse to pack when any expected executable is missing.

## Toolchain

Go 1.26.4+ (`go.mod` pins the module's minimum), Node ≥ 22.6 for the Pi
adapter, and Python 3 for the standalone hooks and their tests. SQLite is
`modernc.org/sqlite`, the pure-Go driver and the core's only direct module
dependency. The compiled executable has no external runtime dependency.
