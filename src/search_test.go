// End-to-end retrieval tests: corpus on disk, SQLite projection, search
// orchestration, evidence opening. Frozen clock throughout.

package autojournal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const searchTestNowMs uint64 = 1785240000000

type searchFixture struct {
	rootPath  string
	root      *os.Root
	idx       *Index
	published [4]*Published
	emptyMap  *AliasMap
}

func (fx *searchFixture) request(query string) SearchRequest {
	return SearchRequest{Query: query, World: "testworld", NowMs: searchTestNowMs}
}

// setupSearchCorpus publishes four episodes: two conversation, one
// delegated, one evaluation, with distinct multi-line bodies so ranking,
// context windows, and lane filters are all observable.
func setupSearchCorpus(t *testing.T) *searchFixture {
	t.Helper()
	rootPath, root := testCorpus(t)
	base := mustValidate(t, testPayloadJSON)

	p1 := base
	p1.TurnID = "turn-2001"
	p1.UserContent = "context before\nthe quokka enclosure needed reindexing today\ncontext after"
	p1.AssistantResult = "Noted the fwupd firmware refresh too."
	a := mustPublish(t, root, p1)

	p2 := base
	p2.TurnID = "turn-2002"
	p2.UserContent = "a quokka appeared in the pi-web-access logs"
	p2.AssistantResult = "Filed it."
	b := mustPublish(t, root, p2)

	p3 := base
	p3.TurnID = "turn-2003"
	p3.Lane = LaneDelegatedWork
	p3.UserContent = "delegated wombat census"
	p3.AssistantResult = "Census complete."
	c := mustPublish(t, root, p3)

	p4 := base
	p4.TurnID = "turn-2004"
	p4.Lane = LaneEvaluation
	p4.UserContent = "sealed evaluation phrase quokka"
	p4.AssistantResult = "Sealed."
	d := mustPublish(t, root, p4)

	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatalf("SyncFromCorpus: %v", err)
	}

	return &searchFixture{
		rootPath:  rootPath,
		root:      root,
		idx:       idx,
		published: [4]*Published{a, b, c, d},
		emptyMap:  LoadAliasMapFromBytes([]byte("{}")),
	}
}

func TestSearchRanksMultiTermHitsFirstWithProvenanceAndSnippets(t *testing.T) {
	fx := setupSearchCorpus(t)

	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka enclosure"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %q (%s)", out.Outcome, out.Detail)
	}
	if out.Total != 2 || len(out.Hits) != 2 {
		t.Fatalf("total = %d, hits = %d", out.Total, len(out.Hits))
	}

	top := out.Hits[0]
	if top.EpisodeID != fx.published[0].EpisodeID {
		t.Errorf("top episode = %q, want %q", top.EpisodeID, fx.published[0].EpisodeID)
	}
	if !strings.HasPrefix(top.Revision, "sha256:") {
		t.Errorf("revision = %q", top.Revision)
	}
	if !strings.HasPrefix(top.Path, "worlds/testworld/") {
		t.Errorf("path = %q", top.Path)
	}
	if top.Lane != LaneConversation {
		t.Errorf("lane = %q", top.Lane)
	}
	if top.Scope != "workspace:demo" {
		t.Errorf("scope = %q", top.Scope)
	}
	if top.Score <= out.Hits[1].Score {
		t.Errorf("top score %v not above second %v", top.Score, out.Hits[1].Score)
	}
	if len(top.MatchedTerms) != 2 {
		t.Errorf("matched terms = %v", top.MatchedTerms)
	}
	// The snippet carries the matched line plus context, never frontmatter.
	if !strings.Contains(top.Snippet, "quokka enclosure") {
		t.Errorf("snippet missing matched line: %q", top.Snippet)
	}
	if !strings.Contains(top.Snippet, "context before") {
		t.Errorf("snippet missing context: %q", top.Snippet)
	}
	if strings.Contains(top.Snippet, "payload_digest") {
		t.Errorf("snippet leaks frontmatter: %q", top.Snippet)
	}
	if top.SnippetStart < 19 { // body starts after frontmatter
		t.Errorf("snippet start = %d", top.SnippetStart)
	}
	if top.Line < top.SnippetStart || top.Line > top.SnippetEnd {
		t.Errorf("line %d outside snippet [%d,%d]", top.Line, top.SnippetStart, top.SnippetEnd)
	}
	// Line numbering stays aligned through blank lines: walking the snippet
	// to the hit's line index lands on the matched text.
	{
		lineNo := top.SnippetStart
		matchLine := ""
		found := false
		for _, line := range strings.Split(top.Snippet, "\n") {
			if lineNo == top.Line {
				matchLine = line
				found = true
				break
			}
			lineNo++
		}
		if !found {
			t.Fatalf("hit line %d not reachable in snippet starting at %d", top.Line, top.SnippetStart)
		}
		if !strings.Contains(matchLine, "quokka enclosure") {
			t.Errorf("snippet line %d = %q", top.Line, matchLine)
		}
	}
	if out.Freshness != IndexFresh {
		t.Errorf("freshness = %q", out.Freshness)
	}

	// Determinism: an identical call returns identical results.
	again := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka enclosure"))
	if again.Hits[0].EpisodeID != top.EpisodeID {
		t.Errorf("rerun episode = %q", again.Hits[0].EpisodeID)
	}
	if again.Hits[0].Line != top.Line {
		t.Errorf("rerun line = %d", again.Hits[0].Line)
	}
	if again.Hits[0].Score != top.Score {
		t.Errorf("rerun score = %v, want %v", again.Hits[0].Score, top.Score)
	}
}

