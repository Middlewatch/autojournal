// memory_search and memory_get orchestration.
//
// Search is not evidence opening: Search returns ranked, stable evidence
// references with bounded snippets; Get opens one reference with explicit
// line bounds and validates identity and revision against the file on
// disk. Failures are typed outcomes, never Go errors — recall degrading
// is a normal result the caller renders, not an exception.
//
// Discovery pipeline: query terms → additive alias expansion → additive
// singular folding of plural terms → vocabulary substring scan →
// postings fetch under world/scope/lane filters → per-line crediting
// against the source text (word-start boundary by default, so "hang"
// credits "hanging" but not "changed"; CreditSubstring restores infix
// recall on request) → the pure scorer in retrieval.go → span dedup,
// per-episode cap, floor, page.

package autojournal

import (
	"errors"
	"math/bits"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultLanes is the recall lane set when the caller does not restrict it.
var DefaultLanes = []Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy}

// DefaultResultsLimit is the page size when the caller does not set one, and
// matches the config file's max_results default so the two agree.
const DefaultResultsLimit = 10

// MinNeedleLen: needles shorter than this are excluded from the
// vocabulary scan when longer needles exist — a 2-byte needle ("pi")
// substring-matches a huge share of the vocabulary, floods
// MaxVocabMatches, and the scan's early break then silently drops
// discovery for the query's remaining terms. A query whose tokens are
// all short still scans with them, so curated short alias values ("q8")
// keep working on their own.
const MinNeedleLen = 3

// CreditMode is how a term is credited against a matched line's text.
//
// CreditSubstring: any occurrence counts, so "hang" credits
// lines containing "change". CreditWordStart: the occurrence must begin
// at a token boundary — "hang" credits "hanging" but not "change", and
// "config" still credits "configuration". CreditWholeWord: both edges
// must be token boundaries — "hang" credits only the exact word.
type CreditMode string

const (
	CreditSubstring CreditMode = "substring"
	CreditWordStart CreditMode = "word_start"
	CreditWholeWord CreditMode = "whole_word"
)

// Knobs are the scoring knobs, resolved from owner config by the caller.
type Knobs struct {
	ContextWindow   uint32
	RecencyBoost    float64
	MinScore        float64
	ConfidenceFloor float64
}

// DefaultKnobs are the frozen compatibility defaults.
var DefaultKnobs = Knobs{
	ContextWindow:   3,
	RecencyBoost:    1.0,
	MinScore:        0.0,
	ConfidenceFloor: 3.0,
}

// SearchRequest is one memory_search call.
type SearchRequest struct {
	Query string
	World string
	Scope *string
	Lanes []Lane // nil means DefaultLanes
	// Limit 0 resolves to DefaultResultsLimit; above MaxResultsLimit clamps
	// down. Note the config path rejects max_results: 0 as malformed, so the
	// same value is an error through one door and a default through this one.
	Limit uint32
	// Cursor pages a previous identical request; nil for the first page.
	Cursor     *string
	NowMs      uint64 // injectable clock (epoch ms) for deterministic recency
	Knobs      Knobs
	CreditMode CreditMode
}

// singularVariants returns additive singular candidates for one query
// term: "quotas"→"quota", "boxes"→"box"/"boxe", "policies"→"policy".
// Introduced in aj-scorer.v2: purely additive recall closing the word-form
// gap word-start crediting cannot (a plural query term never occurs inside
// its singular's text). Variants append to the term list like alias values
// (and since aj-scorer.v3 are reported as FoldedTerms); a variant that
// never credits merely contributes df 0.
func singularVariants(term string) []string {
	var out []string
	add := func(v string) {
		if len(v) > 2 && !IsStopWord(v) {
			out = append(out, v)
		}
	}
	switch {
	case strings.HasSuffix(term, "ies") && len(term) > 4:
		add(term[:len(term)-3] + "y")
	case strings.HasSuffix(term, "es") && len(term) > 4:
		add(term[:len(term)-1])
		add(term[:len(term)-2])
	case strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") && len(term) > 3:
		add(term[:len(term)-1])
	}
	return out
}

