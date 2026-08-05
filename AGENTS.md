# Working on AutoJournal

Guidance for anyone — human or coding agent — changing this repository. The
codebase map is [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md); the binding
product contract and the reasoning behind it is
[`docs/AUTOJOURNAL_1_0_DESIGN.md`](docs/AUTOJOURNAL_1_0_DESIGN.md); user-facing
search behavior and thesaurus curation is
[`docs/SEARCH_TUNING.md`](docs/SEARCH_TUNING.md). All three are as-built.

## Scope

AutoJournal does two things: durably write completed agent turns as episodes,
and retrieve exact, ranked, bounded evidence from them. Memory curation,
reflection, wiki maintenance, and model-generated durable claims are out of
scope by design — that is a separate product, not a feature to add here. The
full non-goals list is in the design doc, and it is a real boundary, not a
placeholder.

## Architecture

One Go implementation of every product rule, consumed three ways:

- a Go module (`github.com/Middlewatch/autojournal/src`, package
  `autojournal`) that an embedding host imports directly;
- a standalone static CLI (`src/cmd/autojournal/`), which is both the owner
  CLI and the hook target for any harness without a native integration;
- the thin TypeScript Pi adapter (`adapters/pi/`, the npm package), which
  supervises the CLI and invents no memory policy.

Adapters translate; they never reimplement storage, ranking, identity, or
freshness. If a change would put a product rule in an adapter, it belongs in
`src/` instead.

## What is frozen

Within 1.x, these are contracts and changing them is a major version: episode
Markdown and frontmatter bytes, episode-id and digest derivation, the index
schema, the config file, and the CLI `--json` surface. Harness adapters must
not observe a behavior change from an ordinary release.

`testdata/payloads` and `testdata/golden` pin that behavior — episode bytes,
identity and digest vectors, publish paths, config rewrites, and CLI `--json`
ops samples. Extend the matrix whenever you change CLI-observable behavior;
never weaken it. The fixtures were frozen from the Zig implementation this
core was ported from (git tag `zig-final`), which remains the tiebreaker for
any behavioral question the tests do not already answer.

## Conventions

- gofmt is law. `go vet ./...` and `go test -race ./...` are green before
  every commit.
- `modernc.org/sqlite` (pure Go, no cgo) is the only dependency; everything
  else is stdlib. Keep it that way — the binary ships with no runtime
  dependencies, and that is a product property, not an accident.
- Layout: Go code lives in `src/` and `src/cmd/autojournal/`; only
  `go.mod`/`go.sum` sit at the repo root. No new top-level subtrees.
- Write walkable code with package-level doc comments explaining why a module
  exists, not just what it does.

## Gates

```sh
./scripts/verify.sh        # the full repository gate
./scripts/release-check.sh # verify + cross-compile + npm package layout
```

`verify.sh` runs gofmt, `go vet`, race-enabled tests, a host binary build, the
adapter typecheck and tests, design-contract greps, and an end-to-end
capture → search → get → `stale_revision` → `no_match` smoke in an isolated
root. CI runs the same pipeline plus cross-compilation and a four-target
end-to-end matrix. A change is done when the gate is green.

## Releases

The npm package and the bundled core binary share one version stamp, asserted
in three places and cross-checked by `adapters/pi/scripts/check-package.mjs`:
`package.json`, `ADAPTER_VERSION` in `adapters/pi/index.ts`, and
`PackageVersion` in `src/doc.go`.

Between releases the top `CHANGELOG.md` entry carries the pending version and
the word `unreleased`, and work accumulates under it. Cutting a release means
replacing that word with the date, in the commit that ships it — not after
publishing. The changelog ships inside the package and npm versions are
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
demonstrated — this project stores other people's work, and an unverified
claim about durability or recall is worse than an open bug.