func TestInfixRecallIsSubstringModeOnlyWordStartDefaultDropsIt(t *testing.T) {
	fx := setupSearchCorpus(t)

	// "index" appears only inside "reindexing". v1-parity substring
	// crediting still surfaces it.
	parityReq := fx.request("index")
	parityReq.CreditMode = CreditSubstring
	parity := Search(fx.root, fx.idx, fx.emptyMap, parityReq)
	if parity.Outcome != OutcomeMatch {
		t.Fatalf("parity outcome = %q", parity.Outcome)
	}
	if parity.Hits[0].EpisodeID != fx.published[0].EpisodeID {
		t.Errorf("parity episode = %q", parity.Hits[0].EpisodeID)
	}
	if parity.Hits[0].MatchedTerms[0] != "index" {
		t.Errorf("parity matched = %v", parity.Hits[0].MatchedTerms)
	}

	// The word_start default refuses the infix credit ("index" mid-word),
	// trading it away to stop "hang"-in-"changed" false credits; curated
	// aliases ("index" -> "reindex") recover wanted infix families.
	strict := Search(fx.root, fx.idx, fx.emptyMap, fx.request("index"))
	if strict.Outcome != OutcomeNoMatch {
		t.Errorf("strict outcome = %q", strict.Outcome)
	}
}

func TestEvaluationLaneInvisibleUntilExplicitlyRequested(t *testing.T) {
	fx := setupSearchCorpus(t)

	hidden := Search(fx.root, fx.idx, fx.emptyMap, fx.request("sealed"))
	if hidden.Outcome != OutcomeNoMatch {
		t.Fatalf("hidden outcome = %q", hidden.Outcome)
	}

	req := fx.request("sealed")
	req.Lanes = []Lane{LaneEvaluation}
	shown := Search(fx.root, fx.idx, fx.emptyMap, req)
	if shown.Outcome != OutcomeMatch {
		t.Fatalf("shown outcome = %q", shown.Outcome)
	}
	if shown.Hits[0].Lane != LaneEvaluation {
		t.Errorf("lane = %q", shown.Hits[0].Lane)
	}
}

func TestAliasesRescueVocabularyMismatchQueriesIncludingPhraseValues(t *testing.T) {
	fx := setupSearchCorpus(t)

	// Without the alias the casual word misses.
	miss := Search(fx.root, fx.idx, fx.emptyMap, fx.request("portal"))
	if miss.Outcome != OutcomeNoMatch {
		t.Fatalf("miss outcome = %q", miss.Outcome)
	}

	m := LoadAliasMapFromBytes([]byte(`{"portal": ["pi-web-access"], "refresh": ["fwupd"]}`))
	hit := Search(fx.root, fx.idx, m, fx.request("portal"))
	if hit.Outcome != OutcomeMatch {
		t.Fatalf("hit outcome = %q (%s)", hit.Outcome, hit.Detail)
	}
	if hit.Hits[0].EpisodeID != fx.published[1].EpisodeID {
		t.Errorf("episode = %q, want %q", hit.Hits[0].EpisodeID, fx.published[1].EpisodeID)
	}
	if len(hit.AliasTerms) != 1 {
		t.Errorf("alias terms = %v", hit.AliasTerms)
	}
	if hit.Hits[0].MatchedTerms[0] != "pi-web-access" {
		t.Errorf("matched = %v", hit.Hits[0].MatchedTerms)
	}
	// Alias identity differs from the empty map and stamps the output.
	if hit.AliasDigest == miss.AliasDigest {
		t.Errorf("alias digest %q did not change with the map", hit.AliasDigest)
	}
}

