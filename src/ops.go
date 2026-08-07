// Journal maintenance: the two whole operations a host exposes to its
// owner beyond capture and recall.
//
// Status and Sync are more than thin wrappers over store and index —
// each carries accounting that is easy to get subtly wrong. Freshness
// must add deliberate exclusions to the indexed count or a corpus with
// one duplicate reads as permanently stale; sync must re-stamp the root
// identity and record its exclusion count or the next status contradicts
// it. Those rules live here so the owner CLI and an embedding host
// cannot disagree about whether memory is healthy.

package autojournal

import (
	"errors"
	"os"
	"strconv"
)

// Status is one journal health report.
type Status struct {
	// RootOK is false when the journal root does not exist yet. Not an
	// error: a harness that has captured nothing has no root, and
	// reporting zero episodes against a missing root is the honest answer.
	RootOK bool
	// Episodes is the episode files found by walking the corpus.
	Episodes uint64
	// Indexed is the episode rows the projection holds.
	Indexed   uint64
	Freshness IndexFreshness
}

// Healthy is true when the projection can answer recall completely.
// Stale and unavailable both mean a sync is owed.
func (s Status) Healthy() bool {
	return s.Freshness == IndexFresh
}

// StatusOf is read-only: it never creates the root, the index, or their
// parents, so a status check cannot itself change what it reports.
func StatusOf(rootPath, indexPath string) Status {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return Status{Freshness: IndexNotBuilt}
	}
	defer root.Close()
	episodes := CountEpisodes(root)

	// A missing database is not_built, never mistaken for empty memory.
	if _, err := os.Stat(indexPath); err != nil {
		return Status{RootOK: true, Episodes: episodes, Freshness: IndexNotBuilt}
	}
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		return Status{RootOK: true, Episodes: episodes, Freshness: IndexUnavailable}
	}
	defer idx.Close()
	indexed, err := idx.EpisodeCount()
	if err != nil {
		indexed = 0
	}
	// Files the last sync deliberately excluded (duplicate ids,
	// malformed) count as accounted for, or they read as staleness that
	// no sync can ever clear.
	freshness := IndexStale
	if indexed+idx.ExcludedCount() == episodes {
		freshness = IndexFresh
	}
	return Status{RootOK: true, Episodes: episodes, Indexed: indexed, Freshness: freshness}
}

// Redelivery is a prior capture of the same episode identity.
type Redelivery struct {
	// Outcome is CaptureDuplicate when the stored digest matches the
	// payload, CaptureConflict when the identity was redelivered with
	// different content.
	Outcome CaptureOutcome
	// RelPath is where the existing episode lives.
	RelPath string
}

// CheckRedelivery is the corpus-wide redelivery check for capture. The
// store detects a redelivered identity only when it lands on the same
// event-date path; an identity redelivered with a different event time
// would shard to another date and silently store twice. The index knows
// every shard, so it answers "does this episode id exist anywhere" — but
// the file it names stays the authority: the outcome is classified from
// that file's own frontmatter, and any index miss, stale row, unreadable
// file, or identity mismatch returns nil so the caller proceeds to
// publish (the store's own same-path check still applies).
func CheckRedelivery(root *os.Root, idx *Index, payload *Payload) *Redelivery {
	episodeID := EpisodeID(payload)
	digestHex := PayloadDigestHex(payload)
	row, err := idx.LookupEpisode(episodeID)
	if err != nil || row == nil {
		return nil
	}
	content, err := ReadContained(root, row.RelPath)
	if err != nil {
		return nil
	}
	ep := ParseEpisode(content)
	if ep == nil || ep.EpisodeID != episodeID {
		return nil
	}
	outcome := CaptureConflict
	if ep.DigestHex == digestHex {
		outcome = CaptureDuplicate
	}
	return &Redelivery{Outcome: outcome, RelPath: row.RelPath}
}

// Sync failure vocabulary, one sentinel per typed reference error.
var (
	// ErrSharedDirectory: the journal root sits under a group- or
	// world-writable directory.
	ErrSharedDirectory = errors.New("journal root sits under a shared directory")
	// ErrRootMissing: the root does not exist; there is nothing to sync.
	ErrRootMissing = errors.New("journal root does not exist")
	// ErrIndexUnavailable: the projection could not be opened or its
	// permissions not narrowed.
	ErrIndexUnavailable = errors.New("projection unavailable")
	// ErrSyncFailed: the rebuild failed and was rolled back; the
	// projection is unchanged.
	ErrSyncFailed = errors.New("sync failed and was rolled back")
)

// Sync brings the projection up to date with the corpus and re-stamps
// its identity. Unchanged files are skipped by digest match; new,
// edited, and moved files are (re)indexed.
//
// Opened without the foreign-root gate on purpose: sync replaces
// whatever projection is at indexPath with this root's content, which is
// the documented way to repoint an index.
func Sync(rootPath, indexPath string) (SyncReport, error) {
	if RootInSharedDirectory(rootPath) {
		return SyncReport{}, ErrSharedDirectory
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return SyncReport{}, ErrRootMissing
	}
	defer root.Close()
	// Harden through the handle (fchmod), not the path: a path re-resolve
	// after open could be swapped to another directory.
	if err := root.Chmod(".", 0o700); err != nil {
		return SyncReport{}, ErrIndexUnavailable
	}

	idx, err := OpenIndexHardened(indexPath, nil)
	if err != nil {
		return SyncReport{}, ErrIndexUnavailable
	}
	defer idx.Close()

	report, err := idx.SyncFromCorpus(root)
	if err != nil {
		return SyncReport{}, ErrSyncFailed
	}
	digest := RootDigestHex(rootPath)
	_ = idx.metaSet("root_digest", digest)
	excluded := report.DuplicateIDs + report.SkippedMalformed
	_ = idx.metaSet("sync_excluded", strconv.FormatUint(excluded, 10))
	if err := HardenIndexFiles(indexPath); err != nil {
		return SyncReport{}, ErrIndexUnavailable
	}
	return report, nil
}
