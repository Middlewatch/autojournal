// Proven lexical retrieval core: tokenizer, scorer, dedup, confidence,
// cursor. Pure — no I/O and no SQLite. Candidates come in, a ranked and
// deduplicated ordering comes out; search.go owns candidate discovery and
// snippet extraction.
//
// The scoring behavior is settled evidence ported from the deployed
// TypeScript v1 (legacy-ts/src/search.ts): sum(log(N/df)) rarity with
// exact-query duplicate term weights, day-quantized recency as a nudge
// rather than an override, span deduplication, and deterministic
// tie-breaking. Behavioral changes here require a version bump and a new
// parity baseline.

package autojournal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// Version identities. TokenizerVersion gates the index: postings from
// another tokenizer version are disposed. ScorerVersion is stamped on
// every search result; callers may pin it.
//
// aj-scorer.v2 (ratified 2026-08-03, measured against the judged eval
// set in ~/memory/eval/autojournal-retrieval on the origin host) is v1
// ordering plus: additive singular folding of plural query terms, and a
// per-episode cap of MaxPerEpisodeDefault result regions so one long
// episode cannot crowd a page. aj-conf.v2 bands confidence on the
// coverage-adjusted score (score × coverage^ConfidenceCoverageAlpha)
// while ordering stays pure rarity×recency — coverage measurably
// separates spurious partial matches from real hits but does not improve
// ordering itself.
const (
	TokenizerVersion               = "aj-tok.v1"
	ScorerVersion                  = "aj-scorer.v2"
	ConfidencePolicyVersion        = "aj-conf.v2"
	msPerDay                uint64 = 24 * 60 * 60 * 1000
)

// MaxPerEpisodeDefault is the v2 per-episode page cap.
const MaxPerEpisodeDefault = 2

// ConfidenceCoverageAlpha is the exponent on term coverage in aj-conf.v2
// confidence banding. 1.0 (linear discount) is what the calibration runs
// measured: at this corpus's score magnitudes a gentler exponent leaves
// nearly every spurious partial match banded high.
const ConfidenceCoverageAlpha = 1.0

// --- Tokenizer ---

// stopWords is the verbatim port of the v1 stop-word list. The 2-byte
// entries are live on the index side (its token floor is 2, one below the
// query side's 3), so they do filter real index tokens.
var stopWords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "is": {}, "are": {},
	"was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"have": {}, "has": {}, "had": {}, "do": {}, "does": {},
	"did": {}, "will": {}, "would": {}, "could": {}, "should": {},
	"may": {}, "might": {}, "shall": {}, "can": {}, "need": {},
	"dare": {}, "ought": {}, "used": {}, "to": {}, "of": {},
	"in": {}, "for": {}, "on": {}, "with": {}, "at": {},
	"by": {}, "from": {}, "as": {}, "into": {}, "through": {},
	"during": {}, "before": {}, "after": {}, "above": {}, "below": {},
	"between": {}, "out": {}, "off": {}, "over": {}, "under": {},
	"again": {}, "further": {}, "then": {}, "once": {}, "here": {},
	"there": {}, "when": {}, "where": {}, "why": {}, "how": {},
	"all": {}, "both": {}, "each": {}, "few": {}, "more": {},
	"most": {}, "other": {}, "some": {}, "such": {}, "no": {},
	"nor": {}, "not": {}, "only": {}, "own": {}, "same": {},
	"so": {}, "than": {}, "too": {}, "very": {}, "just": {},
	"because": {}, "but": {}, "and": {}, "or": {}, "if": {},
	"while": {}, "about": {}, "up": {}, "down": {}, "what": {},
	"which": {}, "who": {}, "whom": {}, "this": {}, "that": {},
	"these": {}, "those": {}, "i": {}, "me": {}, "my": {},
	"myself": {}, "we": {}, "our": {}, "ours": {}, "ourselves": {},
	"you": {}, "your": {}, "yours": {}, "yourself": {}, "yourselves": {},
	"he": {}, "him": {}, "his": {}, "himself": {}, "she": {},
	"her": {}, "hers": {}, "herself": {}, "it": {}, "its": {},
	"itself": {}, "they": {}, "them": {}, "their": {}, "theirs": {},
	"themselves": {}, "also": {}, "get": {}, "got": {}, "like": {},
	"know": {}, "think": {}, "want": {}, "look": {}, "use": {},
	"find": {}, "give": {}, "tell": {}, "say": {}, "said": {},
	"take": {}, "come": {}, "make": {}, "go": {}, "see": {},
	"thing": {}, "things": {}, "really": {}, "something": {}, "anything": {},
	"remember": {}, "mentioned": {}, "talked": {},
}

// IsStopWord reports whether word (already lowercased) is on the v1 list.
func IsStopWord(word string) bool {
	_, ok := stopWords[word]
	return ok
}

func isTokenByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// IsIndexTokenByte reports whether b belongs to the index-token alphabet.
func IsIndexTokenByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func lowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// Terms is the query-side tokenization: terms in order with duplicates
// preserved (a repeated query word legitimately doubles its weight,
// exactly as v1 scored it).
type Terms struct {
	Items     []string
	Truncated bool // true when the term cap dropped trailing terms
}