func TestNoiseQueriesProduceTypedNoMatchNotWeakResults(t *testing.T) {
	fx := setupSearchCorpus(t)

	noise := Search(fx.root, fx.idx, fx.emptyMap, fx.request("xyzzyplugh frobnicate"))
	if noise.Outcome != OutcomeNoMatch {
		t.Errorf("noise outcome = %q", noise.Outcome)
	}
	if noise.Total != 0 {
		t.Errorf("noise total = %d", noise.Total)
	}

	stops := Search(fx.root, fx.idx, fx.emptyMap, fx.request("the of and it"))
	if stops.Outcome != OutcomeNoMatch {
		t.Errorf("stops outcome = %q", stops.Outcome)
	}
}

func TestPaginationPagesDeterministicallyWithGuardedCursor(t *testing.T) {
	fx := setupSearchCorpus(t)

	req := fx.request("quokka")
	req.Limit = 1
	page1 := Search(fx.root, fx.idx, fx.emptyMap, req)
	if page1.Total != 2 {
		t.Fatalf("total = %d", page1.Total)
	}
	if len(page1.Hits) != 1 {
		t.Fatalf("page1 hits = %d", len(page1.Hits))
	}
	cursor := page1.NextCursor
	if cursor == "" {
		t.Fatal("page1 has no next cursor")
	}

	req2 := req
	req2.Cursor = &cursor
	page2 := Search(fx.root, fx.idx, fx.emptyMap, req2)
	if len(page2.Hits) != 1 {
		t.Fatalf("page2 hits = %d", len(page2.Hits))
	}
	if page2.NextCursor != "" {
		t.Errorf("page2 cursor = %q, want none", page2.NextCursor)
	}
	if page1.Hits[0].EpisodeID == page2.Hits[0].EpisodeID {
		t.Errorf("pages repeat episode %q", page1.Hits[0].EpisodeID)
	}

	// A cursor replayed against a different query is malformed.
	req3 := fx.request("enclosure")
	req3.Cursor = &cursor
	replay := Search(fx.root, fx.idx, fx.emptyMap, req3)
	if replay.Outcome != OutcomeMalformed {
		t.Errorf("replay outcome = %q", replay.Outcome)
	}
}

func TestEmptyProjectionOverNonemptyCorpusIsIndexStaleNotNoMatch(t *testing.T) {
	fx := setupSearchCorpus(t)

	freshIdx := openMemoryIndex(t)
	out := Search(fx.root, freshIdx, fx.emptyMap, fx.request("quokka"))
	if out.Outcome != OutcomeIndexStale {
		t.Errorf("outcome = %q", out.Outcome)
	}
	if out.Freshness != IndexStale {
		t.Errorf("freshness = %q", out.Freshness)
	}
}

func TestEpisodeEditedAfterIndexingIsExcludedFromEvidence(t *testing.T) {
	fx := setupSearchCorpus(t)

	// Replace episode 2's file with a re-rendered revision (new body, new
	// digest) without re-syncing: the projection still holds the old
	// revision, so the candidate must be dropped, not served.
	p2 := mustValidate(t, testPayloadJSON)
	p2.TurnID = "turn-2002"
	p2.UserContent = "the logs were rotated away"
	p2.AssistantResult = "Filed it."
	id := EpisodeID(&p2)
	digest := PayloadDigestHex(&p2)
	content := Render(RenderInput{
		Payload:       &p2,
		EpisodeID:     id,
		DigestHex:     digest,
		CaptureTimeMs: searchTestNowMs,
	})
	writeCorpusFile(t, fx.rootPath, fx.published[1].RelPath, string(content))

	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %q", out.Outcome)
	}
	if out.Total != 1 {
		t.Errorf("total = %d", out.Total)
	}
	if out.EditedExcluded != 1 {
		t.Errorf("edited_excluded = %d", out.EditedExcluded)
	}
	if out.Hits[0].EpisodeID != fx.published[0].EpisodeID {
		t.Errorf("episode = %q", out.Hits[0].EpisodeID)
	}
}

