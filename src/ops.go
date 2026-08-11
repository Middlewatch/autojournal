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
	"io/fs"
	"os"
	"strings"
	"time"
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
	// A missing database is not_built, never mistaken for empty memory.
	if _, err := os.Stat(indexPath); err != nil {
		episodes := CountEpisodes(root)
		return Status{RootOK: true, Episodes: episodes, Freshness: IndexNotBuilt}
	}
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		episodes := CountEpisodes(root)
		return Status{RootOK: true, Episodes: episodes, Freshness: IndexUnavailable}
	}
	defer idx.Close()
	// The one signal serves status and search alike: both derive
	// freshness from (*Index).Freshness and nothing else, so the two
	// reporters cannot disagree about the same corpus. Deliberate
	// exclusions count as accounted for inside it, or they would read as
	// staleness no sync can ever clear.
	fresh, err := idx.Freshness(root, uint64(time.Now().UnixMilli()))
	if err != nil {
		return Status{RootOK: true, Episodes: CountEpisodes(root), Freshness: IndexUnavailable}
	}
	return Status{RootOK: true, Episodes: fresh.Source, Indexed: fresh.Indexed, Freshness: fresh.Freshness}
}

// Sync failure vocabulary, one sentinel per typed compatibility error.
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

	digest := RootDigestHex(rootPath)
	report, err := idx.syncFromCorpus(root, digest)
	if err != nil {
		return SyncReport{}, ErrSyncFailed
	}
	if err := HardenIndexFiles(indexPath); err != nil {
		return SyncReport{}, ErrIndexUnavailable
	}
	return report, nil
}

// ResealReport is the reseal accounting: Scanned counts every visible
// episode file, Resealed the digest-stale files re-attested (or, under
// preview, that would be), Refused the files that no longer parse as an
// episode — or cannot be read, or whose digest line cannot be located —
// and are left untouched. WriteFailures counts files reseal tried to
// rewrite and could not (I/O): the sweep continues past them so one bad
// shard costs one file, and the terminal sync still runs, but the caller
// must treat any nonzero count as a failed invocation. Paths lists the
// resealed (or would-be-resealed) files.
type ResealReport struct {
	Scanned       uint64
	Resealed      uint64
	Refused       uint64
	WriteFailures uint64
	Paths         []string
}

// Reseal re-attests owner-edited episodes: a digest-stale file gets
// its payload_digest line rewritten to ResealDigestHex's chosen reading,
// through the same owner-only temp-write and atomic rename discipline
// capture uses, then one sync rebaselines the projection. A file that
// verifies is skipped; a file that no longer parses is counted and left
// untouched — reseal re-attests a well-formed edit, never repairs a broken
// file. Under preview it counts and lists and writes nothing, and the
// projection is not touched.
func Reseal(rootPath, indexPath string, preview bool) (ResealReport, error) {
	var report ResealReport
	if RootInSharedDirectory(rootPath) {
		return report, ErrSharedDirectory
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return report, ErrRootMissing
	}
	defer root.Close()
	walkErr := WalkCorpus(root, func(path string, kind WalkKind, info fs.FileInfo) error {
		if kind != WalkEpisode {
			return nil
		}
		report.Scanned++
		content, err := readRootFile(root, path, MaxEpisodeFileBytes)
		if err != nil {
			report.Refused++
			return nil
		}
		if _, verr := VerifyEpisode(string(content)); verr == nil {
			return nil
		} else if !errors.Is(verr, ErrDigestMismatch) {
			report.Refused++
			return nil
		}
		digest, ok := ResealDigestHex(string(content))
		if !ok {
			report.Refused++
			return nil
		}
		rewritten, ok := rewriteDigestLine(string(content), digest)
		if !ok {
			report.Refused++
			return nil
		}
		if !preview {
			if err := resealWrite(root, path, []byte(rewritten)); err != nil {
				report.WriteFailures++
				return nil
			}
		}
		report.Resealed++
		report.Paths = append(report.Paths, path)
		return nil
	})
	if walkErr != nil {
		return report, walkErr
	}
	if !preview {
		if _, err := Sync(rootPath, indexPath); err != nil {
			return report, err
		}
	}
	return report, nil
}

