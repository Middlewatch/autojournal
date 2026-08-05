# AutoJournal design basis

Status: as built, 2026-08-03. This document records the design decisions
behind AutoJournal as shipped (the Go core, the standalone static binary,
and the Pi adapter published as the npm package) and the reasoning a future
maintainer needs to extend it. The core was originally built in Zig and
ported to Go in August 2026 with every on-disk format, identity rule, and
CLI contract frozen; where this document reasons about the implementation
language, the reasoning carried over unchanged. The codebase map for
contributors is [`ARCHITECTURE.md`](ARCHITECTURE.md).

AutoJournal has two jobs:

1. durably write completed agent turns as owner-controlled memory episodes; and
2. retrieve exact, ranked, bounded evidence from those episodes.

Everything here exists to make those two paths correct, inspectable, fast,
portable, and recoverable. Memory curation, reflection, knowledge synthesis,
wiki maintenance, and automatic promotion of generated claims are not part of
AutoJournal. If that work ever exists, it is a separate product with separate
write authority that consumes AutoJournal's public evidence interface.

## How this design came to be

Three earlier implementations shaped every major decision:

- A **TypeScript v1 Pi extension** proved the product: one Markdown episode
  per completed turn, lexical rarity scoring, hand-curated aliases, and
  pull-based recall genuinely helped an agent reuse past work. It also proved
  the failure modes — always-fire noisy results, infix over-crediting, and
  policy scattered through harness-side code.
- A **Rust rewrite** was started and stopped. Its scope documents survive as
  evidence and fixture provenance only; nothing in AutoJournal maintains
  compatibility with its wire or disk formats.
- A **split design** — a scripted policy layer above a compiled substrate, so
  one implementation could run inside a host engine's extension VM — was
  drafted and retired. Once an embedded host could link the core package
  natively, the second runtime bought nothing but a versioned cross-language
  contract to maintain in two hosts.

The result is the single-package design described below: one
implementation of storage, identity, ranking, and freshness, consumed either
as an imported module or as a standalone static binary. Product-rule changes
require recompilation — an accepted cost for a product whose scoring policy
is settled evidence, not an iteration surface.

## Product position

AutoJournal is an independent product and repository, not a session subsystem
of any harness and not policy for one machine.

- An embedding host engine (Evoker is the intended first) ships an
  opinionated built-in integration by importing the AutoJournal Go package
  directly in its build, disableable by owner configuration. Dependency
  direction is host → AutoJournal; AutoJournal never depends on a host.
- Every other harness uses the standalone AutoJournal binary: one static
  executable that is simultaneously the versioned stdio helper, the hook
  target, and the owner CLI. A mandatory daemon is not part of local
  operation.
- Pi is supported through a thin TypeScript adapter that supervises the
  binary (the shipped npm package). Hook-based harnesses (Codex, Claude
  Code, and similar) can use hook entries that invoke the same binary.
- All adapters use the same AutoJournal implementation and on-disk contracts;
  none may reimplement storage, ranking, identity, or freshness rules.

A host engine owns its session log, operation lifecycle, branching, context
projection, compaction, provider traffic, tool execution, and turn
settlement. AutoJournal receives an explicit completed-turn projection after
durable settlement. It owns memory episode publication, its disposable
search index, ranked recall, evidence references, and memory diagnostics.
AutoJournal never mutates host context or treats a compaction summary as
authoritative source memory.

## Design laws

1. **Markdown is authority.** Human-readable episode files are the recoverable
   source of truth.
2. **Indexes are disposable.** SQLite and any later derived representation can
   be deleted and rebuilt from Markdown.
3. **One completed turn is one atomic episode.** Writes never append a
   half-turn to a shared session file.
4. **Capture cannot break the agent turn.** Failure is visible and retryable,
   but never changes a successfully settled harness operation into a failure.
5. **Search is not evidence opening.** `memory_search` returns ranked references
   and bounded snippets; `memory_get` opens exact source evidence.
6. **No match is a real result.** Empty, stale, unavailable, malformed, and
   timed-out memory are distinct typed outcomes.
7. **Recalled text is untrusted evidence.** It carries source and revision
   provenance and is never presented as instruction.
8. **Scope is explicit.** Core code does not infer a world, project, lane, or
   privacy policy from cwd, prompts, or harness names.
9. **Pull first.** Agent-invoked recall is the default. Ambient injection is
   never silently enabled.