// Hit is one ranked evidence reference.
type Hit struct {
	EpisodeID string
	// Revision is the sha256:<hex> revision this evidence was ranked
	// against.
	Revision      string
	Path          string
	Scope         string
	Lane          Lane
	CapturePolicy string
	EventTimeMs   uint64
	// Line is the 1-based matched line in the source file.
	Line         uint32
	SnippetStart uint32
	SnippetEnd   uint32
	// Snippet is the bounded context, rendered from the same verified
	// content the crediting pass read — it always shows the revision
	// this hit was credited against, even if the file changes mid-call.
	Snippet      string
	MatchedTerms []string
	Score        float64
	Confidence   Confidence
}

// SearchOutput is the memory_search result. Fields keep their zero values
// where an outcome does not populate them.
type SearchOutput struct {
	Outcome    Outcome
	QueryTerms []string
	AliasTerms []string
	// FoldedTerms are the additive singular variants that joined the term
	// list (aj-scorer.v3): surfaced so a term the owner never typed is
	// never unexplained in the report.
	FoldedTerms []string
	Hits        []Hit
	// Total is the true post-dedup, post-floor result count (not the raw
	// match count).
	Total       uint64
	NextCursor  string // empty when the page is the last
	BestScore   float64
	AliasDigest string
	Freshness   IndexFreshness
	Indexed     uint64
	Source      uint64
	// EditedExcluded counts candidates dropped because their source file
	// no longer matches the indexed revision (edited or vanished since
	// indexing).
	EditedExcluded uint64
	Detail         string
}

// dbErrorName renders the `detail` failure vocabulary. These strings are an
// Interface-tier contract: an adapter distinguishes a busy index from a
// corrupt one by matching them, so they are not free to reword.
func dbErrorName(err error) string {
	switch {
	case errors.Is(err, ErrSQLiteBusy):
		return "Busy"
	case errors.Is(err, ErrSQLiteCorrupt):
		return "Corrupt"
	case errors.Is(err, ErrSQLiteReadOnly):
		return "ReadOnly"
	case errors.Is(err, ErrSQLiteCantOpen):
		return "CantOpen"
	case errors.Is(err, ErrSQLiteMisuse):
		return "Misuse"
	case errors.Is(err, ErrSQLiteNoMemory):
		return "OutOfMemory"
	default:
		return "SqliteError"
	}
}

// outcomeForError maps an inner failure to its typed outcome: a busy
// index is a timeout, everything else is unavailable.
func outcomeForError(err error) Outcome {
	if errors.Is(err, ErrSQLiteBusy) {
		return OutcomeTimeout
	}
	return OutcomeUnavailable
}

// Search runs one memory_search against the projection.
//
// A zero Knobs struct resolves to DefaultKnobs — Go has no field
// defaults, and silently scoring with a zero context window or recency
// boost would drift from the frozen behavior without any signal. Callers that
// want non-default scoring start from DefaultKnobs and change fields.
// A zero CreditMode resolves to CreditWordStart for the same reason.
func Search(root *os.Root, idx *Index, aliasMap *AliasMap, req SearchRequest) SearchOutput {
	if req.Knobs == (Knobs{}) {
		req.Knobs = DefaultKnobs
	}
	if req.CreditMode == "" {
		req.CreditMode = CreditWordStart
	}
	if req.Limit == 0 {
		req.Limit = DefaultResultsLimit
	}
	out := SearchOutput{
		Outcome:     OutcomeInternalError,
		AliasDigest: aliasMap.DigestHex(),
		Freshness:   IndexUnavailable,
	}
	if err := searchInner(root, idx, aliasMap, req, &out); err != nil {
		out.Outcome = outcomeForError(err)
		out.Detail = dbErrorName(err)
	}
	return out
}

