# consultation-footprint

Date: 2026-09-03   Status: building

## Problem

The estate's steering artifacts (skills, wiki notes, ADRs, reference packs)
are read by models through the `read` tool, through `cat` and `sed` in bash,
through delegated children, and through journal recall. None of that reaches
the journal: the episode's `## Tools` section records tool names only, so a
model-invoked skill leaves no trace and only `/skill:name` produces a marker.
The introspect sweep of 2026-09-03 read the adr skill as never invoked while
33 episodes wrote ADRs in its exact template. Consultation is the signal the
sweep needs and the corpus cannot supply it.

## Outcome

Every pi turn captured after this ships carries a `## Files` section listing
the corpus files the turn consulted, and every past pi turn with a surviving
session log is re-rendered to carry one. The test: a fixture turn that reads
`skills/adr/SKILL.md`, `cat`s a ledger, opens one `memory_get` reference,
fetches a URL, and delegates a child that reads a wiki note renders exactly
the expected `## Files` lines, round-trips through parse and digest
verification, and `introspect-scan` over that episode reports one adr
invocation and one note touch with zero mentions. After the backfill,
`introspect-scan` over the live corpus shows the adr skill with nonzero
invocations in August 2026, and every existing episode without consultation
data keeps its bytes, id, and digest unchanged (the golden pins prove it).

## Non-goals

- **Write and edit targets.** Owner ruling 2026-09-03: the question is how
  often corpus files are used, not how often an edit pass touches them.
  Curation stays inferred from prose mentions.
- **Bash command strings, or non-`.md` bash tokens.** The 2026-08-31 ruling
  keeps command strings out as a secret-exposure surface; the `.md` token
  extraction is the whole bash carve-out.
- **Query strings** from `memory_search` and `web_search`. Content, not
  paths; the miss log already aggregates weak queries.
- **Tool contents and thinking text.** Unchanged from the 2026-08-31 ruling.
- **A new episode schema tag or digest algorithm.** `aj-episode.v1` and
  `autojournal-digest.v1` stay; the section is additive (ADR 0003).
- **The Claude Code adapter.** Retired (ADR 0001); its legacy episodes are
  untouched by the backfill.
- **Wholesale corpus regeneration.** Roughly five hundred episodes have no
  session log (Claude Code, imported lanes) and would be lost.

## Decisions

- **One optional payload field, `files`**, a list of `{op, target}` entries.
  The closed schema admits it as an optional key like `workspace_root`;
  absence renders nothing, so episodes from turns that consulted nothing
  stay byte-identical to today's rendering.
- **Ops** are a closed vocabulary: `read`, `read!` (the read tool returned
  an error), `bash` (a `.md` path token in a bash command), `memory_get`
  (an episode id from the get result's details), `web_fetch` (hostname
  only), `child:read`, `child:read!` (a delegated child's `inspect_read`,
  ok or error). New ops are interface-tier additions.
