# Contributing to AutoJournal

Contributions are welcome. This file is the shortest path from a fresh
clone to a change that can be reviewed.

## Build and test

You need Node 22.6+. With that installed:

```sh
npm ci                 # once, and after any lockfile change
./scripts/verify.sh    # the full repository gate
```

`verify.sh` is the definition of green: the typecheck, the whole node:test
suite (golden byte pins, conformance cases, parse-boundary properties over
the pinned fuzz seeds, store/index/retrieval behavior, the CLI wire
shapes, the extension in-process), and an end-to-end
capture → search → get smoke through the node bin in an isolated root. CI
runs this same script, so a local pass is what CI will say.

For day-to-day work, `npm test` is the fast loop; run the full gate before
opening a pull request.

## The rules a change has to respect

- The golden fixtures under `testdata/` are the behavioral authority: they
  pin the on-disk episode format, identity derivation, and the CLI
  `--json` surface. Extend the matrix when you change CLI-observable
  behavior; never weaken it. Where the fixtures and tests are silent,
  decide what is correct and add a fixture.
- The extension and the CLI translate; they never reimplement storage,
  ranking, identity, or freshness. A product rule belongs in `src/`.
- Episode Markdown bytes, episode-id derivation, and payload-digest
  derivation are frozen within a major version: an existing corpus must
  stay readable by every future build of the same major.
- The package ships zero runtime dependencies, and the engine uses only
  the Node standard library. Keep it that way.
- One capability per `src/` module; `test/engine/ownership.test.ts` holds
  the map.

## Proposing a change

Open an issue or pull request against the repository. A pull request is
expected to pass `./scripts/verify.sh` and to say what it changed about
observable behavior, if anything. Feature work that touches the capture or
retrieval contract should also update the fixture matrix and the
changelog's unreleased entry.

Releases are cut by the maintainers: the top `CHANGELOG.md` entry is
dated, the version stamps in `package.json`, `index.ts`, and `cli.ts` are
bumped together, and `scripts/release-check.sh` checks the version stamps,
the dated changelog, and the npm pack layout before anything is published.