func searchInner(root *os.Root, idx *Index, aliasMap *AliasMap, req SearchRequest, out *SearchOutput) error {
	// Index health first: an empty projection over a nonempty corpus is
	// index_stale, never no_match. The one signal serves search and status
	// alike: both derive freshness from (*Index).Freshness and nothing
	// else, so the two reporters cannot disagree about the same corpus.
	// The injectable clock also drives the memo's settled-corpus guard, so
	// a fixed-clock test exercises the same path a live query takes.
	nowMs := req.NowMs
	if nowMs == 0 {
		nowMs = uint64(time.Now().UnixMilli())
	}
	fresh, err := idx.Freshness(root, nowMs)
	if err != nil {
		return err
	}
	out.Indexed = fresh.Indexed
	out.Source = fresh.Source
	out.Freshness = fresh.Freshness

	// --- Terms and alias expansion ---
	base := ExtractTerms(req.Query)
	termsTruncated := base.Truncated
	out.QueryTerms = base.Items
	if len(base.Items) == 0 {
		out.Outcome = OutcomeNoMatch
		return nil
	}

	// Duplicate term weights are unconditional (aj-scorer.v3): the
	// query's own list keeps its repetitions, and alias values and folded
	// singular variants are appended to it — deduplicated against the terms
	// already present, never replacing the list. Whether a repeated query
	// word counts twice therefore no longer depends on whether an unrelated
	// thesaurus entry happens to fire. Folded variants are reported in
	// FoldedTerms so a term the owner never typed is never unexplained.
	finalTerms := append([]string(nil), base.Items...)
	have := newStringSet()
	for _, t := range base.Items {
		have.add(t)
	}
	for _, t := range base.Items {
		for _, v := range aliasMap.Get(t) {
			if have.has(v) {
				continue
			}
			have.add(v)
			out.AliasTerms = append(out.AliasTerms, v)
			finalTerms = append(finalTerms, v)
		}
	}
	for _, t := range base.Items {
		for _, v := range singularVariants(t) {
			if have.has(v) {
				continue
			}
			have.add(v)
			out.FoldedTerms = append(out.FoldedTerms, v)
			finalTerms = append(finalTerms, v)
		}
	}
	if len(finalTerms) > MaxQueryTerms {
		finalTerms = finalTerms[:MaxQueryTerms]
		termsTruncated = true
	}

	// --- Discovery: vocabulary substring scan ---
	// Needles are the index-token components of each term, so a phrase
	// value like "llama.cpp" discovers via "llama"/"cpp" and is credited
	// by full-substring match on the line text below.
	needles, shortNeedles := newStringSet(), newStringSet()
	for _, t := range finalTerms {
		for _, needle := range TokenizeLine(t) {
			if len(needle) >= MinNeedleLen {
				needles.add(needle)
			} else {
				shortNeedles.add(needle)
			}
		}
	}
	// Discovery policy: the fallback is per query, not per needle.
	// Any long needle makes the whole query trigram-eligible; only a
	// wholly-short query (every needle under MinNeedleLen, so no trigram
	// can witness any of them) takes the linear scan, and it takes it
	// whole — preserving curated short-alias reachability. Both paths
	// iterate the vocabulary in sorted term order, so the MaxVocabMatches
	// cap truncates the same stable prefix either way.
	var vocabMatches []string
	vocabTruncated := false
	if needleKeys := needles.items; len(needleKeys) > 0 {
		vocabMatches, vocabTruncated, err = idx.VocabCandidates(req.World, needleKeys)
		if err != nil {
			return err
		}
	} else {
		vocab, err := idx.VocabTerms(req.World)
		if err != nil {
			return err
		}
	scan:
		for _, token := range vocab {
			for _, needle := range shortNeedles.items {
				if strings.Contains(token, needle) {
					if len(vocabMatches) >= MaxVocabMatches {
						vocabTruncated = true
						break scan
					}
					vocabMatches = append(vocabMatches, token)
					continue scan
				}
			}
		}
	}

	// --- Candidate accumulation from postings ---
	type episodeAccum struct {
		meta          EpisodeInfo
		digestHex     string
		scope         string
		lane          Lane
		capturePolicy string
		bodyLine      uint32
		lines         []uint32
		unionMask     uint64
		// content is the verified bytes the crediting pass read; snippet
		// rendering reuses it, so each episode is read once per query and
		// a snippet always shows the revision that was credited. Held
		// only within this call — snippets copy out of it, and the accum
		// dies with the search.
		content string
	}
	lanes := req.Lanes
	if lanes == nil {
		lanes = DefaultLanes
	}
	episodeOrds := map[string]uint32{}
	var episodes []episodeAccum
	seenLines := map[uint64]struct{}{}

	// Postings coordinates lean, metadata for exactly the referenced
	// episodes, join in memory: the SQL-side join cost one B-tree probe
	// per posting row and dominated broad searches, and a whole-world
	// metadata load cost the corpus size on every query. Pairs outside
	// the world/scope/lane filter simply miss the metadata map. Ordinals
	// are assigned in PostingPairs order below, which does not change, so
	// deterministic tie-breaking is preserved.
	pairs, err := idx.PostingPairs(vocabMatches)
	if err != nil {
		return err
	}
	seenIDs := map[string]struct{}{}
	var referenced []string
	for _, pair := range pairs {
		if _, dup := seenIDs[pair.EpisodeID]; !dup {
			seenIDs[pair.EpisodeID] = struct{}{}
			referenced = append(referenced, pair.EpisodeID)
		}
	}
	eligible, err := idx.EpisodeMetadata(referenced, req.World, req.Scope, lanes)
	if err != nil {
		return err
	}
	metaByID := make(map[string]*PostingRow, len(eligible))
	for i := range eligible {
		metaByID[eligible[i].EpisodeID] = &eligible[i]
	}
	for _, pair := range pairs {
		row, eligibleEpisode := metaByID[pair.EpisodeID]
		if !eligibleEpisode {
			continue
		}
		ord, ok := episodeOrds[pair.EpisodeID]
		if !ok {
			ord = uint32(len(episodes))
			episodes = append(episodes, episodeAccum{
				meta: EpisodeInfo{
					EpisodeID:   row.EpisodeID,
					RelPath:     row.RelPath,
					EventTimeMs: row.EventTimeMs,
				},
				digestHex:     row.DigestHex,
				scope:         row.Scope,
				lane:          row.Lane,
				capturePolicy: row.CapturePolicy,
				bodyLine:      row.BodyLine,
			})
			episodeOrds[pair.EpisodeID] = ord
		}
		key := uint64(ord)<<32 | uint64(pair.LineNo)
		if _, dup := seenLines[key]; dup {
			continue
		}
		seenLines[key] = struct{}{}
		episodes[ord].lines = append(episodes[ord].lines, pair.LineNo)
	}

	if len(episodes) == 0 {
		out.Outcome = OutcomeNoMatch
		if out.Indexed == 0 && out.Source > 0 && out.Freshness == IndexStale {
			out.Outcome = OutcomeIndexStale
		}
		if vocabTruncated || termsTruncated {
			out.Detail = "discovery_truncated"
		}
		return nil
	}

	// --- Per-line crediting against source text ---
	var candidates []Candidate
	df := make([]uint64, len(finalTerms))

	for ord := range episodes {
		ep := &episodes[ord]
		sort.Slice(ep.lines, func(i, j int) bool { return ep.lines[i] < ep.lines[j] })
		content, err := readContained(root, ep.meta.RelPath)
		if err != nil {
			out.EditedExcluded++
			continue
		}
		// Evidence is verified against content, not against the recorded
		// digest line: a body edit that left the frontmatter untouched has
		// no reading that recomputes to the recorded digest and is excluded
		// here. A file that verifies against a different digest
		// than the projection holds is an absorbed edit awaiting sync —
		// excluded the same way, never served under a stale reference.
		verified, err := VerifyEpisode(content)
		if err != nil {
			out.EditedExcluded++
			continue
		}
		if verified.DigestHex != ep.digestHex {
			out.EditedExcluded++
			continue
		}
		ep.content = content

		want := 0
		for lineNo, line := range strings.Split(content, "\n") {
			if want >= len(ep.lines) {
				break
			}
			if uint32(lineNo+1) != ep.lines[want] {
				continue
			}
			want++
			var mask uint64
			for i, term := range finalTerms {
				if CreditLine(line, term, req.CreditMode) {
					mask |= 1 << uint(i)
				}
			}
			if mask == 0 {
				continue
			}
			candidates = append(candidates, Candidate{
				EpisodeOrd:  uint32(ord),
				LineNo:      uint32(lineNo + 1),
				MatchedMask: mask,
			})
			ep.unionMask |= mask
		}
		mask := ep.unionMask
		for mask != 0 {
			bit := trailingZeros(&mask)
			if bit < len(df) {
				df[bit]++
			}
		}
	}

	if len(candidates) == 0 {
		out.Outcome = OutcomeNoMatch
		if vocabTruncated || termsTruncated {
			out.Detail = "discovery_truncated"
		}
		return nil
	}

	// --- Score and rank ---
	var creditedEpisodes uint64
	episodeInfos := make([]EpisodeInfo, len(episodes))
	for i, ep := range episodes {
		if ep.unionMask != 0 {
			creditedEpisodes++
		}
		episodeInfos[i] = ep.meta
	}
	statsN, err := idx.StatsEpisodeCount(req.World)
	if err != nil {
		return err
	}
	// Floor N at the credited-episode count (and 1): stats can lag the
	// postings after partial damage, and df > N would flip an IDF weight
	// negative.
	n := max(max(statsN, creditedEpisodes), 1)
	idf := make([]float64, len(finalTerms))
	for i, d := range df {
		idf[i] = IDFWeight(n, d)
	}

	// The resolved clock, not req.NowMs: a caller using the zero value's
	// documented live-clock fallback must get live recency too, or every
	// event time reads as future and the recency nudge silently vanishes.
	ranked := Rank(candidates, episodeInfos, idf, RankParams{
		NowMs:         nowMs,
		RecencyBoost:  req.Knobs.RecencyBoost,
		MinScore:      req.Knobs.MinScore,
		ContextWindow: req.Knobs.ContextWindow,
		MaxPerEpisode: MaxPerEpisodeDefault,
	})
	out.Total = uint64(len(ranked.Order))
	if len(ranked.Order) > 0 {
		out.BestScore = ranked.Scores[ranked.Order[0]]
	}

	// --- Cursor and page ---
	scope := ""
	if req.Scope != nil {
		scope = *req.Scope
	}
	cursorInputs := CursorInputs{
		Query:       req.Query,
		World:       req.World,
		Scope:       scope,
		Lanes:       lanesTag(lanes),
		AliasDigest: out.AliasDigest,
	}
	var offset uint64
	if req.Cursor != nil {
		offset, err = CursorDecode(*req.Cursor, cursorInputs)
		if err != nil {
			out.Outcome = OutcomeMalformed
			out.Detail = "cursor does not match this query"
			return nil
		}
	}
	if len(ranked.Order) == 0 {
		out.Outcome = OutcomeNoMatch
		if vocabTruncated || termsTruncated {
			out.Detail = "discovery_truncated"
		}
		return nil
	}
	start := len(ranked.Order)
	if offset < uint64(start) {
		start = int(offset)
	}
	limit := max(min(req.Limit, MaxResultsLimit), 1)
	end := min(start+int(limit), len(ranked.Order))
	if end < len(ranked.Order) {
		out.NextCursor = CursorEncode(uint64(end), cursorInputs)
	}

	// --- Render page hits with bounded snippets ---
	hits := make([]Hit, end-start)
	for hitI, candIdx := range ranked.Order[start:end] {
		cand := candidates[candIdx]
		ep := episodes[cand.EpisodeOrd]

		var matched []string
		matchedPositions := 0
		mask := cand.MatchedMask
		for mask != 0 {
			bit := trailingZeros(&mask)
			if bit >= len(finalTerms) {
				continue
			}
			matchedPositions++
			term := finalTerms[bit]
			dup := false
			for _, m := range matched {
				if m == term {
					dup = true
					break
				}
			}
			if !dup {
				matched = append(matched, term)
			}
		}
		coverage := float64(matchedPositions) / float64(len(finalTerms))

		snippet := renderSnippet(ep.content, snippetSpec{
			line:          cand.LineNo,
			bodyLine:      ep.bodyLine,
			contextWindow: req.Knobs.ContextWindow,
		})
		hits[hitI] = Hit{
			EpisodeID:     ep.meta.EpisodeID,
			Revision:      DigestPrefix + ep.digestHex,
			Path:          ep.meta.RelPath,
			Scope:         ep.scope,
			Lane:          ep.lane,
			CapturePolicy: ep.capturePolicy,
			EventTimeMs:   ep.meta.EventTimeMs,
			Line:          cand.LineNo,
			SnippetStart:  snippet.start,
			SnippetEnd:    snippet.end,
			Snippet:       snippet.text,
			MatchedTerms:  matched,
			Score:         ranked.Scores[candIdx],
			Confidence: ConfidenceWithCoverage(ranked.Scores[candIdx],
				coverage, req.Knobs.ConfidenceFloor),
		}
	}
	out.Hits = hits
	out.Outcome = OutcomeMatch
	if vocabTruncated || termsTruncated {
		out.Detail = "discovery_truncated"
	}
	return nil
}