// ExtractTerms tokenizes a query: maximal lowercase [a-z0-9_]+ runs,
// dropping runs of two bytes or fewer and stop words. Byte-for-byte
// equivalent to the v1 pipeline (lowercase, strip punctuation to spaces,
// split on whitespace) for all inputs including UTF-8, because non-ASCII
// bytes are separators under both.
func ExtractTerms(query string) Terms {
	buf := []byte(query)
	for i, b := range buf {
		buf[i] = lowerByte(b)
	}
	var items []string
	truncated := false
	i := 0
	for i < len(buf) {
		for i < len(buf) && !isTokenByte(buf[i]) {
			i++
		}
		start := i
		for i < len(buf) && isTokenByte(buf[i]) {
			i++
		}
		word := string(buf[start:i])
		if len(word) <= 2 || IsStopWord(word) {
			continue
		}
		if len(items) >= MaxQueryTerms {
			truncated = true
			break
		}
		items = append(items, word)
	}
	return Terms{Items: items, Truncated: truncated}
}

// TokenizeLine is the index-side tokenization: same alphabet and
// stop-word list as the query side, plus a byte cap that keeps hash blobs
// out of the vocabulary. The length floor is 2 here, one shorter than the
// query side: curated alias values may legitimately be two bytes ("q8"),
// and discovery happens against this vocabulary. Known gap, accepted for
// parity purposes: a query term whose only occurrence on a line is inside
// a stop word or an over-cap token is not discoverable, because such
// tokens are never indexed.
func TokenizeLine(line string) []string {
	var out []string
	i := 0
	for i < len(line) {
		for i < len(line) && !IsIndexTokenByte(line[i]) {
			i++
		}
		start := i
		for i < len(line) && IsIndexTokenByte(line[i]) {
			i++
		}
		raw := line[start:i]
		if len(raw) < 2 || len(raw) > MaxTokenLen {
			continue
		}
		word := strings.ToLower(raw)
		if IsStopWord(word) {
			continue
		}
		out = append(out, word)
	}
	return out
}

// --- Scorer ---

// RecencyMultiplier is 1 + boost/(days+1) day-quantized recency. A nudge,
// not an override; future timestamps get no boost. Day flooring keeps
// identical queries stable within a day.
func RecencyMultiplier(eventTimeMs, nowMs uint64, boost float64) float64 {
	if eventTimeMs > nowMs {
		return 1.0
	}
	days := float64((nowMs - eventTimeMs) / msPerDay)
	return 1.0 + boost/(days+1.0)
}

// IDFWeight is the log(N/df) rarity weight: a term in every episode
// contributes ~0, a term in one episode of many dominates. df == 0 (term
// absent from the candidate corpus) contributes 0, matching v1's
// df.get(t) ?? N.
func IDFWeight(corpusN, df uint64) float64 {
	if df == 0 {
		return 0.0
	}
	n := float64(max(corpusN, 1))
	return math.Log(n / float64(df))
}

// Candidate is one matched body line. MatchedMask has bit i set when
// query term i (by position in the duplicate-preserving term list) occurs
// in the line as a case-insensitive substring — v1's per-line crediting.
type Candidate struct {
	// EpisodeOrd indexes the caller's episode table.
	EpisodeOrd uint32
	// LineNo is the 1-based absolute line number in the episode file.
	LineNo      uint32
	MatchedMask uint64
}

// EpisodeInfo is the per-episode metadata the scorer needs.
type EpisodeInfo struct {
	EpisodeID   string
	RelPath     string
	EventTimeMs uint64
}

// RankParams carries the scoring knobs.
type RankParams struct {
	NowMs         uint64
	RecencyBoost  float64
	MinScore      float64 // 0 disables the relevance floor
	ContextWindow uint32
	// MaxPerEpisode caps how many result regions one episode contributes
	// to the ordering, so a single long episode cannot crowd a page.
	// 0 disables (pre-v2 behavior); Search passes MaxPerEpisodeDefault.
	MaxPerEpisode uint32
}

// Ranked is the scorer's output: Order holds candidate indices, ranked,
// deduplicated, and floored (pagination is a slice of this ordering);
// Scores is parallel to the input candidates array.
type Ranked struct {
	Order  []uint32
	Scores []float64
}