func TestMemoryGetOpensExactBoundedEvidenceAndTracksRevisions(t *testing.T) {
	fx := setupSearchCorpus(t)

	found := Search(fx.root, fx.idx, fx.emptyMap, fx.request("wombat"))
	if found.Outcome != OutcomeMatch {
		t.Fatalf("search outcome = %q", found.Outcome)
	}
	hit := found.Hits[0]

	// Happy path: hint + matching revision.
	got := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
	})
	if got.Outcome != OutcomeMatch {
		t.Fatalf("get outcome = %q (%s)", got.Outcome, got.Detail)
	}
	if !strings.Contains(got.Content, "delegated wombat census") {
		t.Errorf("content missing body: %q", got.Content)
	}
	if strings.Contains(got.Content, "payload_digest") {
		t.Errorf("content leaks frontmatter: %q", got.Content)
	}
	if got.LineStart < 19 {
		t.Errorf("line_start = %d", got.LineStart)
	}
	if got.Trust != "untrusted_evidence" {
		t.Errorf("trust = %q", got.Trust)
	}
	if !got.Resolved || got.Lane != LaneDelegatedWork {
		t.Errorf("lane = %q (resolved %v)", got.Lane, got.Resolved)
	}

	// Without the hint, the index resolves the path.
	noHint := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
	})
	if noHint.Outcome != OutcomeMatch {
		t.Errorf("no-hint outcome = %q", noHint.Outcome)
	}

	// A revision that no longer matches the file is stale, with the
	// current revision reported for re-request.
	stale := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  "sha256:" + strings.Repeat("ab", 32),
		PathHint:  &hit.Path,
	})
	if stale.Outcome != OutcomeStaleRevision {
		t.Errorf("stale outcome = %q", stale.Outcome)
	}
	if stale.Revision != hit.Revision {
		t.Errorf("stale revision = %q, want %q", stale.Revision, hit.Revision)
	}

	// Malformed references are typed, not crashes.
	badID := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: "not-an-id",
		Revision:  hit.Revision,
	})
	if badID.Outcome != OutcomeMalformed {
		t.Errorf("bad id outcome = %q", badID.Outcome)
	}
	badRev := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  "sha256:short",
	})
	if badRev.Outcome != OutcomeMalformed {
		t.Errorf("bad revision outcome = %q", badRev.Outcome)
	}

	// Deleted evidence is gone.
	if err := os.Remove(filepath.Join(fx.rootPath, filepath.FromSlash(hit.Path))); err != nil {
		t.Fatal(err)
	}
	gone := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
	})
	if gone.Outcome != OutcomeGone {
		t.Errorf("gone outcome = %q", gone.Outcome)
	}

	// Escaping path hints are rejected by containment, not resolved.
	if ContainedPath("../outside.md") {
		t.Error("ContainedPath accepted ../outside.md")
	}
	if ContainedPath("/etc/passwd") {
		t.Error("ContainedPath accepted /etc/passwd")
	}
	if !ContainedPath("worlds/w/2026/07/28/aj1-x.md") {
		t.Error("ContainedPath rejected a valid journal path")
	}
}

func TestLineBoundsClampToBodyAndHonorExplicitSpans(t *testing.T) {
	fx := setupSearchCorpus(t)

	found := Search(fx.root, fx.idx, fx.emptyMap, fx.request("enclosure"))
	if found.Outcome != OutcomeMatch {
		t.Fatalf("search outcome = %q", found.Outcome)
	}
	hit := found.Hits[0]

	// Requesting frontmatter lines clamps to the body start.
	clamped := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
		LineStart: 1,
		LineEnd:   100,
	})
	if clamped.Outcome != OutcomeMatch {
		t.Fatalf("clamped outcome = %q", clamped.Outcome)
	}
	if clamped.LineStart < 19 {
		t.Errorf("clamped line_start = %d", clamped.LineStart)
	}

	// A single-line span serves exactly that line.
	single := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
		LineStart: hit.Line,
		LineEnd:   hit.Line,
	})
	if single.LineStart != hit.Line || single.LineEnd != hit.Line {
		t.Errorf("span = [%d,%d], want [%d,%d]", single.LineStart, single.LineEnd, hit.Line, hit.Line)
	}
	if !strings.Contains(single.Content, "quokka enclosure") {
		t.Errorf("single-line content = %q", single.Content)
	}
	if strings.ContainsRune(single.Content, '\n') {
		t.Errorf("single-line content spans lines: %q", single.Content)
	}
}

