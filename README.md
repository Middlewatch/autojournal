# AutoJournal

AutoJournal is automatic, session-derived memory for coding agents. Every
completed interactive turn becomes a Markdown episode that keeps the user and
model output and drops the JSON metadata and tool traffic around it. Agents
search those episodes later through bounded, provenance-carrying tools.

The Markdown is the durable authority; everything else is a rebuildable
projection of it.

## Install

For the [Pi coding agent](https://github.com/earendil-works/pi-coding-agent):

~~~sh
pi install npm:autojournal
~~~

Then start a new Pi process, or run `/reload` in a running session. The
package bundles prebuilt binaries, so users need no Go, SQLite, compiler, or
`autojournal` on PATH. Supported targets are Linux x64/ARM64 and macOS
Intel/Apple Silicon; Windows is not supported.

The core is a single static Go executable. That was deliberate: the same core
serves other harnesses and an embedding host, so the system stays portable
with no runtime dependencies.

Full Pi usage (the `/autojournal` menu, session-history import, per-session
capture toggle) is documented in
[`adapters/pi/README.md`](adapters/pi/README.md), which is the README npm
publishes.

## First use

Complete one ordinary turn, then run `/autojournal` for the resolved journal
directory, the active world and scope, episode count, index health, and
maintenance actions. `/autojournal status` and `/autojournal sync` are direct
shortcuts.

Capture happens after the turn finishes, so the response being generated right
now enters memory only once it is complete.

## Where the data lives

With no owner configuration, episodes go to `$XDG_DATA_HOME/autojournal/journals`
(normally `~/.local/share/autojournal/journals`):

~~~text
~/.local/share/autojournal/journals/
└── YYYY/
    └── MM/
        └── DD/
            └── aj1-<episode-id>.md
~~~

Journal data sits deliberately outside the `~/.pi` install directory, so
extension updates and removal do not delete memory.

The SQLite search index under `$XDG_STATE_HOME/autojournal` (normally
`~/.local/state/autojournal`) is a disposable projection that
`/autojournal sync` rebuilds. It keeps incremental search work separate from
the source corpus. The repository does not include a large-corpus benchmark,
so it makes no claim about the crossover where SQLite becomes faster than a
direct text scan.

Copying, moving, deleting, and editing episode files is reflected after
`/autojournal sync`. A manual copy sharing an episode identity is deduplicated
(the first copy found stays searchable), and dot-directories such as `.git` or
`.obsidian` are never read as episodes. Revision checking recomputes each
episode's digest from its actual content before serving it, so an in-place body
edit (even one that leaves the recorded `payload_digest` line untouched) is
detected. The episode is then excluded from search, evidence references to it
return
`stale_revision`, and `sync` counts it as a digest mismatch (also available
machine-readably via `sync --json`). Running `autojournal reseal` re-attests
your own edits by rewriting the recorded digest to match the edited content;
`reseal --preview` lists what it would touch first.

## Worlds and scopes

Most users never touch these: every normal session captures and searches
`main` / `default`, across projects and sessions.

- **World** is a separate corpus. Choosing another world changes both capture
  and search for the current conversation.
- **Scope** is an optional collection within a world, bounding capture and
  search the same way.
- **Lane** is a system record type rather than a user folder. Ordinary turns
  use `conversation`; delegated, evaluation, and imported records are assigned
  by their producers.

Selections persist as session state and can be saved as the owner default for
every future session in any conforming harness. Non-default classifications
add directories to the path only when used; see the package README for the
full layout.

## How recall works

AutoJournal registers two tools: `memory_search(query, limit?)` for ranked
discovery within the conversation's active world and scope, and
`memory_get(reference, lines?)` for exact source evidence. Search assigns each
hit a short, conversation-local reference and keeps its opaque episode and
revision identities in adapter state, so the model does not have to transcribe
them. There is
no ambient injection. The agent either asks or nothing happens.

**Ranking.** Every matched line scores as rarity × recency. Rarity is a
classic IDF sum: each matched query term contributes log(N / df), so a term
appearing in one episode out of thousands dominates while a term appearing
everywhere contributes almost nothing. That is multiplied by a recency factor
of 1 + boost / (elapsed_24h_periods + 1); at the default boost of 1.0, an
episode under 24 hours old scores ×2, one 24–47 hours old ×1.5, and one seven
elapsed periods old about ×1.13, decaying toward ×1. Each episode's age is
floored into 24-hour periods, so its score changes only at those boundaries.
Recency is a nudge rather than a full override.

**Thesaurus.** A single hand-editable JSON file maps a casual query word to
the canonical terms that actually appear in the journal, for example
`{"firmware": ["fwupd", "polkit"]}`. Each query term is looked up and its
aliases are added to the term set (additive expansion). The file is re-read on
every search, so edits take effect immediately, and a SHA-256 digest of its
canonical form is stamped on results so you can tell which version produced a
given answer. Curation is deliberately manual; an opt-in miss log records
queries that came back weak, which is the raw material for growing the map
from real recall failures. A broken or missing file is treated as empty.

## Configuration

Owner configuration lives at `~/.config/autojournal/config.json`; the lookup
also honors `$XDG_CONFIG_HOME` and `$AUTOJOURNAL_CONFIG`. Paths must be
absolute and every key is optional: a config naming no `journal_root` keeps
the default location and may carry only capture defaults or retrieval knobs.

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
creates a new projection; the old one is disposable. Do not move or edit the
SQLite file by hand.

Keeping the journal in a local directory that Obsidian also indexes is
supported. Network and cloud-synchronized filesystems must preserve atomic
rename and normal filesystem permissions; this has not been tested against any
such filesystem. If yours does not, keep the authoritative journal on a local
filesystem and synchronize backups separately.

A journal root under a shared directory (one whose nearest existing parent is
group- or world-writable, such as `/tmp`) is refused for capture and sync.
Other local users can interfere in these directories, and such locations are
often cleared on reboot. Choose a private, persistent directory, or `chmod g-w,o-w` the parent.

## Other harnesses

The default directory is host-neutral. Any adapter invoking the same
completed-turn capture protocol shares the corpus when it resolves the same
journal root, world, and scope. Pi gets the richest first-class controls
through its TypeScript adapter; `adapters/claude-code/` and `adapters/codex/`
are working single-file Python hooks you can wire in directly, and they are
short enough to read as a template for another agent. The Go package also
exposes the same core operations to an embedding host, though this repository
does not ship an embedding-host integration. Each hook finds the binary via
`AUTOJOURNAL_BIN`, `~/.local/bin/autojournal`, or `PATH`. Bound what enters
memory with `autojournal default --world <w> --scope <s>` or a `journal_root`
of its own, not by narrowing the hook.

The Pi adapter publishes only interactive sessions: headless runs and
exec-spawned sub-agents are skipped, while their recall tools still work. The
standalone hooks apply the completion events and owner-turn filters available
in their respective harnesses.

## Contributors

One Go implementation builds as both an importable module
(`github.com/Middlewatch/autojournal/src`) and a standalone static executable.
The npm package is a thin Pi lifecycle and TUI adapter plus prebuilt binaries.

~~~text
src/                 shared contracts, publication, index, and retrieval
adapters/pi/         Pi extension and npm package
DESIGN.md            the binding product contract and its reasoning
docs/                architecture map and search tuning
scripts/             verification and release gates
~~~

The complete gate requires Go 1.26.4+, Node 22.6+, Python 3, and the Pi
adapter's installed development dependencies:

~~~sh
(cd adapters/pi && npm ci)
./scripts/verify.sh
~~~

Before npm publication, build every platform binary and inspect the actual
tarball:

~~~sh
./scripts/release-check.sh
~~~

The standalone executable supports capture, search, get, status, catalog,
sync (with a `--json` form for embedding hosts), reseal, capture-default
selection (`default`), alias maintenance,
and version reporting. It defaults to the same
host-neutral journal directory as the Pi package; `--root` and owner
configuration override it.

Released versions are recorded in [`CHANGELOG.md`](CHANGELOG.md). Licensed
MIT.
