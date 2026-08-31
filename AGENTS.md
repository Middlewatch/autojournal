# Working on AutoJournal

Guidance for working within this repository.

## Scope

AutoJournal does two things: durably write completed agent turns as episodes,
and retrieve exact, ranked, bounded evidence from them.

## Architecture

One TypeScript engine (`src/`), exposed in three forms from one source tree:

- the engine library, imported by everything else in this package;
- the Pi extension (`index.ts`, the npm package's entry), which runs the
  engine in-process — capture at settle, the `memory_search`/`memory_get`
  tools, the `/autojournal` menu, and session-history import;
- the owner CLI (`cli.ts`, installed as the `autojournal` bin), thin wiring
  over the same engine with the `--json` interface contract.

Surfaces translate; they never reimplement storage, ranking, identity, or
freshness. If a change would put a product rule in `index.ts` or `cli.ts`,
it belongs in `src/` instead. One capability owns each `src/` module and
`test/engine/ownership.test.ts` pins the map; `docs/ARCHITECTURE.md` is the
orientation document.

## What is frozen

Stability comes in three tiers:

- **Corpus-durable**: episode Markdown and frontmatter bytes, episode-id
  derivation, payload-digest derivation. Changing any of these is a major
  version. These are what make an existing corpus readable by a future
  build — including every corpus the 1.x Go engine wrote.
- **Interface**: the CLI `--json` surface and the config file. Additions
  are minor: new fields, new config keys, new values in an existing typed
  vocabulary. Removals, renames, and meaning changes are major.
- **Derived**: the snapshot index, its format, and its location. Not a
  contract. A format-version bump disposes and rebuilds from Markdown.

`testdata/payloads` and `testdata/golden` pin the corpus-durable tier:
episode bytes, identity and digest vectors, publish paths, config rewrites,
and the CLI `--json` ops samples (re-mintable on purpose via
`AUTOJOURNAL_MINT_OPS_SAMPLES=1`, never by accident).

## Conventions

- Strict TypeScript, type-stripped (no build step): parameter properties
  and other non-erasable syntax are off the table. `npm run typecheck` and
  `npm test` are green before every commit.
- Zero runtime dependencies is a product property, not an accident. The
  engine uses only the Node standard library; typebox and the Pi SDK are
  the extension's dev/peer surface.
- Layout: engine modules in `src/`, every test under `test/`, fixtures
  under `testdata/`. No new top-level subtrees.
- Write walkable code with module-level header comments explaining why a
  module exists, not just what it does.

## Gates

```sh
npm ci                     # once per dependency or lockfile change
./scripts/verify.sh        # the full repository gate
./scripts/release-check.sh # verify + version stamps + npm pack layout
```

`verify.sh` runs the typecheck, the whole node:test suite, and an
end-to-end capture → search → get → `stale_revision` → `no_match` smoke
through the node bin in an isolated root. CI runs this same script on
Linux and Windows rather than a re-listed subset, plus a weekly long
randomized run of the parse-boundary properties. A change is done when the
local gate and applicable CI jobs are green.

Where things live, since the suite is not split by tool: `test/engine/`
holds the engine suites (golden pins in `golden.test.ts`, the shared
conformance corpus driven by `conformance.test.ts` over
`testdata/conformance_cases.json`, and `properties.test.ts`, which runs
the parse-boundary round-trip and containment invariants over the pinned
seeds in `testdata/fuzz` plus a bounded deterministic mutation loop —
`AUTOJOURNAL_PROPERTY_ITERS` raises the budget). `test/` holds the CLI,
ops-golden, extension, and import suites. A defect found at a parse
boundary gets a named regression seed under `testdata/fuzz`, because the
defects found there all parsed cleanly and produced a wrong value.

## Releases

The npm package carries one version stamp, asserted in three places and
cross-checked by `scripts/check-package.mjs`: `package.json`,
`ADAPTER_VERSION` in `index.ts`, and `CLI_VERSION` in `cli.ts`.

Between releases the top `CHANGELOG.md` entry carries the pending version
and the word `unreleased`, and work accumulates under it. Cutting a
release means replacing that word with the date, in the commit that ships
it rather than after publishing. The changelog ships inside the package
and npm versions are immutable, so an entry published as "unreleased" can
only be corrected by burning another version number.

The packaging self-check refuses to pack while the entry is undated, so
`release-check.sh` fails by design until the release is actually being
cut. That failure is the reminder, not a broken gate. The order is: date
the entry → `verify.sh` → `release-check.sh` → `npm publish` → tag the
commit that shipped.

`adapter_version` is written into episode frontmatter but deliberately
excluded from the payload digest, so a version bump never re-identifies or
re-publishes existing episodes.

## Handoff

Run the gate and `git diff --check`, and report the exact Git status. Do
not claim capture or retrieval behavior that a test or a golden comparison
has not demonstrated. This project stores other people's work, and an
unverified claim about durability or recall is worse than an open bug.