// stringSet is an insertion-ordered set; final term order feeds mask bit
// positions, so it must be deterministic.
type stringSet struct {
	items []string
	have  map[string]struct{}
}

func newStringSet() *stringSet {
	return &stringSet{have: map[string]struct{}{}}
}

func (s *stringSet) has(v string) bool {
	_, ok := s.have[v]
	return ok
}

func (s *stringSet) add(v string) {
	if s.has(v) {
		return
	}
	s.have[v] = struct{}{}
	s.items = append(s.items, v)
}

// trailingZeros clears and returns the lowest set bit's index.
func trailingZeros(mask *uint64) int {
	bit := bits.TrailingZeros64(*mask)
	*mask &= *mask - 1
	return bit
}

// CreditLine reports whether term occurs in line under mode's boundary
// rule (case-insensitive). Boundaries use the index-token alphabet, so a
// phrase term ("out of memory", "llama.cpp") is checked at the edges of
// the whole occurrence and its interior punctuation needs no special
// handling.
func CreditLine(line, term string, mode CreditMode) bool {
	for from := 0; from+len(term) <= len(line); {
		pos := indexIgnoreCase(line, term, from)
		if pos < 0 {
			return false
		}
		from = pos + 1
		if mode == CreditSubstring {
			return true
		}
		if pos > 0 && IsIndexTokenByte(line[pos-1]) {
			continue
		}
		if mode == CreditWordStart {
			return true
		}
		end := pos + len(term)
		if end >= len(line) || !IsIndexTokenByte(line[end]) {
			return true
		}
	}
	return false
}