func TestCreditLineBoundaryRulesPerMode(t *testing.T) {
	cases := []struct {
		line, term string
		mode       CreditMode
		want       bool
	}{
		// The motivating false positive: "hang" inside "changed".
		{"we changed the config", "hang", CreditSubstring, true},
		{"we changed the config", "hang", CreditWordStart, false},
		{"we changed the config", "hang", CreditWholeWord, false},

		// Prefix inflections survive word_start but not whole_word.
		{"the server was hanging", "hang", CreditWordStart, true},
		{"the server was hanging", "hang", CreditWholeWord, false},
		{"config", "config", CreditWholeWord, true},
		{"configuration drift", "config", CreditWordStart, true},
		{"configuration drift", "config", CreditWholeWord, false},

		// Infix recall exists only under substring parity.
		{"reindexing finished", "index", CreditSubstring, true},
		{"reindexing finished", "index", CreditWordStart, false},

		// Case-insensitive in every mode.
		{"Hang detected", "hang", CreditWholeWord, true},

		// A later occurrence can credit after an earlier bounded one fails.
		{"changed, then a hang", "hang", CreditWholeWord, true},

		// Phrase terms: boundaries apply at the edges of the whole
		// occurrence; interior punctuation and spaces need no special
		// handling.
		{"ran out of memory today", "out of memory", CreditWholeWord, true},
		{"timeout of memory", "out of memory", CreditWordStart, false},
		{"built llama.cpp again", "llama.cpp", CreditWholeWord, true},

		// Term at line edges has an implicit boundary.
		{"hang", "hang", CreditWholeWord, true},
		{"hangs", "hang", CreditWholeWord, false},
		{"hangs", "hang", CreditWordStart, true},

		// Underscore is a token byte: "foo_hang" does not word_start-credit
		// "hang".
		{"foo_hang", "hang", CreditWordStart, false},
		{"hang_foo", "hang", CreditWholeWord, false},
		{"hang_foo", "hang", CreditWordStart, true},
	}
	for _, tc := range cases {
		if got := CreditLine(tc.line, tc.term, tc.mode); got != tc.want {
			t.Errorf("CreditLine(%q, %q, %s) = %v, want %v", tc.line, tc.term, tc.mode, got, tc.want)
		}
	}
}

func TestPluralQueryFoldsToSingularAdditively(t *testing.T) {
	fx := setupSearchCorpus(t)
	// The corpus says "quokka"; a plural query still finds it because
	// aj-scorer.v2 adds the singular variant to the term union.
	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokkas"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("plural query outcome = %s, want match", out.Outcome)
	}
	found := false
	for _, hit := range out.Hits {
		for _, term := range hit.MatchedTerms {
			if term == "quokka" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no hit credited the folded singular; hits = %+v", out.Hits)
	}
}

func TestPerEpisodePageCapLimitsOneEpisodesRegions(t *testing.T) {
	rootPath, root := testCorpus(t)
	_ = rootPath
	base := mustValidate(t, testPayloadJSON)
	// One episode with many well-separated matching regions, one episode
	// with a single match: the long episode must not crowd the page past
	// MaxPerEpisodeDefault regions.
	long := base
	long.TurnID = "turn-3001"
	long.UserContent = strings.Repeat("zorilla sighting\nfiller one\nfiller two\nfiller three\nfiller four\nfiller five\nfiller six\n", 6)
	mustPublish(t, root, long)
	short := base
	short.TurnID = "turn-3002"
	short.UserContent = "a single zorilla note"
	shortPub := mustPublish(t, root, short)

	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatalf("SyncFromCorpus: %v", err)
	}
	out := Search(root, idx, LoadAliasMapFromBytes([]byte("{}")),
		SearchRequest{Query: "zorilla", World: "testworld", NowMs: searchTestNowMs})
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %s", out.Outcome)
	}
	perEpisode := map[string]int{}
	for _, hit := range out.Hits {
		perEpisode[hit.EpisodeID]++
	}
	for id, n := range perEpisode {
		if n > MaxPerEpisodeDefault {
			t.Errorf("episode %s holds %d page regions, cap is %d", id, n, MaxPerEpisodeDefault)
		}
	}
	if perEpisode[shortPub.EpisodeID] != 1 {
		t.Errorf("single-match episode missing from page: %+v", perEpisode)
	}
}