10. **No generated durable claims.** Model-written summaries, aliases, or
    reflections do not enter the authoritative episode corpus automatically.

## Implementation shape

The implementation language is Go throughout (ported from the original
Zig build with frozen contracts). One package owns all product rules and
durability:

- versioned capture and query contracts;
- episode validation and rendering;
- world, scope, and lane policy;
- lexical tokenization, aliases, scoring, pagination, and result formatting;
- index orchestration and freshness decisions;
- adapter-neutral diagnostics;
- owner-only directory and file creation;
- contained path resolution and symlink rejection;
- same-directory temporary files, exclusive publication, file sync, and parent
  directory sync;
- bounded index-writer serialization (WAL plus a bounded busy timeout;
  cross-process locking goes through SQLite itself, with no separate
  advisory-lock layer);
- SHA-256 digests; and
- a statically linked SQLite driver (`modernc.org/sqlite`, pure Go) with
  transactions, WAL, busy handling, and corruption diagnostics.

The package builds two ways from one source tree: a Go module an embedding
host imports in its build, and a standalone static binary that is the
helper, the owner CLI, and the hook target in one executable. The current
adapter transport is the CLI's `--json` surface; a framed-stdio protocol for
long-lived helper supervision is a designed extension point, not yet built.

No AutoJournal capture or ranking policy lives in a host engine; the host
calls AutoJournal's public API and owns nothing behind it. SQLite is linked
statically. No dynamic module loading is required.

The module map lives in [`ARCHITECTURE.md`](ARCHITECTURE.md). The direction
that may not change: contracts ← store/index/retrieval ← search ← CLI
wiring; a host-side adapter (lifecycle translation, tool registration, UI)
lives in the host's repository and carries no logic beyond wiring.

### Configuration

Configuration belongs to AutoJournal, not to each harness. The embedded
module and the standalone binary read the same owner config file (XDG,
`~/.config/autojournal/config.json`) for journal root, retrieval knobs, and
capture defaults. Every key is optional. A fresh install defaults to the
host-neutral `$XDG_DATA_HOME/autojournal/journals` (normally
`~/.local/share/autojournal/journals`), so every conforming harness under
one user account shares the corpus without setup. Owner configuration always
wins. Harness wiring invents no policy, but a first-class host may transport
an explicit owner-selected session world/scope to the core.

### What an embedding host must provide

The built-in integration path is native, so AutoJournal does not depend on a
host's scripted extension API. A host engine needs three generic
capabilities — useful to any consumer, not AutoJournal-private hooks:

1. **Completed-turn projection.** After durable settlement, a droppable
   seam supplies the original user content, one nonempty terminal assistant
   result, source wall-clock timestamps, outcome, session and operation
   identities, harness identity, and branch provenance when present. The
   adapter combines this neutral projection with explicit world/scope/lane
   configuration; the host does not invent memory policy.
2. **Memory tools in the ordinary tool lifecycle.** `memory_search` and
   `memory_get` register through the host's normal tool path — provider
   advertisement, call-ID validation, limits, middleware, execution, and
   durable tool-result recording.
3. **Observable nonfatal failures.** Failed captures, index lag, and repair
   requirements are counted and visible through status and frontend
   diagnostics without failing the settled operation.

## Authoritative episode format

### Physical layout

Every completed turn is published as one immutable-by-default Markdown file,
sharded so directory operations stay bounded as the corpus grows:

```text
<root>/YYYY/MM/DD/<episode-id>.md
```

The default path represents main/default/conversation and elides those
classification names for direct use as an Obsidian vault or editor tree.
Non-default classifications add reserved components before the date:

```text
<root>/scopes/<scope>/YYYY/MM/DD/<episode-id>.md
<root>/worlds/<world>/YYYY/MM/DD/<episode-id>.md
<root>/worlds/<world>/scopes/<scope>/lanes/<lane>/YYYY/MM/DD/<episode-id>.md
```

Frontmatter, not the path, is authoritative for every classification: the
layout is a human convenience, and sync trusts what a file says, not where
it sits.

One-file-per-turn gives capture a single atomic publication boundary,
permits concurrent harness writers without shared append locks, makes
duplicate delivery idempotent, and gives deletion and evidence references
episode granularity. Session-file append is rejected as an alternate
authority, permanently.

### Required identity

Every episode has closed, versioned frontmatter containing at least:

- schema version and episode ID;
- world ID, lane, and explicit scope;
- source harness/application and adapter version;
- session ID and operation/turn ID;
- source event time and capture time;
- capture/redaction policy identity;
- terminal outcome;
- canonical payload digest; and
- optional parent/delegation and branch provenance.

The episode ID is derived from a collision-resistant idempotency identity
that includes the source harness, session ID, operation/turn ID, world, and
capture policy version. Re-delivering the same identity and payload succeeds
without a second episode. Re-delivering the same identity with different
content is a visible conflict. Scope and lane are deliberately outside the
identity: they are classification, not identity, and an episode's identity
survives reclassification.

The payload digest covers canonical identity metadata plus the body while
excluding the digest field itself. It is the revision identity used by
evidence references.

### Required body

The minimum completed-turn body contains:

- the original user content;
- one nonempty terminal assistant result;
- explicit redaction markers where policy removed content; and
- an optional structured list of tool names plus allowlisted safe metadata.

Shell bodies, search queries, credentials, arbitrary tool arguments, hidden
reasoning, and raw provider metadata are excluded by default. Adapters may
apply stricter redaction, but must identify the policy used. Prompt
inspection never selects capture lanes or privacy behavior.

Delegated work may use a compact body containing the assigned task and
terminal child result with parent/run/child identities. Evaluation episodes
use an explicit evaluation lane and are excluded from normal recall and
corpus statistics.

## Capture transaction

For each accepted completed turn, AutoJournal performs:

1. validate the closed payload and explicit policy;
2. derive episode ID, target path, and canonical digest;
3. ask the index whether this episode ID already exists anywhere in the
   corpus, and when it does, classify the redelivery against that file's
   own frontmatter — duplicate or conflict — without writing anything (the
   file is the authority; a missing, stale, or contradicted index answer
   falls through to normal publication);
4. create the sharded owner-only directory through contained paths;
5. write the complete episode to an owner-only temporary file in the target
   directory;
6. sync the temporary file;
7. publish with atomic no-replace semantics;
8. sync the parent directory;
9. update the SQLite projection in one transaction; and
10. report success only after source publication, with index freshness
    reported independently.

If publication finds an existing target, AutoJournal validates its identity
and digest. An exact duplicate is success; any mismatch is a typed conflict.
If the source episode publishes but SQLite update fails, capture remains
successful, the world becomes visibly index-stale, and `sync` repairs the
projection from Markdown. Temporary files and interrupted transactions are
detected and reconciled on the next status/sync operation.

No source episode is deleted or rewritten merely because indexing failed.
Capture errors are returned to the adapter and recorded in AutoJournal
status, but a harness turn that already settled remains settled.

## Owner edits, moves, and deletion

Markdown remains owner-controlled:

- An owner edit creates a new revision digest after validation and reindexing.
- An evidence reference to the prior digest returns `stale_revision`; it never
  silently serves edited content as the old evidence.
- Moving a file within the same world preserves episode identity after sync.
- Duplicate episode IDs are deduplicated (the first copy found stays served,
  later copies are counted); malformed frontmatter is excluded with visible
  diagnostics. Files are never merged by filename.
- A missing episode is removed from the projection on sync. `memory_get` for
  its prior reference returns `gone`.
- AutoJournal performs no age-based or automatic deletion. Confirmed bulk
  deletion with a dry-run inventory is a designed owner operation, not yet
  built; today the owner deletes files directly and rebaselines with `sync`.
- SQLite is never required for recovery: the corpus is the export.

## World, scope, and lane model

The core accepts explicit policy objects and never inspects cwd or prompt
text to invent them.

- A **world** is an owner-controlled memory corpus and index.
- A **scope** may identify global, workspace, project, session, delegated, or
  user-defined boundaries.
- A **lane** distinguishes normal conversation, delegated work, evaluation,
  and explicit imported legacy source.

Public installations default to shared main/default capture and recall
across sessions, projects, and conforming harnesses under the local user
account. No setup choice is required. An owner explicitly selects another
world for a separate corpus or another scope for a bounded collection; a
first-class host may persist that selection with its conversation and may
save it as the owner default. Evaluation is always system-selected and
excluded from ordinary queries.

Source directories, episode files, indexes, locks, and helper state are
owner-only. Reads and writes reject symlink escapes and paths outside the
configured journal root.

## SQLite projection

SQLite contains only rebuildable retrieval state:

- episode ID, revision digest, path, world/scope/lane, timestamps, and policy
  identity;
- indexed body regions and source line bounds;
- normalized terms/postings and corpus document frequencies;
- index schema, scorer, tokenizer, alias, and configuration identities; and
- freshness and repair state.

SQLite uses WAL with an explicit busy timeout and bounded retry policy.
Source publication does not wait indefinitely for an index writer. Multiple
processes may publish unique episodes concurrently; index updates serialize
through SQLite, and a failed/busy update leaves a known-stale projection
repairable by `sync`.

Repair ships as `sync`: one transaction re-walks the corpus and rebuilds the
projection in place, so a torn rebuild rolls back rather than leaving a
half-projection. A generation-switching `rebuild` (new projection validated
against a source inventory, then swapped atomically) is a designed extension
for corpora large enough that in-place rebuild windows matter. A missing,
corrupt, wrong-root, wrong-schema, or wrong-configuration index is never
interpreted as an empty memory corpus.

## Proven lexical retrieval

AutoJournal ships lexical retrieval first. SQLite discovers candidates and
maintains incremental corpus statistics; a versioned AutoJournal scorer owns
final rank.

The preserved behavior includes:

- lowercase/punctuation/stop-word tokenization;
- exact-query duplicate term weights;
- manually curated deterministic aliases;
- `sum(log(N/df))` rarity scoring;
- day-quantized recency as a nudge, not an override;
- frontmatter exclusion;
- episode/source-span deduplication;
- deterministic stable tie-breaking and pagination; and
- exact source and revision provenance.

Generic SQLite FTS/BM25 rank is not the final scorer. An earlier prototype
was faster but lost judged evidence, while a parity prototype showed SQLite
candidate discovery can preserve AutoJournal ranking.

LLM query rewriting is excluded: it previously displaced exact answers.
Alias promotion remains manual. Weak-query logging is opt-in, owner-private,
bounded, and never writes aliases automatically.

### No-memory decision

A lexical occurrence is not automatically relevant memory. A versioned
confidence policy (`aj-conf.v2`, owner-tunable floor) is reported separately
from score so ordinary noise queries can return `no_match` instead of ten
low-quality results. The version exists so a recalibration is a visible
identity change rather than a silent behavior shift, and it has been used
once that way: `aj-conf.v1` → `aj-conf.v2` in 1.0.1, ratified against a
private judged query set. Recalibration against a larger, publishable set
remains open evidence work.

## Public operations

### `capture_completed_turn`

