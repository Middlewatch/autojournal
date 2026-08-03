// Atomic episode publication under a contained journal root.
//
// Default layout: YYYY/MM/DD/<episode-id>.md. Non-default classifications
// add reserved worlds/, scopes/, and lanes/ components before the date.
// Each episode is one immutable file per completed turn. Publication is:
// exclusive owner-only temp file in the target directory → write → fsync →
// atomic no-replace hard-link into place → temp unlink → parent directory
// fsync. An existing target is validated by digest: exact duplicate is
// success, mismatch is a typed conflict.
//
// Containment: every component below the root is either a validated token
// or generated here, and every operation goes through os.Root with a
// symlink-refusing Lstat check per descent step, so a link planted inside
// the corpus cannot redirect writes outside it.
//
// Two notes on the mechanics versus the Zig reference (behavior is
// identical; the primitives differ because the Go stdlib exposes no
// no-replace rename):
//
//   - The reference publishes with renameat2(RENAME_NOREPLACE). Go's
//     os.Rename always replaces, so publication here uses link(2), which
//     fails atomically with EEXIST when the target exists — the same
//     no-replace guarantee from a different syscall. A crash between link
//     and unlink can leave an orphan temp file; it is invisible to
//     CountEpisodes and to readers, and the next publish retries a fresh
//     temp name.
//   - The reference refuses to follow symlinks per descent step with
//     openat(O_NOFOLLOW). os.Root confines resolution to the tree but
//     would follow an in-corpus link, so each descent step first Lstats
//     and refuses anything that is not a real directory. A planted link
//     is rejected exactly as the reference rejects it; the residual
//     check-then-open race window requires an attacker who already holds
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
	// storeDirPermissions is enforced on every directory descent.
	storeDirPermissions = 0o700
	// storeFilePermissions is the episode file mode.
	storeFilePermissions = 0o600
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

// storeError classifies an I/O error into the store's failure vocabulary,
// keeping the OS detail in the message.
func storeError(context string, err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s: %w: %v", context, ErrPermissionDenied, err)
	default:
		return fmt.Errorf("%s: %w: %v", context, ErrStoreUnavailable, err)
	}
}

// Published is the result of one Publish call.
type Published struct {
	// Outcome is CapturePublished, CaptureDuplicate, or CaptureConflict.
	Outcome   CaptureOutcome
	EpisodeID string
	DigestHex string
	// RelPath is the episode path relative to the journal root,
	// slash-joined (the path vocabulary of evidence references).
	RelPath string
	// Content is the rendered episode bytes, so the capture path can
	// index without re-reading the file it just wrote.
	Content []byte
}

// OpenJournalRoot opens the journal root for publishing, creating it if
// absent and enforcing owner-only permissions — the reference CLI's
// openOrCreateRoot. Intermediate directories of a freshly created root
// get default permissions (0o755 before umask), as in the reference; only
// the root itself is hardened.
func OpenJournalRoot(path string) (*os.Root, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, storeError("create journal root", err)
		}
	} else if err != nil {
		return nil, storeError("open journal root", err)
	}
	if err := os.Chmod(path, storeDirPermissions); err != nil {
		return nil, storeError("harden journal root", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, storeError("open journal root", err)
	}
	return root, nil
}

