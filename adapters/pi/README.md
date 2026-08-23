# AutoJournal for Pi

AutoJournal is install-and-forget durable memory for the
[Pi coding agent](https://github.com/earendil-works/pi-coding-agent). Every
completed interactive turn becomes an owner-controlled Markdown episode. Pi
can later search those episodes with bounded, provenance-carrying tools.

## Install

~~~sh
pi install npm:autojournal
~~~

Start a new Pi process, or run **/reload** in an already-running Pi session.
That is the complete normal setup. The package includes prebuilt AutoJournal
binaries.

Supported package targets are Linux x64/ARM64 and macOS.

## First use

Complete one ordinary Pi turn, then run:

~~~text
/autojournal
~~~

The menu shows:

- the fully resolved journal directory and whether it came from the AutoJournal default or
  owner configuration;
- the active world and scope for this conversation;
- a capture on/off toggle for this conversation;
- a subagent capture toggle that applies to every session (off by default);
- episode count and index health;
- index synchronization and diagnostics;
- **Reseal edited episodes**, which re-attests episode files you edited by hand so they
  return to search. This row runs `autojournal reseal`, the same reseal the CLI exposes;
- an importer for Pi sessions that predate AutoJournal.

Capture occurs after Pi's **agent_settled** event.

Direct maintenance shortcuts remain available:

~~~text
/autojournal status
/autojournal sync
~~~

## Importing existing Pi sessions

Pi keeps every past session as a log file under its own data directory, so
conversations from before AutoJournal was installed can still become
memory. Choose **Import Pi session history** in the **/autojournal** menu:
it scans those logs, asks which world and scope to import into, and
publishes each completed user→assistant turn as an ordinary episode.

When the extension starts with an empty journal and finds existing session
logs, it mentions this option once; the import itself only ever runs when
chosen from the menu.

The import is safe to re-run and safe to combine with live capture: each
turn keeps a stable identity derived from its session log.

## Default journal directory

With no configuration, AutoJournal uses:

~~~text
$XDG_DATA_HOME/autojournal/journals
~~~

When XDG_DATA_HOME is unset, that resolves to:

~~~text
~/.local/share/autojournal/journals
~~~

The ordinary **main / default / conversation** layout contains only date
shards and Markdown:

~~~text
~/.local/share/autojournal/journals/
└── YYYY/
    └── MM/
        └── DD/
            └── aj1-<episode-id>.md
~~~

The npm package is installed elsewhere under Pi's managed package directory.

Open the journal directory directly as an Obsidian vault or in any editor or
IDE. The SQLite search index under
$XDG_STATE_HOME/autojournal (normally ~/.local/state/autojournal) is a
disposable projection that **/autojournal sync** can rebuild.

Copying, moving, deleting, and editing episode files is reflected after `/autojournal
sync`. A manual copy sharing an episode identity is deduplicated (the first copy found
stays searchable), and dot-directories are ignored. Revision checking recomputes each
episode's digest from its actual content before serving it, so an in-place body edit (even
one that leaves the recorded `payload_digest` line untouched) is detected. The episode is
then excluded from search, evidence references to it return `stale_revision`, and `sync`
counts it as a digest mismatch (also available machine-readably via `sync --json`).

Running `autojournal reseal` re-attests your own edits by rewriting the recorded digest to
match the edited content; `reseal --preview` lists detected edits.

## Worlds and scopes

By design AutoJournal does not need repo-level isolation; however, an isolated
journal space can still be useful.

- **World** is a separate corpus. Choosing another world changes both capture and search
  for the current conversation.
- **Scope** is an optional collection within a world, bounding capture and search the same
  way.
- **Lane** is a system record type. Ordinary turns use `conversation`; delegated,
  evaluation, and imported records are assigned by their producers.

**Capture: on/off (this session)** turns journaling off for the current
conversation without unloading the extension

**Subagent capture: on/off (all sessions)** lets subagent sessions publish
episodes.

