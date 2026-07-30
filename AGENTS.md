# AutoJournal work lane

This repository is the canonical implementation lane for AutoJournal 1.0.

- Design authority: `docs/AUTOJOURNAL_1_0_DESIGN.md`. User-facing search
  behavior and thesaurus curation: `docs/SEARCH_TUNING.md`.
- Product scope is durable completed-turn writes and ranked, bounded retrieval.
  Memory curation, reflection, wiki maintenance, and generated durable claims
  belong to a separate future product and must not be added here.
- Architecture: all Zig. One package builds as a module Evoker imports
  natively and as a standalone static binary (framed-stdio helper + owner CLI
  + hook target). A thin TypeScript Pi adapter supervises the binary and is
  the npm package.
- Toolchain: Zig 0.16.0 through `./scripts/zig.sh`. `./scripts/verify.sh` is
  the gate: fmt check, full test suite, build, design-contract presence, and
  an end-to-end capture→search→get smoke against the installed binary.
- The deployed TypeScript v1 extension remains the live Pi writer until
  cutover. Do not repoint the live harness or modify the live journal from
  this repository.
- Evoker engine changes belong in the Evoker repository and must follow that
  repository's charter. Do not privately patch Evoker API contracts from
  here.
- Pushing and publication timing are Jake's decision. One writer at a time in
  this repository.

Before handoff, run `./scripts/verify.sh` and `git diff --check`, and report
the exact Git status. Do not claim capture or retrieval behavior that a test
or smoke run has not demonstrated.
