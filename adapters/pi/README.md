# AutoJournal for Pi

AutoJournal is install-and-forget durable memory for the
[Pi coding agent](https://github.com/earendil-works/pi-coding-agent). Every
successfully completed turn becomes an owner-controlled Markdown episode.
Pi can later search those episodes with bounded, provenance-carrying tools.

## Install

~~~sh
pi install npm:autojournal
~~~

Start a new Pi process, or run **/reload** in an already-running Pi session.
That is the complete normal setup. The package includes prebuilt AutoJournal
binaries; users do not need Go, SQLite, a compiler, or an **autojournal**
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
- a capture on/off toggle for this conversation;
- episode count and index health;
- index synchronization and diagnostics;
- an importer for Pi sessions that predate AutoJournal.

Capture occurs after Pi's **agent_settled** event, so the response currently
being generated appears only after the whole turn has finished.

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
publishes each completed user→assistant turn as an ordinary episode. No
separate CLI install is needed — the importer runs through the same bundled
binary as live capture.

When the extension starts with an empty journal and finds existing session
logs, it mentions this option once; the import itself only ever runs when
chosen from the menu.

The import is safe to re-run and safe to combine with live capture: each
turn keeps a stable identity derived from its session log, so a turn that
is already in the journal — from an earlier import or from live capture —
is reported as already present rather than stored twice. Episodes keep the
original conversation's timestamps, and their provenance is visible in the
episode frontmatter (`adapter_version: <version>+import`). Session logs
written by subagent runs are recognized and skipped, matching live
capture's rule that only the owner's own conversation enters memory; the
source logs are never modified.

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

The npm package is installed elsewhere under Pi's managed package directory.
Journal data is deliberately outside the install directory so extension
updates and removal do not delete memory.

Open the journal directory directly as an Obsidian vault or in any editor or
IDE. Markdown is the durable authority. The SQLite search index under
$XDG_STATE_HOME/autojournal (normally ~/.local/state/autojournal) is a
disposable projection that **/autojournal sync** can rebuild.

Hand-editing is safe for search integrity: an episode whose content changed
serves stale_revision rather than silently different evidence, a manual copy
sharing an episode identity is deduplicated (the first copy found stays
searchable), and dot-directories such as .obsidian or .git are never read as
episodes. After copying, moving, or deleting files yourself, run
**/autojournal sync** to rebaseline the index.

## Worlds and scopes

Most users never configure these. Every normal Pi session captures and
searches **main / default**, across projects and sessions.

Use the **/autojournal** menu when a conversation needs isolation:

- **World** is a separate corpus. Choosing or creating another world changes
  both capture and memory_search for the current Pi conversation.
- **Scope** is an optional collection within a world. Choosing or creating a
  scope likewise bounds capture and search to that scope.
- **Lane** is a system record type, not a user folder. Ordinary turns use
  conversation; delegated, evaluation, and imported records are assigned by
  their producers.

**Capture: on/off (this session)** turns journaling off for the current
conversation without unloading the extension — use it for test runs,
scratch sessions, or anything else that should not enter memory. While
capture is off the menu title says so, each finished turn counts as
skipped in the adapter diagnostics, and memory_search / memory_get keep
working; it never becomes an owner default, so new sessions always start
with capture on.

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

There is no automatic ambient injection. Search results include episode and
revision identity; opening changed evidence returns stale_revision rather
than silently substituting different content.

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

Pre-release configurations using the former **world_root** key remain
accepted for migration, but new and updated configuration should use
**journal_root**.

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
   mv ~/.local/share/autojournal/journals /absolute/new/location
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
7. Search for a known historical phrase and open it with memory_get.

The index filename is keyed to the configured journal path, so relocation
creates a new projection. Do not move or edit SQLite manually; rebuild it.
The old projection is disposable and may be left in place.

Putting the journal on a local directory used by Obsidian is supported.
Network and cloud-synchronized filesystems should preserve atomic rename and
normal filesystem permissions; if they do not, keep the authoritative journal
on a local filesystem and synchronize backups separately.

A journal root placed under a shared directory — one whose nearest existing
parent is group- or world-writable, such as /tmp — is refused for capture
and sync: other local users can interfere there, and such locations are
often cleared on reboot. Choose a private, persistent directory (or
`chmod g-w,o-w` the parent you intend).

### Journals from pre-release Pi installs

Earlier preview builds defaulted the journal into Pi's agent directory
(~/.pi/agent/journals). That location is no longer read by default; the
adapter warns at session start when it finds a journal there while the
host-neutral default is active. Follow the move steps above — or keep the
journal where it is by setting "journal_root" to that path — then run
**/autojournal sync**.

## Other harnesses

AutoJournal's default directory is host-neutral. Claude Code, Codex, Evoker,
or another adapter that invokes the same completed-turn capture protocol will
share the corpus when it resolves the same journal root, world, and scope.
Pi and Evoker may provide richer first-class controls; simpler hooks can use
owner-configured defaults.

Legacy session Markdown is not silently interpreted as an AutoJournal 1.0
episode. It must be imported through a versioned legacy importer before it
participates in search.

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
