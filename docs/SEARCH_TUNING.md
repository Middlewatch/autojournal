# Search behavior and thesaurus curation

This guide explains how a query turns into results, what to check when an
expected result does not appear, and how to grow the alias thesaurus without
degrading recall quality.

## How a query is matched

A search runs through five stages:

1. **Term extraction.** The query is lowercased and split into words of 3+
   characters from `[a-z0-9_]`; stop words ("the", "was", …) are dropped. A
   query made only of stop words or 1–2 letter words matches nothing.
2. **Alias expansion.** Each term is looked up in the thesaurus by exact
   match, and its canonical values join the term list. Expansion is additive:
   aliases can only widen a search, never narrow it.
   Plural terms also fold: `quotas` additionally searches `quota`,
   `policies` searches `policy` — the one word-form direction the boundary
   rule below cannot cover, since a plural query term never occurs inside
   its singular's text.
3. **Discovery.** The index vocabulary is scanned for tokens containing any
   term as a substring, and the matching tokens' postings become the
   candidate lines. Needles shorter than 3 bytes are skipped when longer
   needles exist, so one over-broad fragment cannot exhaust the discovery
   budget for the rest of the query.
4. **Crediting (the filter this guide is mostly about).** Each candidate
   line is re-checked against the source text. A term credits a line only
   where an occurrence *begins at a word boundary*:
   - `hang` credits "hang", "hangs", "hanging" — but **not** "changed".
   - `config` credits "config" and "configuration" — prefixes stay free.
   - `index` does **not** credit "reindexing" — mid-word (infix)
     occurrences never credit. See below for how to recover an infix
     family you actually want.
5. **Scoring.** Credited lines are ranked by term rarity (a word appearing
   in few episodes outweighs a ubiquitous one) with a mild recency boost.
   One episode contributes at most two result regions per page, so a long
   episode cannot crowd out the rest of the corpus. Each hit's
   `confidence` band discounts partial matches: a hit crediting only some
   of your query words needs a proportionally stronger score to report
   `high` — ordering is unaffected, the band is display trust only.

## When an expected result is missing

Work through these in order; each has a fast check.

1. **Is the index current?** Run `autojournal status`. If it reports stale,
   run `autojournal sync`. A search over an empty-but-should-not-be index
   reports `index_stale`; a partially stale one answers normally while
   marking `freshness: stale`, rather than pretending the corpus is smaller
   than it is.
2. **Was your word dropped at extraction?** 1–2 letter words and stop words
   never search. Rephrase with a longer or rarer word ("q8" style short
   canonical terms work as *alias values*, not as query words).
3. **Is it a word-form mismatch?** You searched `deploy`, the journal says
   `deployment` — that still matches (prefix). But you searched `index` and
   the journal only says `reindexing` — that does not (infix). Diagnose
   with v1-parity crediting:

   ```sh
   autojournal search <query...> --credit-mode substring
   ```

   If the missing result appears under `substring`, the boundary filter is
   dropping an infix occurrence. Fix it durably with an alias that names a
   word-start form: `autojournal alias add index reindex` makes `index`
   also credit "reindex", "reindexing", "reindexed".
4. **Is it a vocabulary mismatch?** You search `vpn`, the journals only ever
   say `tailscale`. No boundary rule fixes that — it is what the thesaurus
   is for: `autojournal alias add vpn tailscale`.
5. **Check the miss log.** With `"miss_log": true` in `config.json`, every
   weak-scoring search is recorded, and `autojournal alias candidates`
   aggregates repeated misses into candidate queries for review. This is the
   intended feedback loop for growing the map from real usage instead of
   guessing.

## Updating the thesaurus

The thesaurus is one flat JSON object mapping a query word to the canonical
terms your journals actually use. It lives at
`$XDG_CONFIG_HOME/autojournal/thesaurus.json` (normally
`~/.config/autojournal/thesaurus.json`, or the configured `thesaurus_path`)
and is read fresh on every search — edits apply immediately, no restart or
reindex.

Prefer the CLI, which validates entries and keeps the file well-formed:

```sh
autojournal alias list
autojournal alias add firmware fwupd
autojournal alias remove firmware [fwupd]
autojournal alias candidates   # miss-log suggestions
```

Hand-editing the file is fine too; a corrupt file degrades to an empty map
(search still works, just without expansion) rather than failing.

### Curation rules that keep the map useful

