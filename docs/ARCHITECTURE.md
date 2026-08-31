# Architecture map for contributors

This is the orientation document for someone who pulled the repository and
wants to work on it. It tells you where things live, how data flows, and
which invariants the code is organized around.

## The one-engine shape

A single TypeScript engine (`src/`) serves three surfaces from one source
tree:

- the **Pi extension** (`index.ts`), which captures completed turns and
  registers the `memory_search`/`memory_get` tools and the `/autojournal`
  menu, all in-process;
- the **owner CLI** (`cli.ts`, installed as the `autojournal` bin), thin
  wiring over the same engine with the `--json` interface contract; and
- the **engine library** itself, importable by anything in this package.

There is one scoring implementation and one storage protocol. Surfaces
translate; storage, ranking, identity, and freshness rules belong in
`src/`.

The engine was ported to TypeScript in August 2026 from the Go build
(ADR 0001), which itself descended from an earlier systems-language build.
All corpus-durable formats froze at the first port: `testdata/golden`
dates from that freeze and is the behavioral authority on its own. The
2.0 port was additionally gated on a sweep of the maintainer's full
pre-port corpus (4,212 episodes) re-deriving every episode identity and
payload digest byte-identically.

## Module map (`src/`)

One capability owns each file; a file serves exactly one capability.
`test/engine/ownership.test.ts` pins this map: a module missing from it,
or a key symbol in the wrong module, is a failing test rather than a
review note.

| Module | Capability | Owns |
|---|---|---|
| `contracts.ts` | 1 Contracts | Closed wire schema (capture payload), typed outcome vocabularies, validation charsets, size budgets. Everything else consumes its types. |
| `json.ts` | 1 | The strict ordered JSON parser both boundaries share: duplicate-key rejection, raw number literals, key order. |
| `identity.ts` | 2 Identity and rendering | Episode identity: collision-resistant idempotency ID and the canonical payload digest that becomes the evidence revision. |
| `render.ts` | 2 | Episode Markdown rendering and frontmatter digest extraction. |
| `episode.ts` | 2 | Episode parsing at the read boundary (stored data is untrusted) and digest verification against content. |
| `paths.ts` | 3 Paths and containment | Where things live: root resolution, `HOME`/XDG rules, root digest, index path, thesaurus path, miss-log path. No filesystem descent. |
| `corpus.ts` | 3 | How the corpus is entered: sharded layout components, the symlink-refusing owner-only descent, atomic temp-write and directory fsync, the containment vocabulary, the contained readers, and `walkCorpus`, the one visibility rule every corpus traversal shares. |
| `config.ts` | 4 Configuration | The owner config file (XDG `config.json`): journal root, retrieval knobs, capture world/scope defaults. Every key is optional; the `default` command rewrites capture defaults atomically, preserving the rest. |
| `store.ts` | 5 Store | The capture transaction's decisions: the oversize truncation policy, atomic publication, duplicate/conflict classification, the corpus-wide redelivery check, prior-policy lookup for import. |
| `index.ts` | 6 Index | The disposable single-file snapshot projection (ADR 0002): episode rows, term-major postings, the stat-walk freshness signature, the writer lock with stale recovery, sync accounting, the root-digest foreign-snapshot gate. |
| `retrieval.ts` | 7 Retrieval | Pure lexical core: tokenizer, stop words, smoothed-IDF scorer, recency nudge, confidence, cursor codec. Versioned identities. |
| `aliases.ts` | 7 | The thesaurus read path: load, merge, digest, lookup. Read fresh per search, digest-identified. |
| `search.ts` | 8 Search | `memory_search`/`memory_get` orchestration: discovery scan, word-start crediting, ranking, snippets, revision-verified evidence opening. |
| `ops.ts` | 9 Operations | Owner maintenance accounting: `status`, `sync`, `catalog`, `reseal`. |
| `ops-alias.ts` | 9 | Alias maintenance and the weak-query miss log: the thesaurus write path and its aggregation. |
| `cli.ts` (root) | 10 CLI wiring | Argument parsing, config and root resolution, command dispatch, exit codes, every `--json` shape and text renderer. No product rules. |
| `index.ts` (root) | 11 Pi extension | Lifecycle translation, tools, menu, import. No product rules. |

## Data flow

**Capture:** the extension summarizes a settled run (every visible
assistant segment, in order — policy `pi-visible-v2`) into a raw payload →
`contracts` validates the closed schema → `store` applies the oversize
policy and derives identity + digest via `identity` → publishes atomically
under `corpus`'s containment discipline → `index` updates the snapshot
best-effort. Source publication succeeding while indexing fails is a
normal, visible, repairable state (`sync`) rather than a capture failure.

**Retrieval:** query terms → additive alias expansion and singular
folding → vocabulary substring scan over the snapshot's sorted terms →
postings under world/scope/lane filters → per-line crediting at word-start
boundaries against the verified source text, one read per episode per
query → smoothed-IDF ranking with day-quantized recency → span dedup,
per-episode cap, confidence floor, cursor pagination (aj2 cursors bind the
corpus signature and the first page's clock). `memory_get` then opens one
reference, recomputing the episode's digest from its content before
serving anything under the requested revision.

## Freshness

The snapshot records a SHA-256 over every visible episode's
(relpath, size, mtime) at the moment it was built. It is fresh exactly
while the corpus re-derives that signature; a move, add, remove, or
ordinary edit all change it. An edit that forges size and mtime stays
invisible to freshness and is caught by per-episode digest verification on
the serving path.

## Verification workflow

```sh
npm ci                 # once, and after any lockfile change
./scripts/verify.sh    # the full gate
```

`verify.sh` runs the typecheck, the whole suite (golden byte pins,
conformance cases, parse-boundary properties over the pinned fuzz seeds,
store/index/retrieval behavior, the CLI wire shapes, the extension
in-process), and an end-to-end capture → search → get → stale_revision →
typed no_match smoke through the node bin in an isolated root. CI runs
this same script on Linux and Windows, plus a weekly long randomized
property run. A change is done when this gate is green.

`release-check.sh` adds the packaging self-check: one version stamp
asserted in `package.json`, `index.ts` (`ADAPTER_VERSION`), and `cli.ts`
(`CLI_VERSION`); a dated changelog entry; and the npm pack layout.

## Toolchain

Node ≥ 22.6 (type-stripped TypeScript; the same floor Pi requires). The
engine uses only the Node standard library — node:crypto, node:fs,
node:path, node:zlib, node:test — and the package ships zero runtime
dependencies. typebox and the Pi SDK are the extension's dev/peer surface.
