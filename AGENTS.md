# Working on AutoJournal

Guidance for working within this repository.

## Scope

AutoJournal does two things: durably write completed agent turns as episodes,
and retrieve exact, ranked, bounded evidence from them.

## Architecture

One Go implementation of every product rule, exposed in three forms:

- a Go module (`github.com/Middlewatch/autojournal/src`, package
  `autojournal`) that an embedding host imports directly.
- a standalone static CLI (`src/cmd/autojournal/`), which is both the owner
  CLI and the hook target for any harness without a native integration.
- the thin TypeScript Pi adapter (`adapters/pi/`, the npm package), which
  supervises the CLI and invents no memory policy.

Adapters translate; they never reimplement storage, ranking, identity, or
freshness. If a change would put a product rule in an adapter, it belongs in
`src/` instead.

## What is frozen

Stability comes in three tiers:

- **Corpus-durable**: episode Markdown and frontmatter bytes, episode-id
  derivation, payload-digest derivation. Changing any of these is a major
  version. These are what make an existing corpus readable by a future binary.
- **Interface**: the CLI `--json` surface and the config file. Additions are
  minor: new fields, new config keys, new values in an existing typed
  vocabulary. Removals, renames, and meaning changes are major.
- **Derived**: the SQLite projection, its schema, and its location. Not a
  contract. A version bump disposes and rebuilds from Markdown.

`testdata/payloads` and `testdata/golden` pin the corpus-durable tier: episode
bytes, identity and digest vectors, publish paths, config rewrites, and CLI
`--json` ops samples.

## Conventions

- gofmt is law. `go vet ./...` and `go test -race ./...` are green before
  every commit.
- `modernc.org/sqlite` (pure Go, no cgo) is the only direct Go module
  dependency. Keep the direct dependency surface narrow. The binary ships
  with no external runtime dependencies, and that is a product property
  rather than an accident.
- Layout: Go code lives in `src/` and `src/cmd/autojournal/`; only
  `go.mod`/`go.sum` sit at the repo root. No new top-level subtrees.
- Write walkable code with package-level doc comments explaining why a module
  exists, not just what it does.

## Gates

```sh
(cd adapters/pi && npm ci) # once per dependency or lockfile change
./scripts/verify.sh        # the full repository gate
./scripts/release-check.sh # verify + cross-compile + npm package layout
```

`verify.sh` runs gofmt, `go vet`, race-enabled tests, five bounded
parse-boundary fuzz steps, a host binary build, the adapter typecheck and
tests, the Python hook suite (which replays the recorded transcript fixtures),
the cross-adapter conformance suite, and an end-to-end
capture → search → get → `stale_revision` → `no_match` smoke in an isolated
root. CI runs this same script rather than a re-listed subset, so one
command reproduces it locally; CI adds only what a single machine cannot do,
namely cross-compilation, npm package layout, a four-target binary end-to-end
matrix, and a weekly long-fuzz job over the same five targets. A change is done
when the local gate and applicable CI jobs are green.

Where the conformance harness lives, since it is not under a `tests/`
directory: `testdata/payloads` and `testdata/golden` are the fixture corpus,
`testdata/transcripts` holds the redacted real-transcript recordings with their
pinned projections, `adapters/test_python_hooks.py` is the stdlib-only hook
suite, `adapters/test_conformance.py` drives every adapter over the shared
cases in `adapters/conformance_cases.json`, and `verify.sh` itself drives the
built binary through its real CLI for the end-to-end cases. The fuzz harness is
`src/fuzz_test.go`: five functions turn bytes this package did not produce into
structured values (`ParsePayload`, `ParseConfig`, `ParseEpisode`,
`LoadAliasMapFromBytes`, and `CursorDecode`), and each is a native fuzz target
asserting round-trip and containment invariants, not absence of panic, because
the defects found at those boundaries all parsed cleanly and produced a wrong
value. Seeds live under `src/testdata/fuzz`, from the existing fixtures plus
one named regression seed per defect found at that boundary.

## Releases

The npm package and the bundled core binary share one version stamp, asserted
in three places and cross-checked by `adapters/pi/scripts/check-package.mjs`:
`package.json`, `ADAPTER_VERSION` in `adapters/pi/index.ts`, and
`PackageVersion` in `src/doc.go`.

Between releases the top `CHANGELOG.md` entry carries the pending version and
the word `unreleased`, and work accumulates under it. Cutting a release means
replacing that word with the date, in the commit that ships it rather than
after publishing. The changelog ships inside the package and npm versions are
immutable, so an entry published as "unreleased" can only be corrected by
burning another version number.

The packaging self-check refuses to pack while the entry is undated, so
`release-check.sh` fails by design until the release is actually being cut.
That failure is the reminder, not a broken gate. The order is: date the
entry → `verify.sh` → `release-check.sh` → `npm publish` → tag the commit
that shipped.

`adapter_version` is written into episode frontmatter but deliberately
excluded from the payload digest, so a version bump never re-identifies or
re-publishes existing episodes.

## Handoff

Run the gate and `git diff --check`, and report the exact Git status. Do not
claim capture or retrieval behavior that a test or a golden comparison has not
demonstrated. This project stores other people's work, and an unverified
claim about durability or recall is worse than an open bug.
