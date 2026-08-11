# AutoJournal design basis

**Status**: ratified 2026-08-09, as built 2026-08-10. What shipped and when is
in `CHANGELOG.md`.

This document states the **ratified design**, and the tree implements all of
it. The document is maintained as current state: chronology lives in
`CHANGELOG.md`, not here.

Read beside this document:

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — the codebase map.
- [`docs/SEARCH_TUNING.md`](docs/SEARCH_TUNING.md) — search behavior and
  thesaurus curation for owners.
- [`AGENTS.md`](AGENTS.md) — how to change this repository, and the gates a
  change runs.
- [`CHANGELOG.md`](CHANGELOG.md) — what shipped, when.

## Thesis

**AutoJournal turns completed agent turns into an owner-owned Markdown corpus
that survives the loss of every derived artifact, and never returns evidence
that misrepresents what that corpus says.**

Falsifiable, and the acceptance test is three checks a build can run:

1. **Recoverability.** Capture a corpus, run a query set that returns a
   non-empty ranked result for every query, delete the index entirely, rebuild
   from Markdown alone, and re-run. Every query must still return results, in the
   same order, with the same scores, evidence references, and source line bounds.
   Two runs that agree only by both returning nothing prove nothing and do not
   satisfy the check. Any state that cannot be regenerated from the Markdown
   falsifies the claim.
2. **Evidence honesty.** Modify an episode's body by any means, including a hand
   edit that leaves its recorded digest untouched. The product must never serve
   that text under the revision it no longer matches. Serving it as a `match`
   falsifies the claim.
3. **Durability under partial failure.** Make the index write fail during a
   capture. Source publication must still succeed, the world must report itself
   index-stale, and `sync` must repair the projection from Markdown. A capture
   that reports success without a durable file, or a failure that loses a
   published file, falsifies the claim.

Everything below exists to keep those three true.

AutoJournal has two jobs: durably write completed agent turns as episodes, and
retrieve exact, ranked, bounded evidence from them. It is an independent product
and repository, not a subsystem of any harness and not policy for one machine.

## Model and primitives

**Episode.** One completed agent turn, published as one Markdown file with
closed, versioned frontmatter and a body. The file is the product. Everything
else is derived from it and can be thrown away.

**Identity.** An episode's id is a hash over source harness, session id,
operation/turn id, world, and capture-policy version. It is deterministic, so
re-delivering the same turn addresses the same episode rather than creating a
second one. Scope and lane are deliberately outside identity: they are
classification, and an episode's identity survives reclassification.

**Capture policy.** A version stamp naming the rules an adapter used to project
a turn — what counted as the prompt, what counted as the result, what was
redacted. It participates in identity, so changing how a harness projects turns
produces new episodes rather than silently altering the meaning of old ones. A
corpus can therefore hold several vintages of the same conversation, each honest
about how it was made.

**Revision.** The payload digest: a SHA-256 over length-prefixed canonical
identity metadata plus the body, excluding capture-run metadata such as capture
time and adapter version, so a faithful re-delivery hashes identically
regardless of when or by which build it arrives. This is the revision identity
that evidence references carry, and it is derived from content rather than
trusted from the file's own claim about itself: both read paths recompute the
digest from the file's canonical content before serving anything under it.

Note what identity and revision cover differently: classification is outside
*identity* but inside the *digest*. An episode reclassified by an owner edit to
its frontmatter keeps its episode id and its place in the corpus, but its
recomputed revision changes, so outstanding evidence references to it resolve as
stale. That is the correct answer — the episode a reference was taken against is
not the episode on disk any more — but a reader of "identity survives
reclassification" should not read it as "references survive reclassification."

**World, scope, lane.** A world is an owner-controlled corpus and index. A scope
bounds a collection within it — global, workspace, project, session, delegated,
or owner-defined. A lane distinguishes normal conversation, delegated work,
evaluation, and explicitly imported legacy material. All three are explicit
inputs; core code never infers them from working directory, prompt text, or
harness name.

**Projection.** The SQLite index. It holds only rebuildable retrieval state and
is keyed to the identity of the journal root it describes, so an index belonging
to another root is rejected as foreign rather than misread as an empty corpus.

**Adapter.** The translation layer for one harness: it converts that harness's
lifecycle events into the completed-turn schema and renders diagnostics. It
holds pending-turn pairing state and nothing else. Adapters never write Markdown
directly and invent no memory policy.

**Evidence reference.** A stable pointer to an episode at a revision, with exact
source line bounds. Search returns references and bounded snippets; opening a
reference returns exact source text with current provenance.

## Doctrine

Invariants with dated rationale. These carry into any rewrite as requirements.

1. **Markdown is authority.** (2026-07-29) Human-readable episode files are the
   recoverable source of truth. A memory system whose contents are only legible
   through its own binary has made the owner a tenant of their own history.
2. **Indexes are disposable.** (2026-07-29) SQLite and any later derived
   representation can be deleted and rebuilt from Markdown. This is what makes
   schema changes cheap and corruption survivable.
