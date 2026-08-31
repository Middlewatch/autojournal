# 0002: A stdlib snapshot projection replaces the SQLite index

Date: 2026-08-31
Status: accepted

## Context

The SQLite projection was the repository's only direct dependency, chosen
when the index served a Go binary. A TypeScript engine (ADR 0001) would need
either a native module (breaking install-anywhere) or a WASM build (a heavy
dependency) to keep it. Evoker's journal, facing the same choice, measured a
pure-stdlib snapshot index against this owner's real 4,176-episode, 23 MiB
corpus: cold sync 0.40 s, warm search 0.09 s, 17 MiB snapshot.

## Decision

The index is a versioned single-file snapshot built with the Node standard
library: writers serialize on a lock file, snapshots replace by atomic
rename, readers only ever open a complete snapshot. It remains the Derived
tier — disposable, never a contract, rebuilt from Markdown on any version
mismatch.

## Consequences

Zero runtime dependencies, native or npm, so the package runs wherever Node
runs. Incremental indexing becomes our code instead of SQL, and freshness
moves to a stat-walk signature. If the corpus outgrows in-memory scoring,
the escalation is streamed postings over the same snapshot, not a return to
SQLite.
