# AutoJournal work lane

This repository is the canonical implementation lane for AutoJournal 1.0.

- Design authority: `docs/AUTOJOURNAL_1_0_DESIGN.md`. User-facing search
  behavior and thesaurus curation: `docs/SEARCH_TUNING.md`. Both still say
  "Zig" — they describe behavior that survives the port and are revised to
  as-built Go reality at cutover, not mid-port.
- Product scope is durable completed-turn writes and ranked, bounded
  retrieval. Memory curation, reflection, wiki maintenance, and generated
  durable claims belong to a separate future product and must not be added
  here.

## Architecture (owner ruling 2026-08-03)

All Go. One implementation of every product rule, consumed three ways:

- a Go module (`github.com/Middlewatch/autojournal`, package `autojournal`
  in `src/`) that Evoker imports natively for its bundled, toggleable
  memory extension;
- a standalone static CLI (`src/cmd/autojournal/`) that is the owner CLI
  and the hook target for every other harness, cross-compiled for the npm
  package's four targets (linux x64/arm64, macOS x64/arm64);
- the thin TypeScript Pi adapter (`adapters/pi/`, the npm package), which
  supervises the CLI and invents no memory policy.

The on-disk contracts are frozen across the port: episode Markdown and
frontmatter bytes, episode-id/digest derivation, index schema, config file,
and the CLI `--json` surface. Harness adapters (Pi TS, Python hooks) must
not observe a behavior change.

## Port discipline

- The archived Zig implementation is the behavioral spec: git tag
  `zig-final`, tree + static binary at `~/projects/zig-reference/autojournal/`.
  The live binary cut over to the Go build 2026-08-03
  (`~/.local/bin/autojournal` → this repo's `artifacts/autojournal`).
- `testdata/payloads` + `testdata/golden` pin the oracle's capture
  behavior (episode bytes, identity/digest vectors, publish paths,
  config rewrites, ops samples). Extend
  the matrix when porting a module that has CLI-observable behavior; never
  weaken it.
- The port is complete and golden-verified end to end: contracts,
  identity, render, frontmatter, paths, config, store, db, index,
  retrieval, aliases, search, ops, and the CLI, whose `--json` output is
  byte-compared against the oracle's ops samples. `modernc.org/sqlite`
  (pure Go, no cgo) is the only dependency; everything else is stdlib.
- Layout constraint (owner): no new top-level subtrees, no `go/` dir. Go
  code lives in `src/` (package `autojournal`) and `src/cmd/autojournal/`;
  only `go.mod`/`go.sum` sit at the repo root.
- House Go rules match `~/projects/evoker/main/docs/GO_GUIDE.md`:
  gofmt is law; `go vet ./...` and `go test -race ./...` green before
  every commit; walkable code with package/owner doc comments (Jake is
  learning Go through these builds).

## Gates and cutover

- Interim gate (until the CLI exists): `gofmt -l .`, `go vet ./...`,
  `go test -race ./...`. `scripts/verify.sh`, `scripts/release-check.sh`,
  and `.github/workflows/` still reference the Zig toolchain — rewrite
  them when the Go CLI lands; they are known-stale, not the gate.
- Cutover (one ordered change, after the CLI passes parity): repoint
  `~/.local/bin/autojournal` at the Go build, verify all three live
  harnesses (Pi extension + Claude Code hook + Codex hook), run
  `/autojournal sync`, then revise user docs (`README.md`s, `docs/`) to
  as-built Go reality and update `DEPLOY_MANIFEST.toml`.
- Pushing and publication timing are Jake's decision. One writer at a time
  in this repository.

Before handoff, run the interim gate and `git diff --check`, and report
the exact Git status. Do not claim capture or retrieval behavior that a
test or oracle comparison has not demonstrated.