3. **One completed turn is one atomic episode**, and an episode's bytes change
   only when a redelivery of its own identity is proven to contain them.
   (2026-07-29; amended 2026-08-09) Writes never append a half-turn to a shared
   session file, which gives capture a single atomic publication boundary,
   permits concurrent writers without shared append locks, makes duplicate
   delivery idempotent, and gives deletion and evidence references episode
   granularity. The 2026-08-09 amendment admits one narrow exception: a turn
   captured before its content had settled can be completed rather than frozen
   wrong. See *Supersede and the curation boundary*.
4. **Capture cannot break the agent turn.** (2026-07-29) Failure is visible and
   retryable, but never converts a settled harness operation into a failure. A
   memory subsystem that can fail the work it was recording will be switched off
   the first time it does, and a memory product nobody leaves enabled records
   nothing.
5. **Search is not evidence opening.** (2026-07-29) Search returns ranked
   references and bounded snippets; opening a reference returns exact source
   evidence. Conflating them makes every query an unbounded read.
6. **No match is a real result.** (2026-07-29) Empty, stale, unavailable,
   malformed, and timed-out memory are distinct typed outcomes. A memory system
   that cannot say "nothing" says something wrong instead.
7. **Recalled text is untrusted evidence.** (2026-07-29) It carries source and
   revision provenance and is never presented as instruction. The corpus records
   whatever was said in past turns, including text an agent was asked to repeat
   or quote; anything that reaches a model as instruction rather than as cited
   evidence turns the memory store into an injection surface against its own
   owner.
8. **Scope is explicit.** (2026-07-29) Core code does not infer a world, scope,
   lane, or privacy policy from working directory, prompts, or harness names.
   Inference here is unauditable and fails silently.
9. **Pull first.** (2026-07-29) Agent-invoked recall is the default. Ambient
   injection is never silently enabled. Injected memory arrives without the
   query that asked for it, so neither the owner nor the model can tell which
   part of an answer came from the corpus and which from the current turn —
   which is doctrine 7's problem arriving by a different door. A pulled result
   is always attributable to a request.
10. **No generated durable claims.** (2026-07-29) Model-written summaries,
    aliases, or reflections do not enter the authoritative episode corpus
    automatically. A corpus that mixes what was said with what a model concluded
    about what was said cannot be audited by reading it, and the audit is the
    whole reason the files are text.
11. **Evidence is verified against content, not against its own assertion.**
    (2026-08-09) The revision a
    reference carries is recomputed from the file's canonical content before that
    content is served. A file's claim about its own digest is a convenience,
    never the check. The prior design trusted the recorded line, which meant a
    hand-edited body was served under an unedited revision — the one failure mode
    this product's handoff rule calls worse than an open bug. The one sanctioned
    way a recorded digest changes without a re-capture is the owner invoking
    `reseal` on their own edit; nothing else rewrites it, and no agent or adapter
    can.
12. **One health signal has one meaning.** (2026-08-09) Where two operations
    report the same named property, they compute it the same way. Search
    freshness and status freshness previously disagreed by construction, so an
    adapter surfacing one told the owner memory was healthy while the other
    reported it stale.

### Supersede and the curation boundary

Re-delivering a known identity with different content is a conflict, and the
first publication wins. That is the right default: two genuinely different turns
claiming one identity is exactly the situation a store should refuse to resolve
on its own.

One exception, added 2026-08-09. Where the stored content is provably contained
in the incoming content — a strict prefix, or otherwise strictly contained — the
two are the same turn at different stages of completion, not a disagreement. The
store publishes the fuller revision at the episode's own path, reports the
outcome distinctly, and does not retain the superseded bytes. Outstanding
references to the previous revision resolve as stale, which is already the
correct answer for a file that was replaced.

The boundary this draws, stated explicitly because it is easy to erode: the
store resolves a **mechanical, provable relationship between two byte
sequences**. It never judges which version is better, more relevant, or more
correct. Where containment does not hold, or the relationship is not detectable,
conflict stands. Length is not evidence of sameness; recency is not evidence of
quality. The moment the store starts ranking versions on merit it has become a
curator, which is a different product.

The cost is recorded rather than argued away: superseded bytes leave the corpus.
An owner who needs them relies on their own version control or backups, which is
practical precisely because the corpus is a plain directory of text files.

### What is frozen, and in which tier

Three tiers, not one list. Conflating them made routine additions look like
major-version events.

**Corpus-durable — changing these is a major version.** Episode Markdown and
frontmatter bytes; episode-id derivation; payload-digest derivation. These are
what make an existing corpus readable and addressable by a future binary, and
they are pinned by golden fixtures in both directions.

**Interface — additive changes are minor.** The CLI `--json` surface and the
owner config file. New fields, new config keys, and new values within an
existing typed vocabulary are minor. Removals, renames, and changes to the
meaning of an existing field or key are major. **Consumers must tolerate unknown
fields and unknown values of a typed vocabulary.** A consumer that treats an
unrecognized outcome as an error is defective, not protected — a typed
vocabulary that cannot grow forces every clarification into a major version.

**Derived — not a contract.** The SQLite projection, its schema, and its file
location. A version bump disposes and rebuilds from Markdown, which is precisely
what the projection is designed to survive. Freezing it would contradict
doctrine 2.

Frontmatter reading is tolerant of unknown keys, so an episode written by a
newer build remains readable by an older one.

## Expected behaviors

### Capture