// rewriteDigestLine replaces the recorded digest hex on the frontmatter's
// payload_digest line, leaving every other byte untouched. False when the
// content carries no parseable digest line — a state Reseal counts as
// refused rather than repairs.
func rewriteDigestLine(content, newHex string) (string, bool) {
	oldHex, ok := FrontmatterDigestHex(content)
	if !ok {
		return "", false
	}
	oldLine := "payload_digest: " + DigestPrefix + oldHex
	newLine := "payload_digest: " + DigestPrefix + newHex
	if !strings.Contains(content, oldLine) {
		return "", false
	}
	return strings.Replace(content, oldLine, newLine, 1), true
}

// resealWrite replaces one episode file in place: exclusive owner-only
// temp in the episode's own directory, fsync, rename over the final name,
// directory fsync — the same discipline capture's supersede path uses, so
// a torn reseal can never leave a half-written episode.
func resealWrite(root *os.Root, relPath string, content []byte) error {
	components := strings.Split(relPath, "/")
	dirComponents := components[:len(components)-1]
	name := components[len(components)-1]
	dir, err := openComponents(root, dirComponents)
	if err != nil {
		return err
	}
	if len(dirComponents) > 0 {
		defer dir.Close()
	}
	tmpName := "." + name + ".reseal.tmp"
	// A leftover temp is a crashed reseal's garbage — its content was
	// never renamed into place — and under the one-writer rule nothing
	// else owns the name. Removing it first keeps one old crash from
	// hard-failing every future reseal on the exclusive create.
	_ = dir.Remove(tmpName)
	if err := writeTemp(dir, tmpName, content); err != nil {
		return err
	}
	defer dir.Remove(tmpName)
	if err := dir.Rename(tmpName, name); err != nil {
		return corpusError("reseal episode", err)
	}
	return syncDir(dir)
}

// CountEpisodes counts authoritative-looking episode files under the
// journal root. Diagnostics only; malformed candidates are excluded by
// sync. The walk is WalkCorpus's visibility rule: dot-directories
// are foreign tooling state, never episode shards; symlinks are not
// followed; descent stops CorpusWalkDepth components below the root.
func CountEpisodes(root *os.Root) uint64 {
	var total uint64
	// Diagnostics-best-effort by contract: a read failure skips that subtree
	// rather than failing the count, because a corpus statistic must never be
	// the thing that breaks recall. Both of WalkCorpus's unreadable signals
	// are deliberately discarded here to preserve that contract — an
	// unreadable subtree is invisible to this count, and the accounting for
	// it starts elsewhere. Note the cost: this count and sync agree on a
	// total that omits such a subtree.
	WalkCorpus(root, func(relPath string, kind WalkKind, info fs.FileInfo) error {
		if kind == WalkEpisode {
			total++
		}
		return nil
	})
	return total
}

// Catalog lists the world/scope pairs an owner can select: the configured
// capture default pair first, then every pair the projection knows,
// deduplicated in discovery order. A missing or unopenable index yields the
// default pair alone — catalog is a convenience view, never an error path.
func Catalog(rootPath, indexPath string, defaults CaptureDefaults) []WorldScope {
	pairs := []WorldScope{{World: defaults.World, Scope: defaults.Scope}}
	if _, err := os.Stat(indexPath); err != nil {
		return pairs
	}
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndexHardened(indexPath, &digest)
	if err != nil {
		return pairs
	}
	defer idx.Close()
	rows, err := idx.WorldScopePairs()
	if err != nil {
		return pairs
	}
	for _, row := range rows {
		exists := false
		for _, pair := range pairs {
			if pair.World == row.World && pair.Scope == row.Scope {
				exists = true
				break
			}
		}
		if !exists {
			pairs = append(pairs, row)
		}
	}
	return pairs
}