// Publish publishes one validated payload into the journal root. The
// world subtree is created on demand with owner-only permissions.
func Publish(root *os.Root, payload *Payload, captureTimeMs uint64) (*Published, error) {
	episodeID := EpisodeID(payload)
	digestHex := PayloadDigestHex(payload)
	content := Render(RenderInput{
		Payload:       payload,
		EpisodeID:     episodeID,
		DigestHex:     digestHex,
		CaptureTimeMs: captureTimeMs,
	})

	components := layoutComponents(payload)
	episodeDir, err := openComponents(root, components)
	if err != nil {
		return nil, err
	}
	defer episodeDir.Close()

	finalName := episodeID + ".md"
	// The temp name embeds the capture time and an attempt counter; a
	// collision (orphan from a crashed writer) retries a fresh name.
	tmpName := ""
	written := false
	for attempt := 0; attempt < 64 && !written; attempt++ {
		tmpName = fmt.Sprintf(".%s.%d.%d.tmp", episodeID, captureTimeMs, attempt)
		err := writeTemp(episodeDir, tmpName, content)
		switch {
		case errors.Is(err, errTempCollision):
			continue
		case err != nil:
			return nil, err
		default:
			written = true
		}
	}
	if !written {
		return nil, fmt.Errorf("temp name: %w: 64 collisions", ErrStoreUnavailable)
	}
	// Whatever happens next, the temp file does not outlive this call.
	defer episodeDir.Remove(tmpName)

	outcome := CapturePublished
	linkErr := episodeDir.Link(tmpName, finalName)
	switch {
	case linkErr == nil:
		// published
	case errors.Is(linkErr, fs.ErrExist):
		outcome, err = classifyExisting(episodeDir, finalName, digestHex)
		if err != nil {
			return nil, err
		}
	default:
		return nil, storeError("link episode", linkErr)
	}

	// Make the directory entry durable before reporting success.
	if err := syncDir(episodeDir); err != nil {
		return nil, storeError("sync episode dir", err)
	}

	relPath := strings.Join(append(components, finalName), "/")
	return &Published{
		Outcome:   outcome,
		EpisodeID: episodeID,
		DigestHex: digestHex,
		RelPath:   relPath,
		Content:   content,
	}, nil
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
	// Input is unsigned, so pre-epoch times cannot occur by construction.
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

// openOrCreateChild opens a direct child directory, creating it
// owner-only if absent. A symlink or non-directory component is a
// containment violation. Concurrent creators are tolerated.
func openOrCreateChild(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := parent.Mkdir(name, storeDirPermissions); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, storeError("create corpus dir "+name, err)
		}
		// Re-check: a concurrent creator may have made a non-directory.
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, storeError("inspect corpus dir "+name, err)
	}
	if !info.IsDir() {
		// Lstat does not follow links, so a symlink reports !IsDir and
		// is refused here — the reference's O_NOFOLLOW descent.
		return nil, fmt.Errorf("corpus component %s: %w", name, ErrContainmentViolation)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, storeError("open corpus dir "+name, err)
	}
	if err := child.Chmod(".", storeDirPermissions); err != nil {
		child.Close()
		return nil, storeError("harden corpus dir "+name, err)
	}
	return child, nil
}

// writeTemp creates the temp file exclusively with owner-only
// permissions, writes the content, and fsyncs. A failed write removes the
// temp it created: the name embeds the capture time, so an orphan would
// make an identical redelivery fail its exclusive create until the
// attempt suffix moves on.
func writeTemp(dir *os.Root, tmpName string, content []byte) error {
	f, err := dir.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFilePermissions)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errTempCollision
		}
		return storeError("create temp", err)
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			dir.Remove(tmpName)
		}
	}()
	if _, err := f.Write(content); err != nil {
		return storeError("write temp", err)
	}
	if err := f.Sync(); err != nil {
		return storeError("sync temp", err)
	}
	ok = true
	return nil
}

// classifyExisting decides duplicate versus conflict by comparing the
// stored episode's frontmatter digest against the incoming payload
// digest. The existing file's permissions are repaired to owner-only on
// the way, matching the reference.
func classifyExisting(dir *os.Root, finalName, digestHex string) (CaptureOutcome, error) {
	f, err := dir.Open(finalName)
	if err != nil {
		return "", storeError("open existing episode", err)
	}
	defer f.Close()
	if err := f.Chmod(storeFilePermissions); err != nil {
		return "", storeError("harden existing episode", err)
	}
	existing, err := io.ReadAll(io.LimitReader(f, MaxEpisodeFileBytes+1))
	if err != nil {
		return "", storeError("read existing episode", err)
	}
	if len(existing) > MaxEpisodeFileBytes {
		return "", storeError("read existing episode", fmt.Errorf("exceeds %d bytes", MaxEpisodeFileBytes))
	}
	existingDigest, ok := FrontmatterDigestHex(string(existing))
	if !ok {
		return CaptureConflict, nil
	}
	if existingDigest == digestHex {
		return CaptureDuplicate, nil
	}
	return CaptureConflict, nil
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

// CountEpisodes counts authoritative-looking episode files under the
// journal root. Diagnostics only; malformed candidates are excluded by
// sync. The walk follows the index's visibility rules: dot-directories
// are foreign tooling state, never episode shards; symlinks are not
// followed; descent stops CorpusWalkDepth components below the root.
func CountEpisodes(root *os.Root) uint64 {
	var total uint64
	// The walk is diagnostics-best-effort: any read failure skips that
	// subtree, matching the reference's error-swallowing iterator.
	fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
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
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, IDPrefix) && strings.HasSuffix(name, ".md") {
			total++
		}
		return nil
	})
	return total
}
