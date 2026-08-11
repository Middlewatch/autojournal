// How the corpus is entered: the sharded layout below the journal root, the
// symlink-refusing owner-only descent, atomic temp-write and directory fsync,
// the containment vocabulary, and both contained readers. Everything here is
// capability 3 (paths and containment) mechanics that store, search, and index
// previously carried piecemeal; paths.go answers where things live, this file
// answers how the tree at those paths is safely entered.
//
// Two requirements drive the mechanics, and the Go stdlib does not offer
// either primitive directly:
//
//   - Publication must never replace an existing episode, because first-write-
//     wins is what makes redelivery idempotent. os.Rename always replaces, so
//     publication uses link(2), which fails atomically with EEXIST when the
//     target exists. A crash between link and unlink can leave an orphan temp
//     file; it is invisible to CountEpisodes and to readers, and the next
//     publish retries a fresh temp name.
//   - Descent must never follow a symlink. os.Root confines resolution to the
//     tree but would still follow a link planted inside it, so each step
//     Lstats first and refuses anything that is not a real directory. The
//     residual check-then-open race requires an attacker who already holds
//     write access inside the owner-only corpus.

package autojournal

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

const (
	// corpusDirPermissions is enforced on every directory descent.
	corpusDirPermissions = 0o700
	// corpusFilePermissions is the episode file mode.
	corpusFilePermissions = 0o600
)

// Store publish failures. The CLI maps each to its contract outcome.
var (
	// ErrContainmentViolation means a path component inside the corpus is
	// a symlink or not a directory.
	ErrContainmentViolation = errors.New("containment violation")
	// ErrPermissionDenied maps to the permission_denied outcome.
	ErrPermissionDenied = errors.New("permission denied")
	// ErrStoreUnavailable is any other I/O failure or sync uncertainty;
	// the caller may retry — idempotency makes redelivery safe.
	ErrStoreUnavailable = errors.New("store unavailable")
	// errTempCollision retries with the next temp name suffix.
	errTempCollision = errors.New("temp name collision")
)

// ErrEvidenceUnavailable is any failure to read an episode file under
// containment. Search folds it into EditedExcluded; Get folds it into the
// gone outcome.
var ErrEvidenceUnavailable = errors.New("evidence unavailable")

// corpusError classifies an I/O error into the store's failure vocabulary,
// keeping the OS detail in the message.
func corpusError(context string, err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s: %w: %v", context, ErrPermissionDenied, err)
	default:
		return fmt.Errorf("%s: %w: %v", context, ErrStoreUnavailable, err)
	}
}

// OpenJournalRoot opens the journal root for publishing, creating it if
// absent and enforcing owner-only permissions. Intermediate directories of a
// freshly created root keep default permissions (0o755 before umask) and only
// the root itself is hardened: the root is where episodes live, and tightening
// the owner's unrelated parent directories on the way past would be overreach.
func OpenJournalRoot(path string) (*os.Root, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, corpusError("create journal root", err)
		}
	} else if err != nil {
		return nil, corpusError("open journal root", err)
	}
	if err := os.Chmod(path, corpusDirPermissions); err != nil {
		return nil, corpusError("harden journal root", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, corpusError("open journal root", err)
	}
	return root, nil
}

// layoutComponents builds the directory components below the journal
// root: reserved classification directories for non-default world, scope,
// and lane, then the YYYY/MM/DD shard from the source event time.
func layoutComponents(payload *Payload) []string {
	var components []string
	if payload.World != "main" {
		components = append(components, "worlds", payload.World)
	}
	if payload.Scope != "default" {
		components = append(components, "scopes", payload.Scope)
	}
	if payload.Lane != LaneConversation {
		components = append(components, "lanes", string(payload.Lane))
	}
	// The conversion below wraps for values at or above 2^63, producing a
	// pre-epoch or negative-year shard. Validate is where that is refused;
	// this line trusts it and must not be read as proof it cannot happen.
	t := time.UnixMilli(int64(payload.EventTimeMs)).UTC()
	components = append(components,
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", int(t.Month())),
		fmt.Sprintf("%02d", t.Day()),
	)
	return components
}