func TestConfidenceDiscountsPartialCoverage(t *testing.T) {
	// Full coverage keeps the plain banding; half coverage of the same
	// score must never band higher and drops out of "high" near the
	// 2*floor boundary.
	floor := 3.0
	if got := ConfidenceWithCoverage(6.0, 1.0, floor); got != ConfidenceHigh {
		t.Errorf("full coverage at 2*floor = %s, want high", got)
	}
	if got := ConfidenceWithCoverage(6.0, 0.5, floor); got == ConfidenceHigh {
		t.Errorf("half coverage at 2*floor still bands high")
	}
	if got := ConfidenceWithCoverage(6.0, 0.0, floor); got != ConfidenceLow {
		t.Errorf("zero coverage = %s, want low", got)
	}
}

// TestSearchExcludesDigestStaleEpisode: an episode whose body was edited
// with its payload_digest line left untouched is absent from results and
// counted in edited_excluded.
func TestSearchExcludesDigestStaleEpisode(t *testing.T) {
	fx := setupSearchCorpus(t)

	// Edit one body byte of the first published episode on disk, leaving
	// its frontmatter (and so the recorded digest line) untouched.
	full := filepath.Join(fx.rootPath, filepath.FromSlash(fx.published[0].RelPath))
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "reindexing", "reindexinG", 1)
	if edited == string(b) {
		t.Fatal("edit did not apply; fixture text changed?")
	}
	if err := os.WriteFile(full, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka"))
	for _, hit := range out.Hits {
		if hit.EpisodeID == fx.published[0].EpisodeID {
			t.Errorf("digest-stale episode served: %s", hit.EpisodeID)
		}
	}
	if out.EditedExcluded < 1 {
		t.Errorf("edited_excluded = %d, want >= 1", out.EditedExcluded)
	}
}

// TestSearchStillServesUneditedEpisodes: the fixture corpus returns its
// usual results, unchanged by the verification tightening.
func TestSearchStillServesUneditedEpisodes(t *testing.T) {
	fx := setupSearchCorpus(t)
	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %q, want match", out.Outcome)
	}
	if out.EditedExcluded != 0 {
		t.Errorf("edited_excluded = %d over an unedited corpus", out.EditedExcluded)
	}
	if len(out.Hits) == 0 {
		t.Fatal("no hits over the unedited fixture corpus")
	}
}

// TestGetReportsStaleRevisionOnBodyEdit: a digest-stale file — body edited,
// payload_digest line untouched — reports stale_revision with an empty
// revision and the reseal detail. There is no honest current revision for a
// file that does not verify.
func TestGetReportsStaleRevisionOnBodyEdit(t *testing.T) {
	fx := setupSearchCorpus(t)
	pub := fx.published[0]

	full := filepath.Join(fx.rootPath, filepath.FromSlash(pub.RelPath))
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "reindexing", "reindexinG", 1)
	if edited == string(b) {
		t.Fatal("edit did not apply")
	}
	if err := os.WriteFile(full, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	out := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: pub.EpisodeID,
		Revision:  DigestPrefix + pub.DigestHex,
	})
	if out.Outcome != OutcomeStaleRevision {
		t.Fatalf("outcome = %q, want stale_revision", out.Outcome)
	}
	if out.Revision != "" {
		t.Errorf("revision = %q, want empty for a file that does not verify", out.Revision)
	}
	if !strings.Contains(out.Detail, "reseal") {
		t.Errorf("detail = %q, want the reseal pointer", out.Detail)
	}
}