Idempotently publishes one completed episode and reports source/index
status. This is an adapter operation (the binary's `capture` command), not
an agent-facing tool.

### `memory_search`

Input includes world, scopes, lanes, query, limit/cursor, and optional
accepted scorer version. Output contains:

- typed outcome;
- ranked stable evidence references;
- bounded matched snippets and exact source line bounds;
- score and confidence information;
- world/scope/lane and capture-policy provenance;
- scorer/tokenizer/alias/configuration identities; and
- index freshness and diagnostics.

Search does not return an unbounded episode body.

### `memory_get`

Opens one evidence reference with explicit line bounds. It validates episode
ID and revision digest and returns exact source evidence, current
provenance, and trust metadata. Edited and deleted evidence return
`stale_revision` and `gone` respectively.

### Owner operations

`status`, `catalog`, `sync`, `default`, and `alias` maintenance ship in the
CLI and through the adapter's `/autojournal` command, reporting source and
index health separately. A generation-switching `rebuild`, a dry-run
`inventory`, `export` with a digest manifest, and confirmed deletion are
designed under the same reporting contract, not yet built.

Outcomes distinguish at least `match`, `no_match`, `stale_revision`, `gone`,
`index_stale`, `timeout`, `unavailable`, `permission_denied`, `malformed`,
`conflict`, and `internal_error`.

The v1 `read_memory` compatibility alias was dropped before the first public
release — no external installed base ever had v1, so every integration
exposes `memory_search` and `memory_get` directly.

## Adapter behavior

### Embedded host (Evoker)

The designed built-in native integration:

- subscribes to the completed-turn seam and translates the explicit payload
  into `capture_completed_turn`;
- registers `memory_search` and `memory_get` through the host's ordinary
  tool lifecycle;
- exposes status/sync commands and frontend diagnostics;
- records memory tool results through the host's ordinary durable
  tool-result path; and
- can be disabled by owner configuration without changing session storage or
  host behavior.

Capture handler failure is droppable and observable. Recall tool failure is
a normal typed tool result. Ambient projection injection is disabled.

### Pi and hook-based harnesses

Adapters translate their lifecycle events into the same completed-turn
schema, register or describe the compatible tools, and render diagnostics.
Pending-turn pairing is adapter state. They do not write Markdown directly,
and they invent no memory policy: world and scope come from AutoJournal's
own config file, except that a first-class host may transport an explicit
owner-selected session world/scope. Lane and every other policy remain
AutoJournal's.

Only interactive sessions publish episodes. Headless runs and exec-spawned
sub-agents are synthetic work products: capturing them would pollute the
corpus with generated data, so the Pi adapter skips capture outside its
interactive modes while leaving recall tools available.

## Legacy import

Pre-AutoJournal session Markdown is valid read-only legacy source. The
design reserves the `imported` lane for it: each legacy turn receives a
deterministic synthesized evidence identity and revision digest without
rewriting its source file, and imported episodes are distinguishable from
native capture forever. An importer is not part of the shipped surface;
anyone building one inherits those constraints.

## Verification

The shipped verification gate (`scripts/verify.sh`, run from a clean
checkout) enforces:

- closed runtime schemas rejecting unknown, malformed, cross-world, and
  over-budget data;
- leak-checked unit and fault-path tests across publication, indexing,
  dedupe, corruption, and containment (including symlink rejection and
  owner-only permissions);
- an end-to-end smoke of capture → search → alias rescue → evidence opening
  → stale revision → typed no-match, plus fresh-install defaults, journal
  relocation, owner-default selection, vault dedupe, and shared-directory
  refusal;
- the Pi adapter's typecheck and test suite against the built binary; and
- release packaging checks (`scripts/release-check.sh`): every platform
  binary rebuilt, the actual npm tarball inspected, and version identity
  cross-checked between the manifest, the adapter, and the binary.

Evidence deliberately not yet collected, for whoever takes this further:
large-corpus benchmarks (capture/fsync latency, search percentiles, rebuild
time, and disk amplification at 10k–100k episodes), ranked-result parity
runs against the preserved v1 fixtures at scale, and a judged query set that
can ship with the repository. Scorer and confidence tuning to date used a
private judged set, so no ranking-quality claim here is reproducible by a
reader; `scripts/retrieval-eval.py` runs any judged set in that format
against any binary. Benchmark manifests should pin corpus, scorer,
configuration, clock, source revision, toolchain, and host profile.

## Explicit non-goals

AutoJournal does not include:

- memory curation, wiki maintenance, proposal review, or proposal application;
- reflection, consolidation, or model-generated durable claims;
- embeddings, vector search, graphs, rerankers, or semantic release gates;
- automatic alias promotion;
- mandatory ambient memory injection;
- automatic retention or deletion;
- an always-on daemon requirement;
- ownership of harness sessions, context, compaction, branching, providers,
  or tool execution;
- compatibility with retired prototype wire/disk formats; or
- host-specific paths, prompt heuristics, or global-memory defaults in core.

A future curator is a separate repository, process, policy, and owner
decision. It may read exported/source evidence through public AutoJournal
operations, but it receives no implicit authority to rewrite episodes or any
other knowledge store.

## Decisions deferred to implementation evidence

The design does not preselect:

- numeric no-memory thresholds;
- fixed latency, memory, or disk ceilings before a measured baseline exists;
- post-lexical semantic retrieval technology; or
- optional ambient-recall UX.

These are reversible implementation or policy choices. They cannot change
the Markdown authority, atomic episode contract, public operations, adapter
boundary, or separation from curation without a new owner-approved design
revision.

## Implementation decision records

Resolutions taken during implementation under the clause above — reversible,
and not revisions of the contract. Public operations are unchanged by all of
them.

- **Config file is JSON.** The owner config is
  `~/.config/autojournal/config.json` (closed schema, absolute paths,
  optional retrieval knobs), chosen over TOML because the closed `std.json`
  parsing was already proven for the capture contracts.
- **Index location.** The SQLite projection lives outside the journal root,
  in the XDG state directory, keyed by a digest of the root path
  (`index-<hex16>.sqlite`) so the corpus stays a clean git-trackable tree and
  distinct roots never share a projection. The full root digest is also
  stored in index metadata; an index recording another root's identity is
  rejected as foreign, never misread as an empty corpus.
- **Host-neutral default and session policy.** With no owner config or
  explicit `--root`, the core resolves the XDG data journal above. Pi's
  `/autojournal` menu persists explicit world/scope selections as
  branch-local session entries and applies them to both capture and search.
  Lane remains a closed system record type.
- **Owner default selection.** The `default` command shows or sets the
  owner's default world/scope for capture and search: an atomic owner-config
  rewrite that preserves every other key, migrates the pre-release
  `world_root` key, and refuses to touch a malformed file. Every config key
  is optional — a defaults-only config keeps the host-neutral journal root.
  Pi's menu exposes this as "Save as default for new sessions"; the
  session-local selection continues to govern the current conversation.
- **Default-eliding physical layout.** Main/default/conversation episodes
  live directly under date shards. Reserved worlds/, scopes/, and lanes/
  components appear only for non-default classifications; frontmatter
  remains authoritative for every classification.
- **Vault-tolerant sync.** Sync deduplicates by episode identity — the first
  copy encountered stays indexed, later copies are counted as
  `duplicate_ids` — skips dot-directories (`.obsidian`, `.git`), and repairs
  owner-only permissions best-effort rather than failing on foreign-owned
  entries. Deliberate exclusions are recorded in index metadata so `status`
  and search freshness compare indexed + excluded against source files;
  after manual corpus surgery, `sync` rebaselines.
- **Shared-directory refusal.** Writing commands refuse a journal root whose
  nearest existing ancestor is group- or world-writable (the sshd
  StrictModes rule): other users could interfere there, and `/tmp`-style
  locations are volatile. Ownership-aware exemptions were considered and
  rejected — they need platform-split raw stat plumbing for one edge case a
  `chmod g-w` fixes.
- **Interactive-only capture.** The Pi adapter publishes episodes only from
  interactive modes; headless runs and exec-spawned sub-agents skip capture
  (counted, visible in status) while keeping recall tools, so automation
  never writes synthetic turns into the corpus.
- **Aliases are not projected into SQLite.** The owner-edited thesaurus file
  (`thesaurus.json`, byte-compatible with the proven v1 map) is read fresh
  on every search invocation, which makes hot reload deterministic and
  removes a projection-drift surface; only its canonical SHA-256 digest is
  recorded (index metadata and every search report) as the alias identity.
  Revisit only if a long-lived embedded host measures the per-call read as
  significant.
- **Candidate discovery and df.** Discovery scans the per-world vocabulary
  for tokens containing each query term as a substring, then fetches
  postings for matched tokens; needles shorter than 3 bytes are skipped
  when longer needles exist, because a 2-byte needle floods the
  `max_vocab_matches` cap and truncates discovery for the rest of the
  query (an all-short query still scans with its short needles, so
  curated values like "q8" work alone). Document frequencies for the
  compatibility scorer are derived from the credited candidate set per
  query, reproducing v1's matched-files df exactly. `term_stats` maintains
  incremental per-world statistics with `df` (evaluation excluded) and
  `eval_df` (so explicitly requested evaluation lanes remain
  discoverable). Index-side tokens have a 2-byte floor so short curated
  alias values ("q8") stay discoverable; known accepted gaps: a term
  occurring only inside a stop word or an over-128-byte token is not
  discoverable.
- **Per-line crediting is word-start bounded.** A term credits a line only
  where an occurrence begins at a token boundary: `hang` credits `hanging`
  but no longer `changed`, while prefix recall (`config` →
  `configuration`) is preserved. Measured on a 1,445-entry legacy journal
  corpus, this removed 60–80% of credited matches for boundary-prone
  queries (`hang` 6591→1341, `lock` 5000→1211) with no loss in top-10
  relevance; full-word crediting was also evaluated and rejected because
  it dropped legitimate inflections (`configuration`, `deployment`). v1's
  infix credit (`index` finds `reindexing`) is gone by default — curate an
  alias ("index" → "reindex") where an infix family matters, or pass
  `--credit-mode substring` for v1 parity.
- **Known limitation — in-place edits.** Revision verification compares the
  frontmatter-recorded digest, so replaced or regenerated files are detected
  (`stale_revision`), but a hand edit to body text that leaves the
  frontmatter digest line untouched is not; a designed owner-edit workflow
  (validate → recompute digest → reindex) closes this.
