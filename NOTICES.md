# Notices

AutoJournal 2.x ships no third-party code: the engine, the CLI, and the Pi
extension are first-party TypeScript with zero runtime dependencies beyond
the Node.js standard library. The `typebox` and
`@earendil-works/pi-coding-agent` packages are peer dependencies resolved
by the installing host, not redistributed here.

Versions 1.x bundled prebuilt Go binaries that statically linked
third-party modules; the attributions for those releases ship inside their
own published packages.
