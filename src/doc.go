// Package autojournal owns every AutoJournal product rule: capture
// contracts, episode identity and rendering, atomic publication, the
// disposable search index, and ranked retrieval. Harness adapters (the Pi
// extension, hook scripts, an embedding host like Evoker) translate
// lifecycle events and register tools; they never reimplement storage,
// identity, ranking, or freshness rules.
//
// The package lives in src/ because this repository predates the Go port:
// src/ held the Zig modules, and the owner ruled (2026-08-03) that the Go
// rebuild happen in place with no new top-level subtrees. The directory
// name not matching the package name is deliberate; go tooling resolves
// the import path github.com/Middlewatch/autojournal/src to this package.
//
// Port discipline: the archived Zig implementation (git tag zig-final) is
// the behavioral spec. On-disk formats
// (episode Markdown, frontmatter, index schema) and identity/digest
// derivation are frozen — a Go-rendered episode is byte-identical to the
// Zig-rendered one, verified against testdata/golden.
package autojournal

// PackageVersion is the released package identity the CLI prints; the
// reference took it from build.zig.zon.
const PackageVersion = "1.0.3"
