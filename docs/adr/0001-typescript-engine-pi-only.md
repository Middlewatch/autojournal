# 0001: TypeScript engine replaces the Go core; Pi is the only harness

Date: 2026-08-31
Status: accepted

## Context

The Go core existed to serve two hosts: a planned native import into the
owner's evoker harness and a portable binary behind harness hooks. Evoker is
shelved (its ADR 0016), evoker built its own journal rather than importing
this module (its ADR 0015), and the claude-code/codex hooks have no ongoing
user. The one live surface is the Pi extension, whose native runtime is
TypeScript, reached today through subprocess supervision of a bundled binary.

## Decision

The engine is rewritten in TypeScript inside this repository and published as
`autojournal` 2.0.0. Pi is the only supported harness. The Go tree, the
claude-code/codex hook adapters, their Python test suites, and the
cross-compile machinery are deleted once the parity gate in the 2026-08-31
port spec is green.

## Consequences

The extension calls the engine in-process: no subprocess seam, no bundled
binaries, no cross-compilation, one language. The corpus-durable tier
(episode bytes, identity, digest) stays frozen, so the existing corpus needs
no migration and `testdata/golden` gates the port byte-for-byte. We give up
the standalone static binary and any non-Pi harness path; reinstating one
would mean rebuilding a hook surface against the npm package's Node CLI.