Capture is an adapter operation — the binary's `capture` command — and is not
an agent-facing tool. This is the write-authority boundary the curation non-goal
rests on: an agent can read memory through `memory_search` and `memory_get` and
has no path to write it. Only a harness adapter, translating a turn its host has
already settled, publishes an episode.

An adapter delivers a closed completed-turn payload. AutoJournal validates it,
derives identity, path, and canonical digest, asks the projection whether the
episode already exists anywhere in the corpus, and classifies any existing file
against its own frontmatter rather than against the index's opinion. It then
creates owner-only sharded directories through contained paths, writes to a
temporary file in the target directory, syncs it, publishes with atomic
no-replace semantics, syncs the directory, and updates the projection in one
transaction. Success is reported only after source publication; index freshness
is reported independently.

Outcomes are `published`, `duplicate`, `superseded`, `conflict`, `malformed`,
`permission_denied`, `unavailable`, and `internal_error`. The first three are
success.

If publication succeeds and the projection update fails, capture remains
successful, the world becomes visibly index-stale, and `sync` repairs it. No
source episode is deleted or rewritten because indexing failed. A harness turn
that already settled stays settled.

Ordinary failure paths remove their temporary file. A process killed mid-
publication can leave a dot-prefixed temporary file behind; corpus scans ignore
it, so an interrupted capture costs a stray hidden file and never a partial
episode.

### Physical layout

```text
<root>/YYYY/MM/DD/<episode-id>.md
<root>/scopes/<scope>/YYYY/MM/DD/<episode-id>.md
<root>/worlds/<world>/YYYY/MM/DD/<episode-id>.md
<root>/worlds/<world>/scopes/<scope>/lanes/<lane>/YYYY/MM/DD/<episode-id>.md
```

Main/default/conversation episodes live directly under date shards, so the root
is usable as an ordinary vault or editor tree. Reserved components appear only
for non-default classifications. Frontmatter, not the path, is authoritative for
every classification: sync trusts what a file says, not where it sits.

### Episode content

Frontmatter carries schema version, episode id, world, lane, scope, source
harness and adapter version, session and turn ids, source event time and capture
time, capture policy, terminal outcome, canonical payload digest, and optional
parent/delegation and branch provenance.

The body carries the original user content, the terminal assistant result, and
an optional structured list of tool names with allowlisted metadata, plus
explicit markers where policy removed content. Where one owner prompt produced
more than one terminal assistant response — a delegation handoff followed by the
delegated result, for instance — the body carries each in order. Progress prose
and tool-call summaries are not results and are excluded. Shell command bodies,
search queries, credentials, arbitrary tool arguments, hidden reasoning, and raw
provider metadata are excluded by default. Adapters may redact more strictly but
must identify the policy they used. Prompt inspection never selects capture
lanes or privacy behavior.

Delegated work may use a compact body carrying the assigned task and terminal
child result with parent/run/child identities. Evaluation episodes use an
explicit evaluation lane and are excluded from ordinary recall and corpus
statistics.

### Retrieval

`memory_search` takes world, scope, lanes, query, limit, cursor, and a credit
mode. It returns a typed outcome, ranked stable
evidence references, bounded snippets with exact source line bounds, score and
confidence, classification and capture-policy provenance, scorer/tokenizer/alias
identities, and index freshness. It never returns an unbounded body.

`memory_get` opens one reference with explicit line bounds, validates episode id
and revision against content, and returns exact source evidence with current
provenance. Edited evidence returns
`stale_revision`; deleted evidence returns `gone`.

The retrieval outcome vocabulary is closed: `match`, `no_match`,
`stale_revision`, `gone`, `index_stale`, `timeout`, `unavailable`,
`permission_denied`, `malformed`, `conflict`, and `internal_error`. It is stated
here in full because the Interface tier's rule — new values within an existing
typed vocabulary are a minor change — is unusable against a vocabulary the basis
does not enumerate.

