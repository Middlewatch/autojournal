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
// The descent, temp-write, and fsync mechanics publication relies on live in
// corpus.go with the rest of the containment discipline; this file owns the
// publication decision itself.

package autojournal

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// publishWriteTemp and publishSyncDir are Publish's two durability points —
// the temp-file fsync and the parent-directory fsync — held as package
// variables so the supersede durability witness can observe that the rename
// path exercises exactly the fsyncs the link path does. Production never
// rebinds them.
var (
	publishWriteTemp = writeTemp
	publishSyncDir   = syncDir
	// publishRename is the supersede replacement step, indirected with the
	// fsync points so the witness can assert ordering: the directory fsync
	// is only worth having if it lands after the entry change it makes
	// durable.
	publishRename = func(dir *os.Root, oldname, newname string) error {
		return dir.Rename(oldname, newname)
	}
)

// Published is the result of one Publish call.
type Published struct {
	// Outcome is CapturePublished, CaptureDuplicate, CaptureSuperseded,
	// or CaptureConflict.
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
		err := publishWriteTemp(episodeDir, tmpName, content)
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
		outcome, err = classifyExisting(episodeDir, finalName, payload, digestHex)
		if err != nil {
			return nil, err
		}
		if outcome == CaptureSuperseded {
			// The redelivery proved it contains the stored content:
			// replace in place at the episode's own path. The temp is
			// already fsynced, and the directory fsync below makes the
			// replacement entry durable; the superseded bytes are not
			// retained anywhere.
			if err := publishRename(episodeDir, tmpName, finalName); err != nil {
				return nil, corpusError("supersede episode", err)
			}
		}
	default:
		return nil, corpusError("link episode", linkErr)
	}

	// Make the directory entry durable before reporting success.
	if err := publishSyncDir(episodeDir); err != nil {
		return nil, corpusError("sync episode dir", err)
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

// classifyExisting decides duplicate, superseded, or conflict for a target
// that already exists at the derived path: it verifies the stored file and,
// when it verifies, tests containment against the incoming payload.
//
// This is where the supersede decision lives, and it deliberately does not
// consult the index. Supersede requires every path-determining field to match, so a
// genuine supersede candidate always lands on the stored episode's own path,
// and the link collision Publish already detects is the only signal needed.
// Supersede therefore works against a missing, stale, or foreign projection —
// which matters, because the alternative would make correcting a settled turn
// depend on index health.
//
// The order of its three tests is part of the contract, because two of them
// can be true of one file and they return different exit codes:
//
//  1. The stored file's *recorded* digest equals digestHex -> duplicate.
//     Deliberately first: an exact redelivery of an episode the owner has
//     since hand-edited stays a duplicate, which is the answer that keeps
//     redelivery idempotent.
//  2. Otherwise verify the stored file. If it verifies and supersedes holds ->
//     superseded.
//  3. Otherwise conflict. This covers a stored file that does not verify,
//     which is never superseded on any path.
//
// The existing file's permissions are repaired to owner-only on the way:
// owner-only is a standing invariant, and a redelivery is a free opportunity
// to fix a file that lost it.
func classifyExisting(dir *os.Root, finalName string, incoming *Payload, digestHex string) (CaptureOutcome, error) {
	f, err := dir.Open(finalName)
	if err != nil {
		return "", corpusError("open existing episode", err)
	}
	defer f.Close()
	if err := f.Chmod(corpusFilePermissions); err != nil {
		return "", corpusError("harden existing episode", err)
	}
	existing, err := io.ReadAll(io.LimitReader(f, MaxEpisodeFileBytes+1))
	if err != nil {
		return "", corpusError("read existing episode", err)
	}
	if len(existing) > MaxEpisodeFileBytes {
		return "", corpusError("read existing episode", fmt.Errorf("exceeds %d bytes", MaxEpisodeFileBytes))
	}
	existingDigest, ok := FrontmatterDigestHex(string(existing))
	if ok && existingDigest == digestHex {
		return CaptureDuplicate, nil
	}
	stored, verifyErr := VerifyEpisode(string(existing))
	if verifyErr == nil && supersedes(incoming, stored) {
		return CaptureSuperseded, nil
	}
	return CaptureConflict, nil
}

// supersedes reports whether incoming is the same turn as stored at a later
// stage of completion. Every field the payload digest covers must be
// identical — world, scope, lane, harness, session id, turn id, event time,
// capture policy, turn outcome, user content — except the two that are
// allowed to grow: stored's assistant result must be a strict prefix of
// incoming's, and stored's tool-name list a prefix of incoming's.
//
// Requiring the event time, scope and lane to match is not belt-and-braces:
// they determine the layout path. A redelivery with a different event time
// shards to another date, publication succeeds rather than colliding, and the
// corpus gains a second file claiming one episode id — the outcome the one-file-per-identity rule rejects,
// reached by accident. A redelivery that shards elsewhere is a conflict.
//
// Anything else is divergence and stays a conflict. Length is not evidence of
// sameness and recency is not evidence of quality; this function is the whole
// of the store's judgment and it is mechanical.
func supersedes(incoming *Payload, stored *VerifiedEpisode) bool {
	if incoming.World != stored.World ||
		incoming.Scope != stored.Scope ||
		incoming.Lane != stored.Lane ||
		incoming.Harness != stored.Harness ||
		incoming.SessionID != stored.SessionID ||
		incoming.TurnID != stored.TurnID ||
		incoming.EventTimeMs != stored.EventTimeMs ||
		incoming.CapturePolicy != stored.CapturePolicy ||
		incoming.TurnOutcome != stored.TurnOutcome ||
		incoming.UserContent != stored.UserContent {
		return false
	}
	if len(incoming.AssistantResult) <= len(stored.AssistantResult) ||
		!strings.HasPrefix(incoming.AssistantResult, stored.AssistantResult) {
		return false
	}
	if len(stored.Tools) > len(incoming.Tools) {
		return false
	}
	for i, tool := range stored.Tools {
		if incoming.Tools[i].Name != tool.Name {
			return false
		}
	}
	return true
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

// CheckRedelivery is the corpus-wide existence check for capture. It exists
// because the store detects a redelivered identity only when it lands on the
// same event-date path, so an identity redelivered with a different event
// time would otherwise shard elsewhere and store twice. The index knows
// every shard, so it answers "does this episode id exist anywhere" — but
// the file it names stays the authority: the outcome is classified from
// that file's own frontmatter, and any index miss, stale row, unreadable
// file, or identity mismatch returns nil so the caller proceeds to
// publish (the store's own same-path check still applies).
//
// It returns nil — proceed to publish — in one more case: the stored episode
// is at the very path this payload derives, and its digest differs. That is
// the supersede candidate, and only Publish's own same-path classification
// can rule on it. Without this, a strict extension would be reported
// conflict before Publish is ever called, because an extended assistant
// result changes the digest by construction; supersede would be unreachable.
// Everything else is unchanged: an exact digest match anywhere is duplicate,
// and a differing digest at a *different* path is a conflict, because supersede
// requires every path-determining field to match.
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
	if ep.DigestHex == digestHex {
		return &Redelivery{Outcome: CaptureDuplicate, RelPath: row.RelPath}
	}
	derived := strings.Join(append(layoutComponents(payload), episodeID+".md"), "/")
	if row.RelPath == derived {
		return nil
	}
	return &Redelivery{Outcome: CaptureConflict, RelPath: row.RelPath}
}

// CaptureRequest is one whole capture transaction's input.
//
// Note Defaults' type: config.go's CaptureDefaults (renamed from `type
// Capture`, because src/ is one package and the entry point below
// needs the name Capture).
type CaptureRequest struct {
	RootPath      string
	IndexPath     string
	Raw           RawPayload
	Defaults      CaptureDefaults // owner capture defaults, for world/scope fill
	CaptureTimeMs uint64
}

// CaptureResult is the transaction's typed outcome. Err is nil for every
// success outcome; Detail carries CaptureErrorName(Err) when it is not.
type CaptureResult struct {
	Outcome    CaptureOutcome
	EpisodeID  string
	DigestHex  string
	RelPath    string
	IndexState IndexFreshness
	Err        error
	Detail     string
}

// Capture composes the whole transaction so the imported module form and the
// binary run the same code rather than the same intent: defaults fill,
// Validate, root canonicalization, shared-directory refusal, corpus-wide
// redelivery classification, atomic publication, index update, and the
// index-failure freshness downgrade. Source publication succeeding while
// indexing fails is a success with a downgraded IndexState, never a failure.
//
// The order is part of the contract, not an implementation detail: an
// embedding host that reordered it would report different failures for the
// same input. Shared-directory refusal is decided before the root is opened
// (a refused root is never created), and Validate before either.
func Capture(req CaptureRequest) CaptureResult {
	// Owner-default world/scope fill: a host provides explicit values only
	// when transporting an owner session choice.
	raw := req.Raw
	if raw.World == nil {
		world := req.Defaults.World
		raw.World = &world
	}
	if raw.Scope == nil {
		scope := req.Defaults.Scope
		raw.Scope = &scope
	}
	payload, err := Validate(raw)
	if err != nil {
		return CaptureResult{Outcome: CaptureMalformed, IndexState: IndexNotBuilt,
			Err: err, Detail: CaptureErrorName(err)}
	}

	rootPath := ResolveJournalRoot(req.RootPath)
	if RootInSharedDirectory(rootPath) {
		err := fmt.Errorf("journal root under a shared directory: %w: %w",
			ErrPermissionDenied, ErrSharedDirectory)
		return CaptureResult{Outcome: CapturePermissionDenied, IndexState: IndexNotBuilt,
			Err: err, Detail: CaptureErrorName(err)}
	}
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		return CaptureResult{Outcome: CaptureUnavailable, IndexState: IndexNotBuilt,
			Err: err, Detail: CaptureErrorName(err)}
	}
	defer root.Close()

	// The index is opened once, with the foreign-root gate, and reused for
	// the redelivery check and the post-publish update. Best-effort
	// throughout: an absent or unhelpful index skips the corpus-wide check
	// (the store's own same-path classification still applies) and
	// downgrades freshness after publication.
	digest := RootDigestHex(rootPath)
	idx, indexErr := OpenIndexHardened(req.IndexPath, &digest)
	if idx != nil {
		defer idx.Close()
		if existing := CheckRedelivery(root, idx, &payload); existing != nil {
			state := IndexFresh
			if existing.Outcome != CaptureDuplicate {
				state = IndexStale
			}
			return CaptureResult{
				Outcome:    existing.Outcome,
				EpisodeID:  EpisodeID(&payload),
				DigestHex:  PayloadDigestHex(&payload),
				RelPath:    existing.RelPath,
				IndexState: state,
			}
		}
	}

	published, err := Publish(root, &payload, req.CaptureTimeMs)
	if err != nil {
		outcome := CaptureUnavailable
		switch {
		case errors.Is(err, ErrContainmentViolation):
			outcome = CaptureInternalError
		case errors.Is(err, ErrPermissionDenied):
			outcome = CapturePermissionDenied
		}
		return CaptureResult{Outcome: outcome, IndexState: IndexNotBuilt,
			Err: err, Detail: CaptureErrorName(err)}
	}

	// Source publication is already durable; the index is best-effort here
	// and repairable via sync, so its failure downgrades freshness only and
	// never changes Outcome.
	indexState := IndexStale
	switch published.Outcome {
	case CaptureConflict:
		// A conflict wrote nothing; there is nothing to index.
	case CapturePublished, CaptureDuplicate, CaptureSuperseded:
		switch {
		case idx == nil && errors.Is(indexErr, ErrForeignIndex):
			indexState = IndexUnavailable
		case idx == nil:
			// Unopenable index: publication stands, projection stays stale.
		default:
			if idx.IndexEpisode(published.RelPath, string(published.Content)) == nil &&
				HardenIndexFiles(req.IndexPath) == nil {
				indexState = IndexFresh
			}
		}
	}
	return CaptureResult{
		Outcome:    published.Outcome,
		EpisodeID:  published.EpisodeID,
		DigestHex:  published.DigestHex,
		RelPath:    published.RelPath,
		IndexState: indexState,
	}
}