// TestGetReportsReplacementRevisionOnAbsorbedEdit: a regenerated,
// self-consistent file — different content whose recorded digest line
// matches that content — reports stale_revision with the current verified
// revision as the replacement reference.
func TestGetReportsReplacementRevisionOnAbsorbedEdit(t *testing.T) {
	fx := setupSearchCorpus(t)
	pub := fx.published[0]

	// Regenerate the episode wholesale with different content: same
	// identity, new payload, digest line consistent with the new body.
	p := mustValidate(t, testPayloadJSON)
	p.TurnID = "turn-2001"
	p.UserContent = "the enclosure records were regenerated wholesale"
	p.AssistantResult = "Regenerated."
	newDigest := PayloadDigestHex(&p)
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     newDigest,
		CaptureTimeMs: searchTestNowMs,
	})
	full := filepath.Join(fx.rootPath, filepath.FromSlash(pub.RelPath))
	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatal(err)
	}

	out := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: pub.EpisodeID,
		Revision:  DigestPrefix + pub.DigestHex,
	})
	if out.Outcome != OutcomeStaleRevision {
		t.Fatalf("outcome = %q, want stale_revision", out.Outcome)
	}
	if out.Revision != DigestPrefix+newDigest {
		t.Errorf("revision = %q, want the current verified revision %q", out.Revision, DigestPrefix+newDigest)
	}
	if strings.Contains(out.Detail, "reseal") {
		t.Errorf("detail = %q; an absorbed edit does not need reseal", out.Detail)
	}
}

// TestDuplicateTermWeightsSurviveAliasExpansion: whether a repeated query
// word counts once per repetition must not depend on an unrelated
// thesaurus entry firing (aj-scorer.v3). The triple "quokka" against
// a single-line episode outweighs the two-term portal/gateway episode only
// while each repetition keeps its weight; the pre-v3 deduplicated union
// inverts the order.
func TestDuplicateTermWeightsSurviveAliasExpansion(t *testing.T) {
	_, root := testCorpus(t)
	base := mustValidate(t, testPayloadJSON)

	q := base
	q.TurnID = "turn-8101"
	q.UserContent = "the quokka pen was cleaned this morning"
	q.AssistantResult = "Cleaned."
	quokkaEp := mustPublish(t, root, q)

	p := base
	p.TurnID = "turn-8102"
	p.UserContent = "the portal gateway hub came online"
	p.AssistantResult = "Online."
	portalEp := mustPublish(t, root, p)

	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatal(err)
	}
	aliases := LoadAliasMapFromBytes([]byte(`{"portal": ["gateway"]}`))

	out := Search(root, idx, aliases, SearchRequest{
		Query: "quokka quokka quokka portal", World: "testworld",
		NowMs: 1785326400000,
	})
	if out.Outcome != OutcomeMatch || len(out.Hits) < 2 {
		t.Fatalf("outcome = %s with %d hits, want match over both episodes", out.Outcome, len(out.Hits))
	}
	if len(out.AliasTerms) != 1 || out.AliasTerms[0] != "gateway" {
		t.Fatalf("alias_terms = %v, want the active [gateway]", out.AliasTerms)
	}
	if out.Hits[0].EpisodeID != quokkaEp.EpisodeID {
		t.Errorf("top hit = %s, want the triple-weighted quokka episode %s (portal episode %s won: repetition lost its weight)",
			out.Hits[0].EpisodeID, quokkaEp.EpisodeID, portalEp.EpisodeID)
	}
}

// TestFoldedTermsReportedSeparately: a folded singular variant is a term
// the owner never typed, so it is reported in folded_terms, not silently
// searched and never listed as an alias.
func TestFoldedTermsReportedSeparately(t *testing.T) {
	fx := setupSearchCorpus(t)
	out := Search(fx.root, fx.idx, fx.emptyMap, SearchRequest{
		Query: "quokkas", World: "testworld", NowMs: 1785326400000,
	})
	if len(out.FoldedTerms) != 1 || out.FoldedTerms[0] != "quokka" {
		t.Fatalf("folded_terms = %v, want [quokka]", out.FoldedTerms)
	}
	if len(out.AliasTerms) != 0 {
		t.Errorf("alias_terms = %v, want none (folding is not aliasing)", out.AliasTerms)
	}
	if out.Outcome != OutcomeMatch {
		t.Errorf("outcome = %s, want match via the folded singular", out.Outcome)
	}
}