// openComponents descends the component list from root, creating and
// hardening each level, and returns the final directory. Intermediate
// directories are closed on the way down.
func openComponents(root *os.Root, components []string) (*os.Root, error) {
	current := root
	ownsCurrent := false // the caller's root is never closed here
	for _, component := range components {
		next, err := openOrCreateChild(current, component)
		if err != nil {
			if ownsCurrent {
				current.Close()
			}
			return nil, err
		}
		if ownsCurrent {
			current.Close()
		}
		current = next
		ownsCurrent = true
	}
	return current, nil
}

// syncCreatedDir is the ancestor-durability point, indirected so the shard
// witness can count a sync per created level. Production never rebinds it.
var syncCreatedDir = syncDir

// openOrCreateChild opens a direct child directory, creating it
// owner-only if absent. A symlink or non-directory component is a
// containment violation. Concurrent creators are tolerated.
func openOrCreateChild(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := parent.Mkdir(name, corpusDirPermissions); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, corpusError("create corpus dir "+name, err)
		}
		// The new entry must be durable before anything beneath it is
		// reported durable: fsync the parent that carries it.
		// Without this a reported capture success is merely
		// reachable-on-most-filesystems, not durable.
		if err := syncCreatedDir(parent); err != nil {
			return nil, corpusError("sync parent of corpus dir "+name, err)
		}
		// Re-check: a concurrent creator may have made a non-directory.
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, corpusError("inspect corpus dir "+name, err)
	}
	if !info.IsDir() {
		// Lstat does not follow links, so a symlink reports !IsDir and is
		// refused here — a planted link cannot redirect a write out of the
		// corpus.
		return nil, fmt.Errorf("corpus component %s: %w", name, ErrContainmentViolation)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, corpusError("open corpus dir "+name, err)
	}
	if err := child.Chmod(".", corpusDirPermissions); err != nil {
		child.Close()
		return nil, corpusError("harden corpus dir "+name, err)
	}
	return child, nil
}

// writeTemp creates the temp file exclusively with owner-only
// permissions, writes the content, and fsyncs. A failed write removes the
// temp it created: the name embeds the capture time, so an orphan would
// make an identical redelivery fail its exclusive create until the
// attempt suffix moves on.
func writeTemp(dir *os.Root, tmpName string, content []byte) error {
	f, err := dir.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, corpusFilePermissions)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errTempCollision
		}
		return corpusError("create temp", err)
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			dir.Remove(tmpName)
		}
	}()
	if _, err := f.Write(content); err != nil {
		return corpusError("write temp", err)
	}
	if err := f.Sync(); err != nil {
		return corpusError("sync temp", err)
	}
	ok = true
	return nil
}

// syncDir fsyncs a directory, making the entry changes inside it durable.
func syncDir(dir *os.Root) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ContainedPath validates the journal-relative path vocabulary: relative
// validated components only, no dot components, no Windows separators.
func ContainedPath(relPath string) bool {
	if relPath == "" || relPath[0] == '/' {
		return false
	}
	for _, component := range strings.Split(relPath, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		if strings.ContainsRune(component, '\\') {
			return false
		}
	}
	return true
}

// ReadContained reads one episode file under the journal root with
// containment: relative validated components only, no symlink following,
// resolution stays beneath the root. Descent Lstats each component
// (refusing symlinks and non-directories) before opening, the same
// nofollow discipline as store.go's write path.
func ReadContained(root *os.Root, relPath string) (string, error) {
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("%w: %s: %v", ErrEvidenceUnavailable, relPath, err)
	}
	if !ContainedPath(relPath) {
		return fail(errors.New("path outside the containment vocabulary"))
	}
	components := strings.Split(relPath, "/")
	current := root
	ownsCurrent := false
	for i, component := range components {
		info, err := current.Lstat(component)
		if err != nil {
			if ownsCurrent {
				current.Close()
			}
			return fail(err)
		}
		last := i == len(components)-1
		if !last {
			// Lstat does not follow links, so a symlink reports !IsDir and
			// is refused here — the same containment rule the write path
			// applies, enforced on read.
			if !info.IsDir() {
				if ownsCurrent {
					current.Close()
				}
				return fail(errors.New("component is not a directory"))
			}
			next, err := current.OpenRoot(component)
			if err != nil {
				if ownsCurrent {
					current.Close()
				}
				return fail(err)
			}
			if ownsCurrent {
				current.Close()
			}
			current = next
			ownsCurrent = true
			continue
		}
		// Close only descent-owned handles: for a single-component path
		// current is still the caller's root and must stay open.
		if ownsCurrent {
			defer current.Close()
		}
		if !info.Mode().IsRegular() {
			return fail(errors.New("not a regular file"))
		}
		f, err := current.Open(component)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		content, err := io.ReadAll(io.LimitReader(f, MaxEpisodeFileBytes+1))
		if err != nil {
			return fail(err)
		}
		if len(content) > MaxEpisodeFileBytes {
			return fail(errors.New("episode exceeds byte budget"))
		}
		return string(content), nil
	}
	// Unreachable: ContainedPath guarantees at least one component.
	return fail(errors.New("empty path"))
}

