# Contributing to AutoJournal

Contributions are welcome. This file is the shortest path from a fresh clone
to a change that can be reviewed.

## Build and test

You need Go 1.26.4+, Node 22.6+, and Python 3. With those installed:

```sh
(cd adapters/pi && npm ci)   # once, and after any lockfile change
./scripts/verify.sh          # the full repository gate
```

`verify.sh` is the definition of green: gofmt, `go vet`, the race-enabled Go
suites, five bounded parse-boundary fuzz steps, a host binary build, the
adapter typecheck and tests, the Python hook suite, the cross-adapter
conformance suite, and an end-to-end capture → search → get smoke in an
isolated root. CI runs this same script, so a local pass is what CI will say.

For day-to-day Go work, `go test -race ./...` is the fast loop; run the full
gate before opening a pull request.

## The rules a change has to respect

- The golden fixtures under `testdata/` are the behavioral authority: they
  pin the on-disk episode format, identity derivation, and the CLI `--json`
  surface. Extend the matrix when you change CLI-observable behavior; never
  weaken it. Where the fixtures and tests are silent, decide what is correct
  and add a fixture.
- Adapters translate; they never reimplement storage, ranking, identity, or
  freshness. A product rule belongs in `src/`, not in an adapter.
- Episode Markdown bytes, episode-id derivation, and payload-digest
  derivation are frozen within a major version: an existing corpus must stay
  readable by every future 1.x binary.
- `modernc.org/sqlite` is the core's only direct Go dependency, and the
  binary ships with no external runtime dependencies. Keep it that way.
- `gofmt` is law.

## Proposing a change

Open an issue or pull request against the repository. A pull request is
expected to pass `./scripts/verify.sh` and to say what it changed about
observable behavior, if anything. Feature work that touches the capture or
retrieval contract should also update the fixture matrix and the changelog's
unreleased entry.

Releases are cut by the maintainers: the top `CHANGELOG.md` entry is dated,
the version stamps in `package.json`, `adapters/pi/index.ts`, and
`src/doc.go` are bumped together, and `scripts/release-check.sh` inspects
the actual npm tarball before anything is published.