- **Targets** are single-line, control-free, at most 512 characters, home
  prefix replaced by `~`, backslashes normalized to `/`, leading `@`
  stripped (the same normalization agent-delegate's provenance applies). A
  partial read (`offset`/`limit` present) carries the line range as a
  `:start-end` suffix, the estate's citation form. Entries dedupe per
  `(op, target)` in first-seen order and cap at 256; the overflow count
  renders as optional frontmatter `files_dropped`, outside the digest like
  the byte-truncation accounting.
- **Rendering**: `## Files` follows `## Tools`, one line per entry,
  `- <op> <target>`. The parser gains a Files reading in the body
  enumeration; the interpretation cap already bounds the search.
- **Digest** covers the files entries only when the list is nonempty (count,
  then op and target per entry, in the existing framed form). Every existing
  digest is unchanged; an edit to a Files line fails verification like an
  edit to any other body text. ADR 0003 records why this is additive rather
  than a major.
- **Versioning**: `aj-episode.v1` stays; ships as autojournal 2.1. A 2.0
  build reads every pre-2.1 episode and reports a Files-bearing episode as
  digest-mismatch; documented, accepted, this estate is the only consumer.
- **Capture policy** becomes `pi-visible-v3`. Import's prior-policy list
  gains `pi-visible-v2`.
- **Extraction** lives in the adapter's run summary: `toolCall.arguments`
  for `read` (path, offset, limit), `bash` (command tokenized on whitespace,
  quotes stripped, tokens containing `/` or starting with `~` and ending in
  `.md`, trailing `;,)` trimmed), `web_fetch` (url → hostname);
  `toolResult` joined by `toolCallId` for `read` errors, `memory_get`
  details for the episode id, and `delegate` details for child reads. The
  same function serves live capture and import, so backfill and live
  agree by construction.
- **Backfill** is an import option, `replace prior policy`: when a turn is
  already stored under a prior policy, import publishes the v3 episode,
  removes the prior file, and resyncs the index. `memory_get` on a
  reference into a removed episode returns `gone`, an existing outcome. The
  store gains its first removal primitive for this; it is owner-driven from
  the `/autojournal` menu and the CLI, never agent-reachable, the pattern
  reseal set. The first run on the live corpus happens with the owner
  present: the session logs make it repeatable, but it rewrites thousands of
  files.
- **agent-delegate** keeps a bounded `reads` list in the compact tool-result
  details: `{target, status}` for `inspect_read` provenance entries only,
  at most 64. Nothing else about the compact details changes.
- **introspect-scan** reads `## Files` when present: `read`, `bash`, or
  `child:read` of a path under `skills/<name>/` is an invocation of that
  skill; of a wiki note filename, a touch. Episodes without a Files section
  fall back to today's markers. The mentions column is unchanged.

## Seams under test

- **Contract, render, parse, digest** (`test/engine/golden.test.ts`,
  `conformance.test.ts`, `properties.test.ts`): a Files-bearing payload's
  bytes, id, and digest pinned; every existing pin unchanged; the parse
  round-trip and containment properties extended to bodies with a Files
  section, including a body whose owner text contains a quoted `## Files`.
- **Run summary** (`test/adapter.test.ts`): a recorded messages fixture
  exercising each op, a failed read, a partial read, a bash heredoc
  containing `.md` paths, the 256 cap, and a delegate result with child
  reads.
- **Import with replacement** (`test/import.test.ts`): a fixture session
  plus a pre-existing v2 episode of one of its turns; after import the v3
  episode exists, the v2 file is gone, the index agrees, and `get` on the
  old reference returns `gone`.
- **agent-delegate compact details** (its own `tests/`): provenance with
  mixed inspect tools compacts to `inspect_read` targets only.
- **introspect-scan** (`tests/run.sh`): a fixture episode with a Files
  section counts as invocation and touch; the fixture with prose-only
  references still counts as mentions.

## Slices

- [x] S1 Files section end to end on a hand-built payload: contract field,
  render, parse reading, digest coverage, golden and property pins. A
  fixture episode with Files verifies; every existing golden pin is
  unchanged. ADR 0003 lands with it.
- [x] S2 Live capture under `pi-visible-v3`: run-summary extraction for
  read, read!, bash, memory_get, web_fetch with normalization, ranges,
  dedupe, cap, and `files_dropped`. A real turn in this estate renders the
  expected lines. (after S1)
- [x] S3 Backfill: prior-policy list, the replace option, the store's
  removal primitive, menu wiring, import test with replacement. (after S2)
  Code landed 2026-09-03; the owner-present run over the live corpus is
  still to do.

- [x] S4 Child reads: agent-delegate compact `reads`, autojournal renders
  `child:read`, both suites extended. (after S2; kit and autojournal
  commits paired)
- [x] S5 introspect-scan consumes Files with fallback; README bias section
  rewritten around the new column; the next sweep's snapshot compares
  against the pre-fix one. (after S3)

## Open questions

- Whether the wiki's `bin/usage-report` also switches its read side to
  `## Files` or thins to delegate it to `introspect-scan` (already open in
  the introspect spec).
- Whether `bash` tokens should widen beyond `.md` once a month of data shows
  what the heuristic catches. Decide at the second sweep after S5.
- 2026-09-03 (S3): the replace option lives on the `/autojournal` import
  menu only. Import has no CLI verb today (it reads pi session logs from the
  extension), and giving it one means moving import into `src/`, which is
  its own change. The spec's "menu and CLI" is narrowed to the menu.
- 2026-09-03 (S2): bash-derived targets are relative to the command's
  working directory, so `docs/x.md` resolves only by suffix. The scanner
  matches skills and notes by suffix already.
