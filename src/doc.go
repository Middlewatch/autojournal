// Package autojournal owns AutoJournal's capture, storage, index, retrieval,
// and owner-operation contracts. It remains under src so embedding hosts and
// the standalone binary use the same published import path:
// github.com/Middlewatch/autojournal/src.
//
// The golden fixtures under testdata are the behavioral authority for this
// package: they pin the on-disk formats, identity derivation, and CLI output
// that the corpus-durable tier freezes. Where the fixtures and the
// tests are silent, decide what is correct and add a fixture — there is no
// second authority to appeal to.
package autojournal

// PackageVersion is the product release version. It is separate from the
// immutable wire schema versions and must match the CLI and adapter manifests.
const PackageVersion = "1.1.1"