Retrieval is lexical throughout: lowercase/punctuation/stop-word tokenization,
exact-query duplicate term weights (unconditionally — alias values and folded
variants are appended to the query's term list, never substituted for it),
manually curated deterministic aliases,
`sum(log(N/df))` rarity scoring, day-quantized recency as a nudge rather than an
override, frontmatter exclusion, episode and source-span deduplication,
deterministic stable tie-breaking, deterministic vocabulary discovery order
(the substring scan iterates the vocabulary in sorted term order, so the
discovery cap truncates a stable, defined prefix rather than whatever subset a
scan happened to reach first), and exact provenance. SQLite discovers
candidates; a versioned AutoJournal scorer owns final rank. Generic FTS/BM25
rank is not the final scorer — an earlier prototype was faster and lost judged
evidence.

Per-line crediting is word-start bounded: a term credits a line only where an
occurrence begins at a token boundary, so `hang` credits `hanging` but not
`changed`, while prefix recall such as `config` → `configuration` is preserved.
Full-word crediting was evaluated and rejected because it dropped legitimate
inflections. The earlier substring crediting that found `reindexing` from
`index` remains available per query as an explicit credit-mode option, so the
prior behavior is a choice rather than a loss.

Query rewriting by a model is excluded, and not only on dependency grounds: it
was tried and it displaced exact answers, promoting a paraphrase of the question
over the episode that actually answered it. Weak-query logging is the sanctioned
alternative — opt-in, owner-private, bounded in size, and never writing an alias
on its own. Alias promotion stays manual.

A lexical occurrence is not automatically relevant memory. A versioned
confidence policy with an owner-tunable floor is reported separately from score,
so noise queries return `no_match` rather than ten weak results. The version
exists so a recalibration is a visible identity change rather than a silent
behavior shift, and it has been used that way once.

### Owner control

Markdown stays owner-controlled, and two kinds of edit behave differently.

An edit that produces a well-formed episode carrying a correct digest for its
new content — replacing or regenerating the file — is absorbed: `sync` reindexes
it, search reflects it, and references to the previous revision resolve as
`stale_revision`. An edit to body text that leaves the recorded digest line
untouched is a different case. The recomputed revision no longer matches the
recorded one, so the episode is excluded from search as `edited_excluded`,
`memory_get` returns `stale_revision` against it, and `sync` reports it in the
digest-mismatch count — in its text form and in `sync --json`, which carries
the same accounting for a host that wants it machine-readable. Sync cannot
clear that state: it re-reads the same recorded line the recomputation already
disagreed with.

Exclusion is the correct immediate answer — serving edited text under an
unedited revision is the failure doctrine 11 exists to prevent — but it is not
the end state. Editing one's own files is a supported thing to do, so the
product provides the way back.

**`reseal`** sweeps the corpus for episodes whose recomputed digest disagrees
with their recorded line, revalidates each against the episode schema, rewrites
the recorded digest to match the file's actual content, and reports what it
resealed. An episode that no longer parses as a valid episode is refused and
reported, never resealed — reseal re-attests a well-formed edit, it does not
repair a broken file. Resealing changes the episode's revision, so outstanding
evidence references to it resolve as stale afterwards, which is the honest
result: the text those references were taken against is not the text on disk.

Reseal is an owner operation. No adapter and no agent can invoke it — it writes
to the corpus, and the write-authority boundary that keeps agents out of capture
keeps them out of this. It acts by default, because the owner ran it on purpose
and `sync`'s digest-mismatch count already tells them how many files are
affected before they do; a preview flag lists the affected paths without
touching them. Sweeping the whole corpus is the intended shape: one command
takes a corpus with any number of hand edits back to a fully sealed state.

Moving a file within a world preserves episode identity after sync. Duplicate
episode ids are deduplicated with the first copy found staying served and later
copies counted; malformed frontmatter is excluded with visible diagnostics;
files are never merged by filename. A missing episode is removed from the
projection on sync, and a prior reference to it returns `gone`. AutoJournal
performs no age-based or automatic deletion — the owner deletes files directly
and rebaselines with `sync`, and no bulk-delete command ships. SQLite is never
required for recovery: the corpus is the export.

Owner operations are `status`, `catalog`, `sync`, `reseal`, `default`, and
`alias` maintenance, reporting source and index health separately. A harness
adapter that offers a memory menu exposes the same set, including reseal.
No `rebuild`, `inventory`, `export`, or bulk-deletion command ships. The
repository additionally carries `scripts/corpus-junk-report.py`, a report-only
owner script that scores captured sessions by how machine-shaped their content
is; it recommends and never performs deletion, so the no-bulk-deletion rule is a
rule about what the product does to a corpus, not about what an owner may be
helped to see. `scripts/repair-corpus.py` sits on the same side of that line:
report-only by default, it replays episodes captured under `cc-stop.v3` through
the v4 projection and shows what it would publish and delete; with `--apply` it
publishes each replacement through `autojournal capture` and deletes only the
v3 file whose replacement published successfully — an owner-invoked repair with
the owner reading the report first, not a product deletion path.

Freshness has one meaning wherever it is reported: an authoritative check of
the corpus against the projection, short-circuited by a memoized corpus
signature so an unchanged corpus does not pay for the walk — the memo and its
guards are described under Structure. `status` and search report the same
signal, which is what doctrine 12 requires. `sync` reports malformed
exclusions, duplicate ids, and files whose recomputed digest disagrees with
their recorded line as distinct counts, so a hand-edited episode is a visible,
located problem rather than a silent permanent exclusion.

### Configuration and defaults

Configuration belongs to AutoJournal, not to each harness. One owner config file
under the XDG config directory holds journal root, retrieval knobs, and capture
defaults; every key is optional. A fresh install defaults to a host-neutral
journal root under the XDG data directory, so every conforming harness under one
user account shares a corpus with no setup. Owner configuration always wins.
Harness wiring invents no policy, though a first-class host may transport an
explicit owner-selected session world and scope.

Source directories, episode files, indexes, and helper state are owner-only.
Reads and writes reject symlink escapes and paths outside the configured root,
and writing commands refuse a root whose nearest existing ancestor is group- or
world-writable.

### Integration surfaces

One implementation of every product rule, exposed in three forms. Dependency
direction is always host → AutoJournal; AutoJournal never depends on a host.

**As an imported Go module.** An embedding host links the core natively, so
AutoJournal does not depend on any host's scripted extension API. Such a host
provides three capabilities, each generic rather than an AutoJournal-private
hook:

1. **Completed-turn projection.** After durable settlement, a droppable seam
   supplies the original user content, the terminal assistant result or results,
   source wall-clock timestamps, outcome, session and operation identities,
   harness identity, and branch provenance where present. The adapter combines
   this neutral projection with explicit world/scope/lane configuration; the host
   invents no memory policy.
2. **Memory tools in the ordinary tool lifecycle.** `memory_search` and
   `memory_get` register through the host's normal tool path — provider
   advertisement, call-id validation, limits, middleware, execution, and durable
   tool-result recording.
3. **Observable nonfatal failures.** Failed captures, index lag, and repair
   requirements are counted and visible through status and diagnostics without
   failing the settled operation.

Capture handler failure is droppable and observable; recall tool failure is a
normal typed tool result; ambient projection injection is disabled. The
integration can be turned off by owner configuration without changing session
storage or host behavior. No embedding-host integration ships in this
repository.

**As a standalone static binary.** One executable that is simultaneously the
owner CLI, the hook target, and the versioned helper other adapters supervise.
The current adapter transport is the CLI's `--json` surface, invoked as a
process per operation; no long-lived framed-stdio transport ships and no daemon
is required.

**Through a thin harness adapter.** Adapters translate their lifecycle events
into the same completed-turn schema, register or describe the compatible tools,
and render diagnostics. Pending-turn pairing is adapter state. They do not write
Markdown directly and invent no memory policy: world and scope come from
AutoJournal's own configuration, except that a first-class host may transport an
explicit owner-selected session world and scope. Lane is an explicit payload
field the adapter supplies and the library validates against a closed set — it
states what kind of work the turn was, which only the adapter is in a position to
know. No other policy is the adapter's.

Headless runs and exec-spawned sub-agents are synthetic work products, and
capturing them fills the corpus with generated data. Adapters gate on this to the
extent their host makes it detectable, and they differ today: the Pi adapter
publishes only from interactive and RPC sessions and counts the skips in its own
diagnostics, and skips sub-agent sessions outright when importing history, while
the two hook adapters publish whenever their completed-turn hook fires, because
their hosts' hook payloads carry no mode. Recall stays available in every mode.

**Legacy import.** Pre-AutoJournal session Markdown is published through the
normal capture operation into an explicitly marked legacy lane. Each imported
turn receives a deterministic synthesized identity and revision digest, so
repeated imports deduplicate, and source files are never rewritten. A harness
adapter may offer an equivalent importer for its own session history, preserving
original event times and skipping subagent logs.

### Reversible implementation decisions

Current behavior that could change without touching the thesis, the doctrine, or
any frozen tier. Recorded with reasons so a future change is a decision rather
than a rediscovery.

- **The owner config is JSON**, a closed schema with absolute paths and optional
  retrieval knobs — chosen over TOML to reuse the existing closed JSON contract
  shape and standard-library parsing.
- **The projection lives outside the journal root**, in the XDG state directory,
  named by a digest of the canonicalized root path, so the corpus stays a clean
  version-controllable tree, distinct roots never share an index, and two
  spellings of one root share one. The full
  root digest is stored in index metadata; an index recording another root is
  rejected as foreign rather than misread as empty.
- **Sync is vault-tolerant.** It deduplicates by episode identity, skips
  dot-directories so an editor's or version control's metadata is ignored, and
  repairs owner-only permissions best-effort rather than failing on
  foreign-owned entries. Deliberate exclusions are recorded in index metadata so
  freshness compares indexed plus excluded against source files, and sync
  rebaselines after manual corpus surgery.
- **Repair is a single transaction.** `sync` re-walks the corpus and rebuilds the
  projection in place, so a torn rebuild rolls back rather than leaving a half
  projection. No generation-switching rebuild command ships. A missing, corrupt,
  wrong-root, wrong-schema, or wrong-configuration index is never interpreted as
  an empty corpus.
- **Writing commands refuse a group- or world-writable ancestor**, the same rule
  an SSH daemon applies to key files: other users could interfere there, and
  temp-style locations are volatile. Ownership-aware exemptions were considered
  and rejected as platform-split plumbing for one edge case a permission change
  fixes.
- **A malformed thesaurus degrades recall, it never fails it.** An unreadable or
  unparseable file is a valid empty configuration. Duplicate keys, and keys
  differing only by case, are merged rather than refused: alias values are
  additive by construction, so the union of two entries for one key is exactly
  what a single entry carrying both values would mean. This is the one place the
  product resolves a conflicting owner edit rather than refusing it, and it does
  so because there is no conflict to resolve — unlike the config, where two
  values for one key are mutually exclusive and the product must not guess.
  `alias list` reports what it merged, and `alias add`/`alias remove` collapse
  the duplicates on disk as a side effect of their atomic rewrite.
- **Aliases are not projected into SQLite.** The owner-edited thesaurus is read
  fresh on every search, which makes hot reload deterministic and removes a
  projection-drift surface; only its canonical digest is recorded in each search
  report as the alias identity. Worth revisiting only if a long-lived embedded
  host measures the per-call read as significant.
- **Document frequencies come from the credited candidate set per query**,
  reproducing the proven v1 behavior exactly, while per-world statistics are
  maintained incrementally with evaluation-lane counts kept separate so an
  explicitly requested evaluation lane stays discoverable. Index-side tokens
  have a two-byte floor so short curated alias values remain reachable; a term
  occurring only inside a stop word or an over-long token is a known, accepted
  gap. On the query side the rule is different and needs to be: a needle shorter
  than three bytes is skipped when the query also carries longer ones, because a
  two-byte needle matches most of the vocabulary and would exhaust the discovery
  cap before the discriminating terms were tried. A query made only of short
  needles still runs with them, so a curated two-character alias value works
  when searched alone.
- **The SQLite driver is pure Go, statically linked, and the only direct
  dependency** — `modernc.org/sqlite`, named here because a reader rebuilding
  this behavior from the external artifacts needs to know which one. No cgo, no
  system SQLite, no dynamic module loading. This is what
  makes "one static binary that works offline" true rather than aspirational, and
  it is why the dependency surface is kept deliberately narrow rather than
  merely tidy.
- **An adapter gates capture on session mode only where its host exposes one.**
  Where the signal exists it is used; where it does not, the adapter captures
  whatever its completed-turn hook delivers rather than guessing from the
  environment. The failure costs are asymmetric and decide it: a wrong skip loses
  a turn permanently and invisibly, since nothing else records it and the owner
  has no way to notice the absence, while a wrong capture writes one deletable
  file into a corpus the owner already controls. Buying protection against the
  recoverable failure by risking the unrecoverable one is the wrong trade. This
  reverses cheaply if a host starts reporting its mode.
- **Concurrency serializes through SQLite**, with write-ahead logging and a
  bounded busy timeout and no separate advisory-lock layer. Source publication
  never waits indefinitely on an index writer, and AutoJournal adds no
  application retry loop: multiple processes may publish unique episodes
  concurrently, and a busy or failed index update leaves a known-stale
  projection that `sync` repairs.

## Deliberately not in scope

Each of these is a live temptation with a reason it stays out.

**Memory curation, wiki maintenance, proposal review and application.** A
curator is a separate repository, process, policy, and owner decision. It may
read evidence through public AutoJournal operations; it receives no implicit
authority to rewrite episodes or any other knowledge store. Merging the two
would give a component that generates claims the same write authority as the
component that records what actually happened.

**Reflection, consolidation, and model-generated durable claims.** See doctrine
10.

**Embeddings, vector search, graphs, rerankers, and semantic release gates.**
The lexical ceiling is real and accepted: exact-substring retrieval cannot find
an episode that discussed the same idea in different words, and the thesaurus
exists to compensate. The rejection stands because the alternative puts a model
dependency, and usually a network call or a weights file, inside a product whose
properties are that it has none of those — it is one static binary, it works
offline, and its behavior is reproducible from its inputs. The investment goes
into making the existing compensation cheaper instead: ranked alias suggestions
derived from the miss log rather than raw query dumps, morphological folding
beyond today's plurals-only handling, and alias values learned from
co-occurrence in episodes the owner did eventually find. Reconsidering this
means reconsidering the thesis, not adding a feature.

**Automatic alias promotion.** Aliases are the one place an owner's judgment
enters ranking. Promoting them automatically from usage would make ranking drift
without anyone deciding it should.

**Mandatory ambient memory injection.** See doctrine 9.

**Automatic retention or deletion.** Deleting someone's history on a schedule is
not a feature a memory product gets to have by default.

**An always-on daemon requirement.** The binary is invoked per operation. A
mandatory daemon would add a failure mode between the owner and their files.

**Ownership of harness sessions, context, compaction, branching, providers, or
tool execution.** Those belong to the host. AutoJournal receives an explicit
completed-turn projection after durable settlement and never mutates host
context or treats a compaction summary as authoritative source memory.

**Compatibility with retired prototype formats**, and **host-specific paths,
prompt heuristics, or global-memory defaults in core.**

### Intentionally open

Not frozen, and reversible without an owner-approved design revision: the
owner-tunable no-memory threshold values; fixed latency, memory, or disk
ceilings before a measured baseline exists; and optional ambient-recall
presentation. None of these may change Markdown authority, the atomic episode
contract, the public operations, the adapter boundary, or the separation from
curation.

## Verification spine

The durable assets of this project are the externalized, executable description
of its behavior. Implementation code is a projection of those assets and can be
regenerated; the assets cannot. The question asked at every review is whether,
if the tree vanished tonight, the external artifacts would suffice to rebuild
the behavior. A "no" is a finding at the same severity as a failing gate.

**The fixtures are the authority, and there is no second one.** (2026-08-09)
This product previously treated an earlier implementation, from which the
fixtures were originally frozen, as the tiebreaker for behavioral questions the
tests did not answer. That arrangement has been retired. Where the fixtures and
the tests are silent, the answer is a design decision to be made and then
pinned — not a lookup against a prior implementation whose own choices were
never separately justified. Compatibility with a predecessor is no longer a
reason for a behavior to exist; several accepted defects were faithful
reproductions of one, and defending them cost more than the compatibility was
worth. The consequence is deliberate: settling an undetermined behavior now
requires deciding what is correct and adding a fixture, which is slower per
question and leaves the answer written down.

**Golden fixtures.** `testdata/payloads` and `testdata/golden` pin episode
bytes, identity and digest vectors, publish paths, config rewrites, and CLI
`--json` samples, checked in both directions. The matrix is extended whenever
CLI-observable behavior changes and never weakened. These are the corpus-durable
tier's enforcement.

**Repository gate.** `scripts/verify.sh` runs formatting, vetting, race-enabled
unit and fault-path tests across publication, indexing, dedupe, corruption, and
containment including symlink rejection and owner-only permissions; a host
binary build; the adapter typecheck and test suite against that binary; the
Python hook suite; design-contract greps against this document; and an
end-to-end capture → search → alias rescue → evidence opening → stale revision →
typed no-match smoke in an isolated root, plus fresh-install defaults, journal
relocation, owner-default selection, vault dedupe, and shared-directory refusal.
`scripts/release-check.sh` adds cross-compilation, npm tarball inspection, and
version identity cross-checks. CI runs both scripts' contents plus a
multi-target binary end-to-end matrix, so one local command reproduces
everything a single machine can do.

**Cross-adapter conformance.** Every adapter implements the payload contract's
token and origin-host rules, and the two hook-based adapters additionally
implement a workspace-root rule. `adapters/conformance_cases.json` is the
shared edge-case corpus, and `adapters/test_conformance.py` feeds every case to
every implementation of its rule — the two Python hooks directly, the
TypeScript Pi adapter through a generated Node harness — and asserts the same
accept/reject decision everywhere, so a change to a payload-contract rule
cannot desynchronize one adapter silently. The corpus pins which adapter
carries which rule, so shrinking coverage takes two deliberate edits.

**Capture correctness against real transcripts.** A change to how a harness
projects turns is not done until it has been replayed against recorded real
transcripts and the projection checked, not only unit tested over synthetic
shapes. `testdata/transcripts/` holds redacted recordings of real Claude Code
sessions — structure preserved, every content word replaced from a placeholder
alphabet — and `EXPECTED.json` pins each recording's projection; the hook suite
replays every fixture on every run. This requirement is written from evidence:
the projection defects found before it existed were invisible to synthetic
fixtures precisely because the fixture author and the projection author shared
one wrong assumption about transcript shape, and a synthetic corpus can only
encode assumptions someone already had.

**Parse-boundary fuzzing.** Five functions turn bytes this product did not
produce into structured values: the payload parser, the config parser, the
episode parser, the alias-map loader, and the cursor decoder. Each is a fuzz
target in `src/fuzz_test.go`, seeded from the existing fixtures plus one named
regression seed per defect found at that boundary, so a known bug class cannot
silently return. The assertions are invariants, not absence of panic — round-trip
identity for anything that parses, derived paths that stay inside the corpus
root, byte-identical config re-emission, and a cursor that refuses inputs it was
not minted against. This distinction is the requirement, not a refinement of it:
every defect found at these boundaries before the harness existed parsed cleanly
and returned a wrong value, so a crash-only harness would have caught none of
them. The repository gate runs each target for a bounded time so that "green"
stays a fixed-cost command; a weekly CI job runs the same five targets longer.

**Ranked-behavior fixtures.** `testdata/ranking/` pins scorer behavior a reader
can reproduce from the repository alone — its duplicate-weight case is the one
in-tree witness to how repeated query terms and alias expansions credit a
score, precisely because the judged set below does not ship.

**Ranking parity.** Changes that claim not to move ranking are gated on proving
it: the judged query set is run before and after, and any movement in a single
result is treated as a defect in the change rather than a tuning outcome. Scorer
and confidence tuning were ratified against a judged query set that does not
ship, because it quotes private corpus content. **No ranking-quality number in
this repository is reproducible by an external reader**, and no claim here
depends on one. `scripts/retrieval-eval.py` runs any judged set in that format
against any binary, so a reader can build their own. Its `--ranking` mode
emits, per query — negatives included — the ordered `(episode_id, revision,
line, score)` list exactly as the search returned it, plus the term
provenance (`query_terms`, `alias_terms`, `folded_terms`) that classifies a
movement; parity between two runs is a diff of that output, and a query has
moved when its ordered `(episode_id, line)` sequence changes. The harness
carries `--self-test` for the two properties a parity diff depends on:
reordered hits change the block, a snippet-only change does not.

**Not present**, and stated so a reader does not assume otherwise: large-corpus
benchmarks for capture latency, search percentiles, rebuild time, and disk
amplification at ten thousand to a hundred thousand episodes; ranked-result
parity runs at that scale; and a publishable judged query set. A reproducible
benchmark manifest would pin corpus, scorer, configuration, clock, source
revision, toolchain, and host profile.

## Structure

The capability sequence the implementation is organized around, in dependency
order. Direction may not change: contracts ← store / index / retrieval ← search
← CLI wiring, with adapters outside all of it. These are capabilities, not a file
map: several are implemented where the operations that need them live rather than
in a module of their own. The core is one Go package, so nothing in the import
graph enforces the direction — it is maintained by review, which makes checking
it a reviewer's job rather than a guarantee to assume.

1. **Contracts** — closed payload and query schemas, the typed vocabularies,
   validation. Implemented in `contracts.go`. Depends on nothing.
2. **Identity and rendering** — episode id, payload digest, frontmatter and body
   rendering, frontmatter parsing. Implemented in `identity.go`, `render.go`,
   and `episode.go`. The corpus-durable tier lives here.
3. **Paths and containment** — root resolution, sharded layout, symlink
   rejection, owner-only permissions, shared-directory refusal, and containment
   of every read and write to the configured root. Implemented in `paths.go`
   (where things live) and `corpus.go` (how the tree is safely entered).
4. **Configuration** — owner config parsing and atomic rewriting, defaults
   resolution, and the shared version stamp. Implemented in `config.go` (stamp
   in `doc.go`). Depends on paths and on nothing
   above it; everything above consumes it. The Interface tier's second contract
   lives here.
5. **Store** — the capture transaction: classify, publish atomically, sync, and
   report. Owns the supersede decision. Implemented in `store.go`. The
   transaction is a library entry point, `Capture`, so every consumer runs it
   rather than reassembling it. Its step order is part of the contract — an
   embedding host that reordered it would report different failures for the
   same input: owner-default world/scope fill, then `Validate`, then root
   canonicalization, then the shared-directory refusal (decided before the
   root is opened, so a refused root is never created), then opening the root,
   then the index open with the foreign-root gate, then the corpus-wide
   redelivery classification, then atomic publication, then the index update
   and hardening. An index failure downgrades the reported freshness and never
   changes the outcome.
6. **Index** — the disposable SQLite projection: schema, episode and posting
   maintenance, corpus freshness, foreign-index rejection, dispose-and-rebuild.
   Implemented in `index.go` over the connection layer in `db.go`.
   Freshness is memoized in index meta beside the stat-only corpus signature
   that produced it — episode count and newest episode mtime in milliseconds —
   and a call reuses the stored verdict only when the current signature matches
   the stored one exactly; otherwise the authoritative content check runs and
   re-stamps verdict and signature in one transaction. Two guards make the memo
   safe rather than merely fast: a verdict is neither reused nor written while
   the newest mtime is not strictly older than the current millisecond, since a
   same-millisecond write would leave the signature unchanged; and every
   projection write elsewhere either changes the signature (capture, supersede)
   or clears the memo in its own transaction (sync). The residual window is
   the class of owner actions that preserve both signature halves at once —
   reachably, a plain `mv` of an episode between shard directories — which
   serve a memoized verdict until the next capture or sync moves the
   signature; per-episode digest verification on the read path bounds the
   damage to a visible exclusion, never wrong content. The memo write itself
   is best-effort and bounded — a read-only projection computes without
   memoizing, and a busy one costs the caller a stamp budget of milliseconds,
   not the connection's full busy timeout. A rebuilder that recomputed the
   verdict on every call would satisfy the agreement gate but is not this
   design.
7. **Retrieval** — tokenization, aliases, scoring, confidence, cursors,
   pagination. Implemented in `retrieval.go` and `aliases.go`.
8. **Search** — candidate discovery, per-line crediting, snippet rendering,
   revision verification, result assembly. Implemented in `search.go`.
9. **Operations** — status, catalog, sync, reseal, default, alias maintenance.
   Implemented in `ops.go` and `ops_alias.go` (alias maintenance and the miss
   log). The `default` operation is deliberately the configuration capability's
   atomic rewrite with no separate operations module: a wrapper would be a name
   with no behavior.
10. **CLI wiring** — argument handling, the `--json` surface, exit codes.
    Implemented in `src/cmd/autojournal/`, with argument handling split from
    rendering.
11. **Adapters** — one per harness, translation only. A change that would put a
    product rule in an adapter belongs in the core instead.

The tree is expected to match this shape. Where it does not, either this
document is stale or the build drifted, and which one it is matters — the
mismatch is a finding, not a formatting nit.

## Provenance

- **2026-08-09 — full-codebase adversarial audit and re-centering.** An
  independent audit of the Go core, the three adapters, and the gate produced
  eleven findings and eleven forward proposals, all dispositioned by the owner,
  followed by a second pass that re-examined every behavior justified by
  compatibility with the predecessor implementation. The rulings from both are
  folded into this document. The dated working records — the ratification items,
  the delta against the previous basis, and the compatibility analysis that
  produced the three-tier freeze — are kept with the project rather than
  published, because they quote captured corpus content. Nothing in this
  document depends on reading them.
- **2026-08-03 — 1.0 release.** The core reached 1.0 as a Go implementation,
  ported from an earlier systems-language build with every on-disk format,
  identity rule, and CLI contract frozen across the move. The golden fixtures
  date from that freeze and now stand on their own; the predecessor is history,
  not an authority (see *Verification spine*).
- **Prior implementations.** A TypeScript extension proved the product — one
  Markdown episode per completed turn, lexical rarity scoring, hand-curated
  aliases, pull-based recall — and proved its failure modes: always-fire noisy
  results, infix over-crediting, and policy scattered through harness-side code.
  A Rust rewrite was started and stopped; its documents survive as fixture
  provenance only, and nothing here maintains compatibility with its formats. A
  split design placing a scripted policy layer above a compiled substrate was
  drafted and retired once an embedded host could link the core natively — the
  second runtime bought nothing but a versioned cross-language contract to
  maintain in two places.

The result is the single-package design above: one implementation of storage,
identity, ranking, and freshness, consumed either as an imported module or as a
standalone static binary. Product-rule changes require recompilation, which is
an accepted cost for a product whose scoring policy is settled evidence rather
than an iteration surface.