- **Keys are exact-match.** `crash` firing does not make `crashed` fire;
  inflected query forms each need their own key.
- **Don't add prefix expansions.** `config → configuration` buys nothing:
  prefixes already match. The useful direction is long-to-short
  (`configuration → config`) and casual-to-canonical (`vpn → tailscale`).
- **Avoid near-universal targets.** An alias value that appears in most of
  your journal flattens the query into "match everything". Before adding a
  broad word such as "session", "agent", "log", or "model", ask whether the
  results of searching that word alone would be useful — the alias inherits
  exactly that behavior.
- **Phrase values are supported and precise.** `oom → "out of memory"`
  credits only lines containing the whole phrase; use phrases when the
  single-word form is too common (`memory`).
- **Avoid values with very short fragments.** A value like `pi-guardrails`
  contributes a 2-byte discovery needle ("pi"); the scanner drops such
  needles when it can, but a value whose *only* tokens are short scans
  broadly. Prefer the distinctive token alone (`guardrails`).
- **Grow from misses, not speculation.** The miss log tells you which real
  queries failed. A speculative synonym pack was tried and removed as
  net-negative; small and targeted wins.

## Reviewing and ruling on candidates

This is the workflow when the owner asks to "review and rule on potential
thesaurus entries" (or similar). It is written so an agent can run it end
to end; every verdict stays with the owner.

1. **Gather.** `autojournal alias candidates` prints distinct weak queries,
   most frequent first. Without the CLI, read the miss log directly: it is
   JSONL at `$XDG_STATE_HOME/autojournal/thesaurus-candidates.jsonl`
   (default `~/.local/state/autojournal/thesaurus-candidates.jsonl`, or
   `$AUTOJOURNAL_MISS_LOG`), one record per weak-scoring search:
   `{"ts", "query", "terms", "best", "top"}`. Aggregate by query, count
   repeats. A query is only logged when `"miss_log": true` is set in
   `config.json` and its best score fell below the confidence floor.
2. **Triage.** Drop candidates that are one-off noise: session-specific
   identifiers, typos, test strings, queries about topics the journal
   genuinely does not cover. Repeated misses and recognizable vocabulary
   mismatches (the owner says "vpn", the journal says "tailscale") are the
   real candidates.
3. **Find the canonical term.** For each surviving candidate, search the
   journal for what it *actually* says about the topic
   (`autojournal search <likely canonical words>`; without the CLI, `rg -li`
   over the journal root). The alias value must be a word the journal
   uses, not another guess.
4. **Check breadth before proposing.** A near-universal target flattens the
   query into "match everything". Run `autojournal search <value> --json`
   and compare its breadth with the corpus size from `autojournal status`.
   The search `total` counts result regions, not distinct episodes, so it is a
   conservative warning signal rather than an episode percentage. If the
   value is visibly broad, prefer a rarer word or a phrase value. The curation
   rules above (no prefix expansions, phrases for precision, no short
   fragments) all apply to the proposed value.
5. **Present verdicts to the owner.** For each candidate: the failed
   query, how often it missed, the proposed
   `autojournal alias add <key> <value>` (or "dismiss" with the reason),
   and what the value's breadth check showed. Apply only what the owner
   approves — the engine never invents aliases, and neither does the
   reviewing agent.
6. **Reset the log.** After the ruling session, truncate the miss log so
   the next review starts from fresh signal:
   `: > "${AUTOJOURNAL_MISS_LOG:-${XDG_STATE_HOME:-$HOME/.local/state}/autojournal/thesaurus-candidates.jsonl}"`.
   The log is
   bounded (1 MiB default) and best-effort, so stale entries otherwise
   linger and crowd `alias candidates`.

## Credit modes reference

`--credit-mode` on `autojournal search` selects the boundary rule, mainly
for diagnosis:

| mode | rule | example |
|---|---|---|
| `word_start` (default) | occurrence starts at a word boundary | `hang` credits "hanging", not "changed" |
| `substring` | any occurrence (v1 parity) | `index` credits "reindexing" |
| `whole_word` | both edges bounded | `hang` credits only "hang" |

`whole_word` was evaluated on a private journal corpus and rejected as the
default because it drops legitimate inflections ("configuration",
"deployment"). In that unpublished evaluation, word-start removed 60–80% of
credited matches on boundary-prone queries (`hang`, `lock`, `space`) with no
measured loss of relevant top results.
