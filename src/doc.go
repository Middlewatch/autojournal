// Package autojournal owns AutoJournal's capture, storage, index, retrieval,
// and owner-operation contracts. It remains under src so embedding hosts and
// the standalone binary use the same published import path:
// github.com/Middlewatch/autojournal/src.
//
// The archived Zig implementation at git tag zig-final is the compatibility
// oracle. Golden fixtures verify that Go preserves its frozen on-disk formats,
// identity rules, and CLI output.
package autojournal

// PackageVersion is the product release version. It is separate from the
// immutable wire schema versions and must match the CLI and adapter manifests.
const PackageVersion = "1.0.4"