// Rank scores, sorts, and deduplicates candidates. idf is indexed by
// query term position (duplicate terms carry their weight twice, once per
// position). Deterministic: ties break on (rel_path, line_no) so the
// ordering never depends on candidate arrival order.
func Rank(candidates []Candidate, episodes []EpisodeInfo, idf []float64, params RankParams) Ranked {
	scores := make([]float64, len(candidates))
	for i, c := range candidates {
		var rarity float64
		mask := c.MatchedMask
		for mask != 0 {
			bit := uint(bits.TrailingZeros64(mask))
			mask &= mask - 1
			if bit < uint(len(idf)) {
				rarity += idf[bit]
			}
		}
		ep := episodes[c.EpisodeOrd]
		scores[i] = rarity * RecencyMultiplier(ep.EventTimeMs, params.NowMs, params.RecencyBoost)
	}

	order := make([]uint32, len(candidates))
	for i := range order {
		order[i] = uint32(i)
	}
	sort.Slice(order, func(x, y int) bool {
		a, b := order[x], order[y]
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		ca, cb := candidates[a], candidates[b]
		pa, pb := episodes[ca.EpisodeOrd].RelPath, episodes[cb.EpisodeOrd].RelPath
		if pa != pb {
			return pa < pb
		}
		return ca.LineNo < cb.LineNo
	})

	// Span dedup after ranking: the best-scoring line in each
	// context_window*2 line bucket of an episode survives, so adjacent
	// matches collapse to one result region (v1 semantics).
	bucketSpan := uint32(max(uint64(params.ContextWindow)*2, 1))
	seen := make(map[uint64]struct{})
	perEpisode := make(map[uint32]uint32)
	var kept []uint32
	for _, idx := range order {
		c := candidates[idx]
		if params.MinScore > 0 && scores[idx] < params.MinScore {
			continue
		}
		key := uint64(c.EpisodeOrd)<<32 | uint64(c.LineNo/bucketSpan)
		if _, dup := seen[key]; dup {
			continue
		}
		if params.MaxPerEpisode > 0 && perEpisode[c.EpisodeOrd] >= params.MaxPerEpisode {
			continue
		}
		seen[key] = struct{}{}
		perEpisode[c.EpisodeOrd]++
		kept = append(kept, idx)
	}
	return Ranked{Order: kept, Scores: scores}
}

// --- Confidence ---

// Confidence is the versioned band vocabulary (aj-conf.v1).
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// ConfidenceWithCoverage is aj-conf.v2 banding: the score is discounted
// by coverage^ConfidenceCoverageAlpha before banding, so a hit matching
// only a fraction of the query's terms needs a proportionally stronger
// score to earn the same band. Ordering never uses this — it is display
// trust only. Coverage is the matched fraction of query term positions,
// clamped to [0, 1].
func ConfidenceWithCoverage(score, coverage, floor float64) Confidence {
	coverage = min(max(coverage, 0), 1)
	return ConfidenceOf(score*math.Pow(coverage, ConfidenceCoverageAlpha), floor)
}

// ConfidenceOf bands a score off the floor; the floor is the legacy
// weak-query bar. Reported separately from score; a whole-response
// no_match decision stays with the caller's MinScore floor.
func ConfidenceOf(score, floor float64) Confidence {
	if floor <= 0 {
		return ConfidenceHigh
	}
	if score >= 2.0*floor {
		return ConfidenceHigh
	}
	if score >= floor {
		return ConfidenceMedium
	}
	return ConfidenceLow
}

// --- Cursor ---

// Cursor wire shape: "aj1.<offset>.<8 hex guard>".
const (
	CursorPrefix      = "aj1."
	CursorGuardHexLen = 8
	CursorMaxLen      = len(CursorPrefix) + 20 + 1 + CursorGuardHexLen
)

// CursorInputs is the query/world/scorer state a cursor is valid against;
// the guard makes replay against anything else a typed malformed.
type CursorInputs struct {
	Query       string
	World       string
	Scope       string
	Lanes       string
	AliasDigest string
}

// CursorGuardHex returns the 8-hex-char guard binding a cursor to its
// minting state. Fields are length-framed so concatenations cannot
// collide.
func CursorGuardHex(inputs CursorInputs) string {
	h := sha256.New()
	h.Write([]byte(ScorerVersion))
	for _, field := range []string{
		inputs.Query, inputs.World, inputs.Scope, inputs.Lanes, inputs.AliasDigest,
	} {
		fmt.Fprintf(h, "\x00%d\x00", len(field))
		h.Write([]byte(field))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:CursorGuardHexLen/2])
}

// CursorEncode mints the cursor for one offset.
func CursorEncode(offset uint64, inputs CursorInputs) string {
	return fmt.Sprintf("%s%d.%s", CursorPrefix, offset, CursorGuardHex(inputs))
}

// ErrCursorMalformed is a cursor that fails shape or guard validation.
var ErrCursorMalformed = errCursorMalformed{}

type errCursorMalformed struct{}

func (errCursorMalformed) Error() string { return "malformed cursor" }

// CursorDecode validates a cursor against the state that minted it and
// returns its offset.
func CursorDecode(cursor string, inputs CursorInputs) (uint64, error) {
	if !strings.HasPrefix(cursor, CursorPrefix) {
		return 0, ErrCursorMalformed
	}
	rest := cursor[len(CursorPrefix):]
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return 0, ErrCursorMalformed
	}
	offset, err := strconv.ParseUint(rest[:dot], 10, 64)
	if err != nil {
		return 0, ErrCursorMalformed
	}
	if rest[dot+1:] != CursorGuardHex(inputs) {
		return 0, ErrCursorMalformed
	}
	return offset, nil
}