// indexIgnoreCase finds the first case-insensitive ASCII occurrence of
// needle in s at or after from; -1 when absent.
func indexIgnoreCase(s, needle string, from int) int {
	if len(needle) == 0 {
		return from
	}
	for i := from; i+len(needle) <= len(s); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if lowerByte(s[i+j]) != lowerByte(needle[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func lanesTag(lanes []Lane) string {
	var b strings.Builder
	for i, lane := range lanes {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(string(lane))
	}
	return b.String()
}

type snippet struct {
	text       string
	start, end uint32
}

type snippetSpec struct {
	line, bodyLine uint32
	contextWindow  uint32
}

// readContained is ReadContained behind a package seam, so the
// one-read-per-episode contract stays countable in tests.
var readContained = ReadContained

// renderSnippet renders ±context_window lines from the content the
// crediting pass already read and verified — one read per episode per
// query, and a snippet always shows the revision that was credited.
// Rendering runs after Rank and feeds no scoring input, so it cannot
// move ranking. Lines are clamped to the body, each capped at a
// codepoint boundary, the whole snippet capped at MaxSnippetBytes.
func renderSnippet(content string, spec snippetSpec) snippet {
	empty := snippet{text: "", start: spec.line, end: spec.line}

	first := max(spec.bodyLine, satSub(spec.line, spec.contextWindow))
	last := spec.line + spec.contextWindow

	var text strings.Builder
	var start, end uint32
	any := false
	for lineNo, line := range strings.Split(content, "\n") {
		no := uint32(lineNo + 1)
		if no < first {
			continue
		}
		if no > last {
			break
		}
		capped := capAtCodepoint(line, MaxSnippetLineBytes)
		if text.Len()+len(capped)+1 > MaxSnippetBytes {
			if no <= spec.line {
				// Never render a snippet that omits the matched line.
				text.Reset()
				start = 0
				any = false
			} else {
				break
			}
		}
		if start == 0 {
			start = no
		}
		// Join on a flag, not buffer length: empty lines are real lines
		// and must keep the snippet's line numbering aligned.
		if any {
			text.WriteByte('\n')
		}
		any = true
		text.WriteString(capped)
		end = no
	}
	if start == 0 {
		return empty
	}
	return snippet{text: text.String(), start: start, end: end}
}

func satSub(v, sub uint32) uint32 {
	if sub > v {
		return 0
	}
	return v - sub
}

// capAtCodepoint is a byte cap that never splits a UTF-8 sequence.
func capAtCodepoint(line string, max int) string {
	if len(line) <= max {
		return line
	}
	cut := max
	for cut > 0 && (line[cut]&0b1100_0000) == 0b1000_0000 {
		cut--
	}
	return line[:cut]
}

// --- memory_get ---

// GetRequest is one memory_get call.
type GetRequest struct {
	EpisodeID string
	// Revision is accepted with or without the sha256: prefix.
	Revision string
	// PathHint is the optional path from a search hit; the index is
	// consulted when absent or wrong (moves preserve identity after sync).
	PathHint      *string
	ExpectedWorld *string
	ExpectedScope *string
	// LineStart 0 means "start of body".
	LineStart uint32
	// LineEnd 0 means "LineStart + max span".
	LineEnd uint32
}

// GetOutput is the memory_get result.
type GetOutput struct {
	Outcome   Outcome
	EpisodeID string
	// Revision is the current revision on disk (sha256:<hex>), which on
	// stale_revision is the replacement reference.
	Revision string
	Path     string
	World    string
	Scope    string
	Lane     Lane
	// LaneSet/PathSet/etc. report presence: outcomes that never resolve
	// an episode leave the zero values, and the CLI renders nulls.
	Resolved      bool
	CapturePolicy string
	LineStart     uint32
	LineEnd       uint32
	Content       string
	// Trust: recalled text is untrusted evidence, never instructions.
	Trust  string
	Detail string
}

// Get opens one bounded evidence span with identity and revision checks.
func Get(root *os.Root, idx *Index, req GetRequest) GetOutput {
	out := GetOutput{
		Outcome:   OutcomeInternalError,
		EpisodeID: req.EpisodeID,
		Trust:     "untrusted_evidence",
	}
	if err := getInner(root, idx, req, &out); err != nil {
		out.Outcome = outcomeForError(err)
		out.Detail = dbErrorName(err)
	}
	return out
}

func getInner(root *os.Root, idx *Index, req GetRequest, out *GetOutput) error {
	if !validEpisodeID(req.EpisodeID) {
		out.Outcome = OutcomeMalformed
		out.Detail = "episode_id must be aj1-<32 hex>"
		return nil
	}
	requestedHex := strings.TrimPrefix(req.Revision, DigestPrefix)
	if len(requestedHex) != DigestHexLen {
		out.Outcome = OutcomeMalformed
		out.Detail = "revision must be sha256:<64 hex>"
		return nil
	}
	if req.LineEnd != 0 && req.LineStart != 0 && req.LineEnd < req.LineStart {
		out.Outcome = OutcomeMalformed
		out.Detail = "line_end precedes line_start"
		return nil
	}

	// Resolve: path hint first, then the index; a hint that no longer
	// resolves falls through to the index because moves preserve identity.
	var content string
	found := false
	usedPath := ""
	if req.PathHint != nil {
		if c, err := ReadContained(root, *req.PathHint); err == nil {
			content = c
			found = true
			usedPath = *req.PathHint
		}
	}
	if !found {
		row, err := idx.LookupEpisode(req.EpisodeID)
		if err != nil {
			return err
		}
		if row != nil {
			if c, err := ReadContained(root, row.RelPath); err == nil {
				content = c
				found = true
				usedPath = row.RelPath
			}
		}
	}
	if !found {
		out.Outcome = OutcomeGone
		out.Detail = "no source file for this episode (index may be stale; try sync)"
		return nil
	}

	ep := ParseEpisode(content)
	if ep == nil {
		out.Outcome = OutcomeGone
		out.Detail = "source file is no longer a parseable episode"
		return nil
	}
	if ep.EpisodeID != req.EpisodeID {
		out.Outcome = OutcomeGone
		out.Detail = "file at the resolved path carries another episode identity"
		return nil
	}
	if req.ExpectedWorld != nil && ep.World != *req.ExpectedWorld {
		out.Outcome = OutcomeGone
		out.Detail = "episode is outside the active world"
		return nil
	}
	if req.ExpectedScope != nil && ep.Scope != *req.ExpectedScope {
		out.Outcome = OutcomeGone
		out.Detail = "episode is outside the active scope"
		return nil
	}
	out.Resolved = true
	out.Path = usedPath
	out.World = ep.World
	out.Scope = ep.Scope
	out.Lane = ep.Lane
	out.CapturePolicy = ep.CapturePolicy

	// Two distinct edit states report differently. A file that does
	// not verify at all — the digest-stale state — has no honest current
	// revision to offer until the owner reseals it, so Revision stays empty.
	// A file that verifies against a different digest than requested is an
	// absorbed edit, and the current verified revision is the replacement
	// reference, exactly as before.
	verified, verifyErr := VerifyEpisode(content)
	if verifyErr != nil {
		out.Outcome = OutcomeStaleRevision
		out.Detail = "episode was edited after capture; run reseal to re-attest it"
		return nil
	}
	out.Revision = DigestPrefix + verified.DigestHex

	if verified.DigestHex != requestedHex {
		// Edited evidence is never silently served as the old revision.
		out.Outcome = OutcomeStaleRevision
		out.Detail = "episode was edited; re-search or request the current revision"
		return nil
	}

	// Bounded body span.
	start := ep.BodyLine
	if req.LineStart != 0 && req.LineStart > start {
		start = req.LineStart
	}
	requestedEnd := uint64(start) + uint64(MaxGetLines) - 1
	if req.LineEnd != 0 && uint64(req.LineEnd) < requestedEnd {
		requestedEnd = uint64(req.LineEnd)
	}

	var text strings.Builder
	var servedStart, servedEnd uint32
	any := false
	for lineNo, line := range strings.Split(content, "\n") {
		no := uint64(lineNo + 1)
		if no < uint64(start) {
			continue
		}
		if no > requestedEnd {
			break
		}
		if text.Len()+len(line)+1 > MaxGetBytes {
			break
		}
		if servedStart == 0 {
			servedStart = uint32(no)
		}
		if any {
			text.WriteByte('\n')
		}
		any = true
		text.WriteString(line)
		servedEnd = uint32(no)
	}
	out.LineStart = servedStart
	out.LineEnd = servedEnd
	out.Content = text.String()
	out.Outcome = OutcomeMatch
	return nil
}

func validEpisodeID(id string) bool {
	if len(id) != EpisodeIDLen || !strings.HasPrefix(id, IDPrefix) {
		return false
	}
	for i := len(IDPrefix); i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
