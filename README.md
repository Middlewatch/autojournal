# AutoJournal for Pi
Note: I will be cleaning up my documentation over time but as of right now it is heavily LLM written

AutoJournal is automatic session derived memory for the
[Pi coding agent](https://github.com/earendil-works/pi-coding-agent). Every
completed interactive turn becomes a Markdown file that trims out unnecessary
JSON metadata and retains the relevant user and model output turns (tool calls
and other junk are dropped). Pi can later search those episodes with bounded,
provenance-carrying tools. Headless and sub-agent runs are deliberately not
captured; see **Update, remove, and recover** below.

## Install

~~~sh
pi install npm:autojournal
~~~

This package includes prebuilt binaries because the core is a single static
Go executable. That was an intentional choice: the same core serves other
harnesses and an embedding host, so the system stays portable with no
runtime dependencies.

Start a new Pi process, or run **/reload** in an already-running Pi session.
The package includes prebuilt AutoJournal binaries; users do not need Go, SQLite, a compiler, or an **autojournal**
command on PATH.

Supported package targets are Linux x64/ARM64 and macOS Intel/Apple Silicon.
Windows is not yet supported.

## First use

Complete one ordinary Pi turn, then run:

~~~text
/autojournal
~~~

The menu shows:

- the fully resolved journal directory and whether it came from the
  AutoJournal default or owner configuration;
- the active world and scope for this conversation;
- episode count and index health;
- index synchronization and diagnostics;
- an importer that backfills memory from Pi sessions that predate
  AutoJournal (see the package README for details).

Capture occurs after Pi's **agent_settled** event, so the response currently
being generated appears only after the whole turn has finished.

Direct maintenance shortcuts remain available:

~~~text
/autojournal status
/autojournal sync
~~~

## Default journal directory

With no owner configuration, AutoJournal uses:

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

Journal data is deliberately outside the ~/.pi install directory so extension
updates and removal do not delete memory.

The SQLite search index under $XDG_STATE_HOME/autojournal (normally
~/.local/state/autojournal) is a disposable projection that
**/autojournal sync** can rebuild. It is future-proofing for a corpus growing
toward six figures of session logs. A plain `rg` scan is probably fast enough
well past current corpus sizes; that crossover has not been measured against a
real dataset of that scale.

Hand-editing is safe for search integrity: an episode whose content changed
serves stale_revision rather than silently different evidence, a manual copy
sharing an episode identity is deduplicated (the first copy found stays
searchable), and dot-directories such as .git or .obsidian are never read as
episodes. After copying, moving, or deleting files yourself, run
**/autojournal sync** to rebaseline the index.

## Worlds and scopes

This terminology is provisional and may change. The schema is in place for
users who want to separate access and retention boundaries between different
journal corpora; by default I recommend sending every session to the same
world.

Use the **/autojournal** menu to edit the following:

- **World** defines a separate corpus. Choosing or creating another world changes
  both capture and memory_search for the current Pi conversation.
- **Scope** is an optional collection within a world. Choosing or creating a
  scope likewise bounds capture and search to that scope.
- **Lane** is a system record type, not a user folder. Ordinary turns use
  conversation; delegated, evaluation, and imported records are assigned by
  their producers.

Selections are saved as Pi session state. Reloading or resuming restores them,
and a fork inherits the selection present on its branch. A brand-new session
starts from the owner default; to make the current world and scope that
default for every future session (in every conforming harness), choose
**Save as default for new sessions** in the menu. It rewrites the owner
config atomically and preserves everything else in it. (Standalone installs
that put the `autojournal` binary on PATH can run
`autojournal default --world <world> --scope <scope>` for the same rewrite;
the Pi package needs no binary on PATH.)

Default classifications are omitted from the physical path. Advanced
classifications add directories only when used:

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

## Recall tools

AutoJournal registers:

- memory_search(query, limit?) for ranked discovery in the conversation's
  active world and scope;
- memory_get(episode_id, revision, lines?) for exact source evidence.

## Recall helpers

Thesaurus (alias map): The thesaurus is a single hand-editable JSON file mapping a casual query word to the canonical terms that actually appear in the journal - for example {"firmware": ["fwupd", "polkit"]}. (That example lives in my own thesaurus and is intentionally stripped from the default package.) When a search runs, each query term is looked up in the map and any aliases are added to the term set (additive expansion). The file is loaded fresh on every search, so an edit takes effect immediately; only a SHA-256 digest of the map's canonical form is stamped on results, so you can tell which thesaurus version produced a given answer. Curation is deliberately manual and an opt-in miss log records queries that came back weak, giving you raw material for growing the map from real recall failures. A broken or missing thesaurus file is treated as empty.

Recency-based recall: Every matched line gets a score of rarity × recency. Rarity is a classic IDF sum — each matched query term contributes log(N / df), so a term that appears in only one episode out of many dominates, while a term present everywhere contributes nearly nothing. That rarity score is then multiplied by a recency factor of 1 + boost / (days_old + 1): with the default boost of 1.0, something written today scores ×2, yesterday ×1.5, a week ago about ×1.13, decaying toward ×1 for old entries. The age is floored to whole days, so the same query gives stable results all day rather than drifting hour by hour. The key design point is that recency is a nudge rather than a hard override.

## Move or share the journal

Owner configuration lives at:

~~~text
~/.config/autojournal/config.json
~~~

The lookup also honors $XDG_CONFIG_HOME and $AUTOJOURNAL_CONFIG. Paths must
be absolute. Every key is optional: a config that names no journal_root keeps
the host-neutral default location and can hold only capture defaults or
retrieval knobs (the file **Save as default for new sessions** writes looks
exactly like that).

### Choose another location before first capture

Create:

~~~json
{
  "journal_root": "/absolute/path/to/my/journals"
}
~~~

Run **/reload**, then open **/autojournal** and confirm that the displayed
journal directory and source are correct.

### Move an existing journal

1. Finish and close active Pi sessions and stop every other process writing
   AutoJournal episodes.
2. Move the complete journal without rearranging its contents:

   ~~~sh
   mv $XDG_DATA_HOME/autojournal/journals /absolute/new/location
   ~~~

3. Create or update ~/.config/autojournal/config.json:

   ~~~json
   {
     "journal_root": "/absolute/new/location"
   }
   ~~~

4. Restart Pi or run **/reload**.
5. Run **/autojournal sync**.
6. Open **/autojournal** and verify the new path, episode count, and fresh
   index.
7. Test with a prompt containing a known historical phrase, and confirm the
   agent successfully returns a memory_search and memory_get.

The index filename is keyed to the configured journal path, so relocation
creates a new projection. Do not move or edit SQLite manually.

Putting the journal in a local directory that Obsidian also indexes is
supported. Network and cloud-synchronized filesystems must preserve atomic
rename and normal filesystem permissions; this has not been tested against any
such filesystem. If they do not, keep the authoritative journal on a local
filesystem and synchronize backups separately.

A journal root placed under a shared directory — one whose nearest existing
parent is group- or world-writable, such as /tmp — is refused for capture
and sync: other local users can interfere there, and such locations are
often cleared on reboot. Choose a private, persistent directory (or
`chmod g-w,o-w` the parent you intend).

## Other harnesses

AutoJournal's default directory is host-neutral. Claude Code, Codex, Hermes,
or another adapter that invokes the same completed-turn capture protocol will
share the corpus when it resolves the same journal root, world, and scope.
Pi gets the richest first-class controls because of its TypeScript adapter,
but a hook that shells out to the standalone binary is straightforward to
write for any other coding agent and shares the same memory system.

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

Only interactive sessions publish episodes: headless `pi -p`/JSON runs and
exec-spawned sub-agents are synthetic work products and are skipped, so
automation never pollutes the journal. Their recall tools still work.

## Contributors and non-Pi integration

The repository builds one Go implementation as both an importable module
(`github.com/Middlewatch/autojournal/src`) and a standalone static
executable. The npm package is a thin Pi lifecycle and TUI adapter plus
prebuilt platform binaries.

~~~text
src/                 shared contracts, publication, index, and retrieval
adapters/pi/         Pi extension and npm package
docs/                design, architecture, and search tuning
scripts/             verification and release gates
~~~

Use a Go 1.26+ toolchain and the complete gate:

~~~sh
./scripts/verify.sh
~~~

Before npm publication, build every platform binary and inspect the actual
tarball:

~~~sh
./scripts/release-check.sh
~~~

The standalone executable supports capture, search, get, status, catalog,
sync, alias maintenance, and version reporting. It defaults to the same
host-neutral journal directory as the Pi package; **--root** and owner
configuration can override it.
