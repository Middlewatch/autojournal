# AutoJournal

AutoJournal is automatic, session-derived memory for the
[Pi coding agent](https://github.com/earendil-works/pi-coding-agent). Every
completed interactive turn becomes a Markdown episode that keeps the user
and the visible model output and drops the JSON metadata and tool traffic
around it. Agents search those episodes later through bounded,
provenance-carrying tools.

## Install

~~~sh
pi install npm:autojournal
~~~

Then start a new Pi process, or run `/reload` in a running session.

## First use

Complete one ordinary turn, then run `/autojournal` for the resolved
journal directory, the active world and scope, episode count, index health,
and maintenance actions. `/autojournal status` and `/autojournal sync` are
direct shortcuts.

Capture happens after the end of each assistant turn. The capture policy
(`pi-visible-v2`) keeps every visible assistant text segment of the turn in
order — mid-turn progress notes and verdicts as well as the final reply.
Thinking traces and tool arguments never enter memory.

If you have Pi session history predating the install, `/autojournal` →
"Import Pi session history" replays it turn by turn; re-running the import
is idempotent, and turns captured live (under this or the previous capture
policy) are recognized rather than stored twice.

## Journal location

By default episodes go to `$XDG_DATA_HOME/autojournal/journals` (normally
`~/.local/share/autojournal/journals`):

~~~text
~/.local/share/autojournal/journals/
└── YYYY/
    └── MM/
        └── DD/
            └── aj1-<episode-id>.md
~~~

A single-file search snapshot under `$XDG_STATE_HOME/autojournal` (normally
`~/.local/state/autojournal`) is a disposable projection that
`/autojournal sync` rebuilds. It keeps incremental search work separate
from the source corpus.

Copying, moving, deleting, and editing episode files is reflected after
`/autojournal sync`. A manual copy sharing an episode identity is
deduplicated (the first copy found stays searchable), and dot-directories
are ignored. Revision checking recomputes each episode's digest from its
actual content before serving it, so an in-place body edit (even one that
leaves the recorded `payload_digest` line untouched) is detected. The
episode is then excluded from search, evidence references to it return
`stale_revision`, and `sync` counts it as a digest mismatch (also available
machine-readably via `sync --json`).

Running `autojournal reseal` re-attests your own edits by rewriting the
recorded digest to match the edited content; `reseal --preview` lists
detected edits.

Oversized turns are not lost: content beyond the 2 MiB per-side budget is
tail-truncated deterministically, and the dropped byte count is recorded in
the episode's frontmatter and surfaced by `status` and `sync`.

## Worlds and scopes

By design AutoJournal does not need repo-level isolation; however, an
isolated journal space can still be useful.

- **World** is a separate corpus. Choosing another world changes both
  capture and search for the current conversation.
- **Scope** is an optional collection within a world, bounding capture and
  search the same way.
- **Lane** is a system record type. Ordinary turns use `conversation`;
  delegated, evaluation, and imported records are assigned by their
  producers.

Selections persist as session state and can be saved as new defaults for
every future session. Non-default classifications add directories to the
path only when used.

## How recall works

AutoJournal registers two tools: `memory_search(query, limit?)` for ranked
discovery within the conversation's active world and scope, and
`memory_get(reference, lines?)` for exact source evidence. Search assigns
each hit a short, conversation-local reference and keeps its opaque episode
and revision identities in extension state, so the model does not have to
transcribe them.

**Ranking.** Every matched line scores as rarity × recency. Rarity is a
smoothed IDF sum: each matched query term contributes
ln(1 + (N − df + 0.5)/(df + 0.5)), so a term appearing in one episode out
of thousands dominates while a term appearing everywhere contributes almost
nothing. That is multiplied by a recency factor of
1 + boost / (elapsed_24h_periods + 1); at the default boost of 1.0, an
episode under 24 hours old scores ×2, one 24–47 hours old ×1.5, decaying
toward ×1. Each episode's age is floored into 24-hour periods, so its score
changes only at those boundaries. `docs/SEARCH_TUNING.md` covers the knobs.

**Thesaurus.** A single hand-editable JSON file maps a casual query word to
the canonical terms that actually appear in the journal, for example
`{"firmware": ["fwupd", "polkit"]}`. Each query term is looked up and its
aliases are added to the term set (additive expansion). The file is re-read
on every search, so edits take effect immediately, and a SHA-256 digest of
its canonical form is stamped on results so you can tell which version
produced a given answer. Curation is deliberately manual; an opt-in miss
log records queries that came back weak, and the `/autojournal` → "Search
quality" section reviews those weak queries and promotes or removes aliases
behind an explicit confirm. The `alias` CLI verbs drive the same engine
functions. A broken or missing file is treated as empty.

## The owner CLI

The package installs an `autojournal` bin over the same in-process engine:
`capture`, `search`, `get`, `alias`, `default`, `status`, `catalog`,
`sync`, `reseal`, and `version`, each with `--json` for machine-readable
output. `autojournal` with no arguments prints the full flag reference.

## Configuration

`~/.config/autojournal/config.json` is the config file. The lookup also
honors `$XDG_CONFIG_HOME` and `$AUTOJOURNAL_CONFIG`. Paths must be absolute
and every key is optional: a config naming no `journal_root` keeps the
default location and may carry only capture defaults or retrieval knobs.

To choose another location before first capture, create:

~~~json
{
  "journal_root": "/absolute/path/to/my/journals"
}
~~~

then `/reload` and confirm the displayed directory in `/autojournal`.

To move an existing journal:

1. Close active sessions and stop every other process writing episodes.
2. Move the journal without rearranging its contents:

   ~~~sh
   mv "${XDG_DATA_HOME:-$HOME/.local/share}/autojournal/journals" /absolute/new/location
   ~~~

3. Point `journal_root` at the new location.
4. Restart the harness or `/reload`, then run `/autojournal sync`.
5. Verify the new path, episode count, and fresh index in `/autojournal`,
   then search a known historical phrase and open it with `memory_get`.

The snapshot filename is keyed to the configured journal path, so
relocation creates a new projection; the old one is disposable. Don't edit
the snapshot file by hand.

A journal root under a shared directory (one whose nearest existing parent
is group- or world-writable, such as `/tmp`) is refused for capture and
sync. Other local users can interfere in these directories, and such
locations are often cleared on reboot. Choose a private, persistent
directory, or `chmod g-w,o-w` the parent.

Concurrent Pi sessions over one corpus are the normal case: publication is
atomic and first-write-wins, index writers serialize on a lock file with
stale-lock recovery, and snapshots replace by atomic rename.

The extension publishes only interactive sessions: headless runs and
exec-spawned sub-agents are skipped, while their recall tools still work.
Subagent sessions can be published by turning on Subagent capture in the
`/autojournal` menu (off by default); headless automation runs stay
excluded.

## Corpus compatibility

Episode Markdown bytes, episode-id derivation, and payload-digest
derivation are frozen (`aj-episode.v1`). A corpus written by the 1.x Go
engine is read, searched, and extended by 2.x with no migration; the 2.0
release was gated on a sweep of the maintainer's full pre-port corpus
re-deriving every identity and digest byte-identically.

## Contributors

One TypeScript engine serves three surfaces: the Pi extension (`index.ts`),
the owner CLI (`cli.ts`), and the engine library (`src/`).

~~~text
src/                 engine: contracts, identity, store, index, retrieval
index.ts             Pi extension entry
cli.ts               owner CLI over the same engine
test/                the whole suite (engine, CLI, extension)
testdata/            golden byte pins, conformance corpus, fuzz seeds
docs/                architecture map, search tuning, ADRs, specs
scripts/             verification and release gates, plus two owner
                     utilities outside the gate: corpus-junk-report.py
                     (recall-hygiene ranking of sessions by junk mass) and
                     retrieval-eval.py (ranking metrics over a private
                     judged query set)
~~~

~~~sh
npm ci
./scripts/verify.sh    # the full repository gate
~~~

Before npm publication:

~~~sh
./scripts/release-check.sh
~~~

Released versions are recorded in [`CHANGELOG.md`](CHANGELOG.md). Licensed
MIT. Development setup and the rules a change has to respect live in
[`CONTRIBUTING.md`](CONTRIBUTING.md); [`NOTICES.md`](NOTICES.md) records
that the package ships no third-party code.