// WalkKind is what one WalkCorpus visit is reporting.
type WalkKind int

const (
	// WalkEpisode: a regular file whose name is an episode file name.
	WalkEpisode WalkKind = iota
	// WalkUnreadableDir: a directory the walk could not read and skipped.
	// Counted as a distinct exclusion so freshness cannot report fresh over
	// content nobody can see.
	WalkUnreadableDir
	// WalkShardDir: a visible directory the walk is about to descend into,
	// reported before its entries are read. Sync repairs permissions here —
	// which is what lets an owner-owned unreadable directory self-heal
	// before the read that would otherwise report it WalkUnreadableDir —
	// and every counting caller ignores it. info is nil.
	WalkShardDir
)

// WalkCorpus is the single visibility rule every corpus traversal shares:
// dot-directories are foreign tooling state and are skipped, symlinks are
// not followed, descent stops CorpusWalkDepth components below the root, and
// only files named <IDPrefix>*.md are episodes. An error returned by visit
// stops the walk and is returned. An unreadable root is returned as an
// error; an unreadable subtree is reported as WalkUnreadableDir and skipped.
//
// visit receives the entry's FileInfo, obtained from the walk's fs.DirEntry
// via d.Info(). Be honest about what that costs: on Linux over an os-backed
// FS, Info() is an lstat per entry, so this is one extra syscall per episode
// file, not a free hand-off. It is here because CorpusSignatureOf needs the
// mtime and would otherwise Stat every entry a second time — one syscall
// instead of two, which is the whole cost argument for putting the signature
// walk on the query path.
//
// info is computed only for WalkEpisode, so CountEpisodes pays nothing it does
// not pay today; it is nil for WalkUnreadableDir and WalkShardDir. An Info()
// failure is treated
// exactly as an unreadable entry: the file is skipped and not reported, which
// is the right answer for a file removed between the directory read and the
// stat.
func WalkCorpus(root *os.Root, visit func(relPath string, kind WalkKind, info fs.FileInfo) error) error {
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == "." {
				return err
			}
			if d != nil && d.IsDir() {
				if verr := visit(path, WalkUnreadableDir, nil); verr != nil {
					return verr
				}
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			name := d.Name()
			if name == "" || name[0] == '.' {
				return fs.SkipDir
			}
			if strings.Count(path, "/")+1 > CorpusWalkDepth {
				return fs.SkipDir
			}
			return visit(path, WalkShardDir, nil)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, IDPrefix) || !strings.HasSuffix(name, ".md") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		return visit(path, WalkEpisode, info)
	})
}

// CorpusSignature is a stat-only summary of the corpus: how many episode
// files are visible and the newest modification time among them. It costs
// one Lstat per entry and no file reads, which is what search already pays
// today to count episodes.
type CorpusSignature struct {
	Episodes   uint64
	MaxMtimeMs uint64
}

// CorpusSignatureOf walks the corpus with WalkCorpus, stating each episode
// file and reading none. A pre-1970 mtime contributes nothing to the
// maximum rather than wrapping the unsigned field.
func CorpusSignatureOf(root *os.Root) (CorpusSignature, error) {
	var sig CorpusSignature
	err := WalkCorpus(root, func(path string, kind WalkKind, info fs.FileInfo) error {
		if kind != WalkEpisode {
			return nil
		}
		sig.Episodes++
		if ms := info.ModTime().UnixMilli(); ms > 0 && uint64(ms) > sig.MaxMtimeMs {
			sig.MaxMtimeMs = uint64(ms)
		}
		return nil
	})
	if err != nil {
		return CorpusSignature{}, err
	}
	return sig, nil
}

// readRootFile reads one confined file with a byte budget; over-budget is
// an error, never a truncation.
func readRootFile(root *os.Root, path string, maxBytes int64) ([]byte, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	return content, nil
}