// TestDuplicateWeightFixtureRanking pins the ordered result of the
// repeated-term-with-active-alias case in testdata/ranking — the public
// witness of the term-weighting invariant, since the judged query set
// cannot ship with the repository.
func TestDuplicateWeightFixtureRanking(t *testing.T) {
	raw, err := os.ReadFile("../testdata/ranking/case-duplicate-weight.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Query     string            `json:"query"`
		World     string            `json:"world"`
		NowMs     uint64            `json:"now_ms"`
		Thesaurus json.RawMessage   `json:"thesaurus"`
		Payloads  []json.RawMessage `json:"payloads"`
		Expected  []struct {
			EpisodeID string `json:"episode_id"`
			Line      uint32 `json:"line"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Expected) == 0 {
		t.Fatal("fixture pins nothing")
	}

	_, root := testCorpus(t)
	for _, pb := range fixture.Payloads {
		rawPayload, err := ParsePayload(pb)
		if err != nil {
			t.Fatal(err)
		}
		p, err := validateAsCaptureHost(t, rawPayload)
		if err != nil {
			t.Fatal(err)
		}
		mustPublish(t, root, p)
	}
	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatal(err)
	}
	out := Search(root, idx, LoadAliasMapFromBytes(fixture.Thesaurus), SearchRequest{
		Query: fixture.Query, World: fixture.World, NowMs: fixture.NowMs,
	})
	var got []string
	for _, h := range out.Hits {
		got = append(got, fmt.Sprintf("%s:%d", h.EpisodeID, h.Line))
	}
	var want []string
	for _, e := range fixture.Expected {
		want = append(want, fmt.Sprintf("%s:%d", e.EpisodeID, e.Line))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ordered ranking = %v, want the pinned %v", got, want)
	}
}

// TestSearchReadsEachEpisodeOnce pins the one-read-per-episode contract:
// the crediting pass reads and verifies each credited episode once, and
// snippet rendering reuses that content instead of re-reading the file.
// The counting wrapper observes every corpus read a search performs.
func TestSearchReadsEachEpisodeOnce(t *testing.T) {
	fx := setupSearchCorpus(t)

	reads := map[string]int{}
	orig := readContained
	readContained = func(root *os.Root, relPath string) (string, error) {
		reads[relPath]++
		return orig(root, relPath)
	}
	defer func() { readContained = orig }()

	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %q (%s)", out.Outcome, out.Detail)
	}
	if len(out.Hits) == 0 {
		t.Fatal("no hits")
	}
	for _, hit := range out.Hits {
		if hit.Snippet == "" {
			t.Errorf("empty snippet for %s — rendering must reuse the held content", hit.EpisodeID)
		}
	}
	if len(reads) == 0 {
		t.Fatal("counting seam observed no reads")
	}
	for rel, n := range reads {
		if n != 1 {
			t.Errorf("episode %s read %d times, want exactly 1", rel, n)
		}
	}
}

// NowMs 0 is the documented live-clock spelling: the scorer must see the
// same resolved clock freshness uses, or every event time reads as future
// and the recency nudge silently collapses to pure rarity.
func TestSearchZeroNowMsUsesLiveClockForRecency(t *testing.T) {
	fx := setupSearchCorpus(t)
	live := fx.request("quokka")
	live.NowMs = 0
	liveOut := Search(fx.root, fx.idx, fx.emptyMap, live)
	if liveOut.Outcome != OutcomeMatch {
		t.Fatalf("live outcome = %s, want match", liveOut.Outcome)
	}
	rarity := fx.request("quokka")
	rarity.Knobs = DefaultKnobs
	rarity.Knobs.RecencyBoost = 0
	rarityOut := Search(fx.root, fx.idx, fx.emptyMap, rarity)
	if rarityOut.Outcome != OutcomeMatch {
		t.Fatalf("rarity outcome = %s, want match", rarityOut.Outcome)
	}
	// The corpus event times are in the past against any live clock, so
	// the default boost must lift the best score above the boost-free run.
	if !(liveOut.BestScore > rarityOut.BestScore) {
		t.Errorf("NowMs=0 best score %v is not above pure rarity %v — recency was dropped",
			liveOut.BestScore, rarityOut.BestScore)
	}
}
