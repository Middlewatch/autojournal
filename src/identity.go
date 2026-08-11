// Episode identity and canonical payload digests.
//
// The episode ID is the idempotency identity: source harness, session,
// turn, world, and capture policy version. Re-delivering the same identity
// with the same payload digest is a duplicate (success); the same identity
// with a different digest is a conflict.
//
// The payload digest covers canonical identity metadata plus the body and
// excludes capture-run metadata (capture time, adapter version, provenance)
// so that a faithful re-delivery hashes identically regardless of when or
// by which adapter build it arrives.
//
// Both derivations are corpus-durable contracts, pinned in both directions by
// testdata/golden. Changing either re-identifies every existing episode and
// invalidates every outstanding evidence reference, which is a major version.

package autojournal

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"strconv"
)

// IDPrefix starts every episode ID.
const IDPrefix = "aj1-"

// EpisodeIDLen is prefix plus 32 hex chars (128 bits of the identity hash).
const EpisodeIDLen = len(IDPrefix) + 32

// DigestPrefix starts every rendered digest value.
const DigestPrefix = "sha256:"

// DigestHexLen is the hex length of a full SHA-256 sum.
const DigestHexLen = 64

// EpisodeID derives the collision-resistant idempotency identity: SHA-256
// over a version tag followed by 0x00-separated identity fields, truncated
// to the first 16 bytes of the sum and hex-encoded.
func EpisodeID(p *Payload) string {
	h := sha256.New()
	io.WriteString(h, "autojournal-episode-id.v1")
	for _, field := range []string{p.Harness, p.SessionID, p.TurnID, p.World, p.CapturePolicy} {
		h.Write([]byte{0})
		io.WriteString(h, field)
	}
	sum := h.Sum(nil)
	return IDPrefix + hex.EncodeToString(sum[:16])
}

// PayloadDigestHex derives the canonical payload digest — the revision
// identity used by evidence references. The input is length-prefix framed
// so no content bytes can be confused with framing. Every field that
// participates is versioned under the leading tag; changing the set of
// fields requires a new tag.
func PayloadDigestHex(p *Payload) string {
	h := sha256.New()
	io.WriteString(h, "autojournal-digest.v1")
	hashField(h, p.World)
	hashField(h, p.Scope)
	hashField(h, string(p.Lane))
	hashField(h, p.Harness)
	hashField(h, p.SessionID)
	hashField(h, p.TurnID)
	hashField(h, strconv.FormatUint(p.EventTimeMs, 10))
	hashField(h, p.CapturePolicy)
	hashField(h, p.TurnOutcome)
	hashField(h, p.UserContent)
	hashField(h, p.AssistantResult)
	hashField(h, strconv.Itoa(len(p.Tools)))
	for _, t := range p.Tools {
		hashField(h, t.Name)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashField frames one field as 0x00, decimal byte length, 0x00, bytes.
// The length is the UTF-8 byte length, not the rune count: framing counts the
// bytes actually hashed, so no content can be confused with its own framing.
func hashField(h hash.Hash, s string) {
	h.Write([]byte{0})
	io.WriteString(h, strconv.Itoa(len(s)))
	h.Write([]byte{0})
	io.WriteString(h, s)
}