Selections persist as session state and can be saved as new defaults for every future
session in any conforming harness. Non-default classifications add directories to the path
only when used.

Default classifications are omitted from the physical path. Advanced classifications add
directories only when used:

~~~text
<journal>/
├── YYYY/MM/DD/*.md
├── scopes/<scope>/YYYY/MM/DD/*.md
├── lanes/<system-lane>/YYYY/MM/DD/*.md
└── worlds/<world>/
    ├── YYYY/MM/DD/*.md
    ├── scopes/<scope>/YYYY/MM/DD/*.md
    ├── lanes/<system-lane>/YYYY/MM/DD/*.md
    └── scopes/<scope>/lanes/<system-lane>/YYYY/MM/DD/*.md
~~~

## How recall works

AutoJournal registers two tools: `memory_search(query, limit?)` for ranked discovery
within the conversation's active world and scope, and `memory_get(reference, lines?)` for
exact source evidence. Search assigns each hit a short, conversation-local reference and
keeps its opaque episode and revision identities in adapter state, so the model does not
have to transcribe them.

**Ranking.** Every matched line scores as rarity × recency. Rarity is a classic IDF sum:
each matched query term contributes log(N / df), so a term appearing in one episode out of
thousands dominates while a term appearing everywhere contributes almost nothing. That is
multiplied by a recency factor of 1 + boost / (elapsed_24h_periods + 1); at the default
boost of 1.0, an episode under 24 hours old scores ×2, one 24–47 hours old ×1.5, and one
seven elapsed periods old about ×1.13, decaying toward ×1. Each episode's age is floored
into 24-hour periods, so its score changes only at those boundaries.

**Thesaurus.** A single hand-editable JSON file maps a casual query word to the canonical
terms that actually appear in the journal, for example `{"firmware": ["fwupd",
"polkit"]}`. Each query term is looked up and its aliases are added to the term set
(additive expansion). The file is re-read on every search, so edits take effect
immediately, and a SHA-256 digest of its canonical form is stamped on results so you can
tell which version produced a given answer. Curation is deliberately manual; an opt-in
miss log records queries that came back weak, which is the raw material for growing the
map from real recall failures. A broken or missing file is treated as empty.

## Configuration

`~/.config/autojournal/config.json` is the config file. The lookup also honors
`$XDG_CONFIG_HOME` and `$AUTOJOURNAL_CONFIG`. Paths must be absolute and every key is
optional: a config naming no `journal_root` keeps the default location and may carry only
capture defaults or retrieval knobs.

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
5. Verify the new path, episode count, and fresh index in `/autojournal`, then
   search a known historical phrase and open it with `memory_get`.

The index filename is keyed to the configured journal path, so relocation
creates a new projection; the old one is disposable. Don't move or edit the
SQLite file by hand.

A journal root under a shared directory (one whose nearest existing parent is group- or
world-writable, such as `/tmp`) is refused for capture and sync. Other local users can
interfere in these directories, and such locations are often cleared on reboot. Choose a
private, persistent directory, or `chmod g-w,o-w` the parent.

## Other harnesses

AutoJournal's default directory is host-neutral. Claude Code, Codex, or another
adapter that invokes the same completed-turn capture protocol shares the
corpus when it resolves the same journal root, world, and scope. The Go package
also exposes the core operations to embedding hosts.

## Update, remove, and recover

Pi manages package updates and removal. Neither operation intentionally
deletes the journal directory or owner configuration. Reinstalling the
extension can reuse the existing Markdown and rebuild its index with:

~~~text
/autojournal sync
~~~

If capture fails, Pi turns remain unaffected and the extension warns once.
Use **/autojournal status** for the exact journal path, adapter counters, and
index state.

Only interactive sessions publish episodes by default: headless `pi -p`/JSON
runs and exec-spawned sub-agents are skipped, so automation never pollutes
the journal. Turning on Subagent capture in the **/autojournal** menu admits
subagent sessions; headless owner runs stay excluded. Recall tools work
either way.
